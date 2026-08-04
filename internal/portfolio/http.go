package portfolio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

// journalStore is the subset of operation.Store this handler needs. It is a
// local interface rather than a direct dependency on package operation
// because package operation imports this package (portfolio.Operation and
// portfolio.Compute back the journal-consistency checks in
// operation.Service) — importing operation.Store here would create an
// import cycle. operation.Operation is a type alias for portfolio.Operation
// (see operation/operation.go), so *operation.Store satisfies this
// interface structurally with no conversion needed at the call site.
type journalStore interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error)
}

// quoteStore is the subset of marketdata.Store this handler needs. Unlike
// journalStore, there's no import cycle forcing this into a local interface
// (package marketdata does not import portfolio) — it's local anyway so
// tests can inject a fake in place of a real Postgres-backed Store, both to
// control which instruments have quotes and to assert LatestQuotes is
// called once per request (a single batched round trip), never once per
// position.
type quoteStore interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// instrumentStore is the subset of *instrument.Store this handler needs: the
// catalog rows behind a whole list of positions, read in one round trip rather
// than one per position (see instrument.Store.ByIDs). Local interface for the
// same reasons quoteStore is one — a fake can count the round trips, and can
// produce the missing-row case a foreign key otherwise makes unreachable — and
// *instrument.Store satisfies it structurally, so nothing changes at the call
// site.
type instrumentStore interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// converter is the subset of *marketdata.Converter this handler needs, in the
// two shapes it needs it.
//
// RatesOn resolves every rate the whole screen is about to want in a single
// round trip, and its answers are filed in the request's memo before the
// per-position loop starts (see prewarmRates). Rate resolves one pair on one
// date, and stays because the memo has to be able to answer anything the
// prefetch did not think to ask for: each lot and each income operation is
// valued at the rate of its own date, and a market valuation at the rate of
// today, so one request wants many rates and the enumeration that predicts
// them is an optimization, never a precondition (see rateQueries and rateKey).
//
// Convert is deliberately absent although *marketdata.Converter has it: every
// conversion here now resolves its rate through the memo and applies it
// itself (see rateLookup.applyTo), which is Convert's own arithmetic, so
// calling it would be the one lookup on the page that could not be shared.
//
// Local interface (mirroring journalStore/quoteStore) so tests can inject a
// fake in place of a real *marketdata.Converter to control exactly which
// currency pairs have a resolvable rate, including forcing
// marketdata.ErrNoRate — *marketdata.Converter satisfies this structurally, no
// conversion needed at the call site.
type converter interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// spaceStore is the subset of family.Store this handler needs: reading the
// space's base currency to convert positions into it (see positionInBase).
// Local interface (mirroring journalStore/quoteStore, and identical to
// account's spaceStore) so tests can inject a fake or a real *family.Store
// interchangeably — Go interface assignability is structural, so
// *family.Store satisfies this with no conversion needed at the call site.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Handler exposes computed account positions over HTTP.
type Handler struct {
	ops         journalStore
	instruments instrumentStore
	quotes      quoteStore
	conv        converter
	spaces      spaceStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments instrumentStore, quotes quoteStore, conv converter, spaces spaceStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{ops: ops, instruments: instruments, quotes: quotes, conv: conv, spaces: spaces, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	srv.Mount("GET /api/v1/accounts/{accountId}/positions", view(h.handleList))
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// instrumentToAPI mirrors instrument.toAPI (unexported in package
// instrument): each module owns its own domain-to-API mapping rather than
// sharing one across packages, matching account.toAPI/operation.toAPI.
func instrumentToAPI(i instrument.Instrument) apitypes.Instrument {
	out := apitypes.Instrument{
		Id:       i.ID,
		Type:     apitypes.InstrumentType(i.Type),
		Name:     i.Name,
		Ticker:   i.Ticker,
		Isin:     i.ISIN,
		Figi:     i.FIGI,
		Currency: i.Currency,
		Frozen:   i.Frozen,
	}
	if i.FaceValueMinor != nil {
		out.FaceValueMinor = nullable.NewNullableWithValue(*i.FaceValueMinor)
	}
	if i.FaceCurrency != nil {
		out.FaceCurrency = nullable.NewNullableWithValue(*i.FaceCurrency)
	}
	return out
}

// centsPerUnit shifts a major-unit decimal amount into minor units — the
// convention (2 decimal digits) used everywhere else in this codebase for
// amountMinor, e.g. marketdata.Converter (see its doc comment). Quote.Price
// and instrument-catalog prices are always expressed in major units of
// their currency.
const centsPerUnit = 2

// marketValue computes a position's market value from the latest quote for
// its instrument, in minor units, plus the currency that value is
// denominated in. ok is false when no valuation applies — the instrument's
// type isn't priced this way (bond without both a face value and a face
// currency, or currency/crypto/metal/custom) — in which case the caller
// must leave market_value_minor/market_value_currency/price/price_on null
// rather than publish a zero or misleading figure.
//
// share/etf: price (per unit, major currency units) × quantity, in the
// quote's own currency (q.Currency). The QUOTE's, deliberately, because
// nothing makes it the instrument's: q.Currency is whatever the provider
// reported (MOEX sends one per row, in CURRENCYID), the instrument's comes
// from the catalog someone filled in, and the position's own — the currency
// this valuation is eventually compared against — comes from the journal,
// being the currency of the position's first operation (see Compute in
// engine.go). Three sources, nothing reconciling them.
//
// This comment used to say the price's currency and the instrument's "always
// agree here". They are only EXPECTED to, and the code around this function is
// already written for the case where they do not: toAPI converts a valuation
// that did not arrive in the position's currency, discloses the figure it
// converted from in market_value_source_currency/_minor, and publishes
// market_value_gap = no_rate_valuation_currency when there is no rate to
// convert with. A claim of "always" over a branch that handles "sometimes not"
// invites the next reader to delete the branch.
//
// bond: price is a percentage of face value (e.g. 95.20 meaning 95.20%), so
// the value is faceValueMinor × price/100 × quantity — already in minor
// units since faceValueMinor is. Crucially, that value is denominated in the
// face value's currency (faceCurrency, e.g. RUB for an OFZ), NOT the quote's
// currency: the quote's "currency" field for a bond is really just the unit
// the percentage price is quoted in, not a currency the resulting money is
// ever in. A bond with a face value but no face currency has no way to
// label that number, so it gets no valuation at all rather than a
// currency-less (and therefore meaningless) figure.
//
// Both branches multiply as decimals throughout and round exactly once at
// the end, half-away-from-zero (decimal.Decimal.Round's native behavior, the
// same rounding marketdata.Converter.Convert uses) — never float, never an
// intermediate round. That last step is money.Minor, which also refuses a
// product too large to be an int64 of minor units rather than letting it wrap
// (#27): a price and a quantity that each look ordinary can multiply past the
// edge, and the wrapped answer is a small figure of arbitrary sign.
//
// A QUANTITY AND A PRICE ARE NOW BOUNDED WHERE THEY ARE WRITTEN
// (operation.maxQuantity, #84) AND THIS GUARD STAYS REGARDLESS, because a figure
// that fitted when it was written can stop fitting afterwards. The price
// multiplied in here is the QUOTE's, which no validation of ours reaches; the
// bond branch below multiplies by a face value, which is the catalog's; a
// position is the sum of many operations and can pass the per-operation bound
// one accepted buy at a time; and the rows written before that bound existed are
// still in the journal, since no migration came with it.
//
// err is that refusal and NOTHING ELSE, which is why it is separate from ok.
// ok=false says no valuation applies to this instrument, and the caller
// answers it by publishing nulls — the same nulls it publishes for a position
// with no quote at all. An overflow answered that way would be a broken
// journal wearing the face of absent market data.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote) (minor int64, currency string, ok bool, err error) {
	switch instType {
	case instrument.TypeShare, instrument.TypeETF:
		minor, err = money.Minor(q.Price.Mul(quantity).Shift(centsPerUnit))
		if err != nil {
			return 0, "", false, fmt.Errorf("%w: %s at %s", err, quantity, q.Price)
		}
		return minor, q.Currency, true, nil
	case instrument.TypeBond:
		if faceValueMinor == nil || faceCurrency == nil {
			return 0, "", false, nil
		}
		minor, err = money.Minor(decimal.NewFromInt(*faceValueMinor).Mul(q.Price).Shift(-centsPerUnit).Mul(quantity))
		if err != nil {
			return 0, "", false, fmt.Errorf("%w: %s at %s%% of a face value of %d", err, quantity, q.Price, *faceValueMinor)
		}
		return minor, *faceCurrency, true, nil
	default:
		// currency, crypto, metal, custom: no defined valuation model yet.
		return 0, "", false, nil
	}
}

// anyUndatedLot is the ONE statement of "this basis contains a piece with no
// purchase date" — published as Position.has_undated_lots (via hasUndatedLots
// below) rather than left to be inferred from in_base being null, because null
// has two causes and they are not the same news: a missing fx rate is a gap
// the backfill job closes on its own, an unrecorded purchase date never
// resolves. Only the position that HAS one can tell them apart, so it says
// which.
//
// Two published facts rest on this one predicate — has_undated_lots and the
// in_base_gap value `undated_lot`, which lotTerms decides — and they are
// claims about the same lots in the same response: a reader who saw
// has_undated_lots false beside in_base_gap: undated_lot would have no way to
// tell which of the two was lying. They were separate predicates until the gap
// existed, when only the first was published and the second was merely a null
// object; naming the cause is what makes a disagreement legible, so the second
// answer is derived from the first rather than written out again.
//
// It scans the same slice positionInBase walks and stops at the first hit —
// the question is whether any exists, not how many.
func anyUndatedLot(lots []Lot) bool {
	return slices.ContainsFunc(lots, func(l Lot) bool { return l.AcquiredOn == nil })
}

// hasUndatedLots is anyUndatedLot published as Position.has_undated_lots (see
// anyUndatedLot for why it exists and what rests on it).
func hasUndatedLots(p *Position) bool {
	return anyUndatedLot(p.Lots)
}

// anyUndatedRealization is the ONE statement of "this position's disposals
// retired a piece of basis with no acquisition date" (see
// ReleasedLot.AcquiredOn) — published as Position.has_undated_realizations (via
// hasUndatedRealizations below), anyUndatedLot's twin for the realized side,
// for the identical reason: in_base.realized_pnl_minor being null has two
// causes — no fx rate for one of its dates, or no purchase date for a parcel
// it retired — and they are not the same news to a reader.
//
// anyUndatedLot cannot stand in for it: that predicate scans p.Lots, the lots
// still HELD, and a piece that stopped realized_pnl_minor has by definition
// already been sold — it is never among them. A position can therefore show
// has_undated_lots=false and has_undated_realizations=true in the very same
// response (see TestPositionInBaseRealizedNullWhenAReleasedParcelHasNoAcquisitionDate),
// which is exactly the case a single flag could not describe.
//
// Two published facts rest on this one predicate, exactly as with
// anyUndatedLot — has_undated_realizations above, and the RealizedTotal
// (account-level) in_base_gap value `undated`, which realizedTerms decides on
// the way to gapUndated (see realizedInBase) and which realizedTotals.add/
// result then carries onto the wire. A reader who saw has_undated_realizations
// false on a row while the account's total answered `undated` would have no
// way to tell which one was lying; deriving the second from the first is what
// makes such a disagreement impossible rather than merely unlikely.
//
// It scans every Realization rather than stopping at the first one with any
// Released piece, but the question is still only whether any undated piece
// exists anywhere, not how many or in which disposal.
func anyUndatedRealization(events []Realization) bool {
	for _, e := range events {
		if slices.ContainsFunc(e.Released, func(r ReleasedLot) bool { return r.AcquiredOn == nil }) {
			return true
		}
	}
	return false
}

// hasUndatedRealizations is anyUndatedRealization published as
// Position.has_undated_realizations (see anyUndatedRealization for why it
// exists and what rests on it).
func hasUndatedRealizations(p *Position) bool {
	return anyUndatedRealization(p.Realizations)
}

// toAPI builds one position's API representation, including its market
// valuation and — when that valuation isn't already in the position's own
// currency — an fx conversion into it (a rate is only ever asked for when that
// conversion is actually needed).
//
// now is the request's one reading of "today", passed in rather than taken
// here, so this conversion, the base-currency one in positionInBase and the
// prefetch that resolves both name the same calendar day. A request that
// straddled UTC midnight would otherwise ask for two different days under one
// word and quietly miss the memo.
//
// err is non-nil only for a genuine conversion failure (a Store/DB error, a
// canceled context) — never for marketdata.ErrNoRate, which is an expected,
// handled outcome (see below), not a request failure.
func (h *Handler) toAPI(ctx context.Context, p *Position, inst instrument.Instrument, quotes map[uuid.UUID]marketdata.Quote, now time.Time, cache map[rateKey]*rateLookup) (apitypes.Position, error) {
	out := apitypes.Position{
		Instrument:             instrumentToAPI(inst),
		Quantity:               p.Quantity.String(),
		CostMinor:              p.CostMinor,
		Currency:               p.Currency,
		RealizedPnlMinor:       p.RealizedPnLMinor,
		IncomeMinor:            p.IncomeMinor,
		FeesMinor:              p.FeesMinor,
		HasUndatedLots:         hasUndatedLots(p),
		HasUndatedRealizations: hasUndatedRealizations(p),
		// Nothing has been withheld from the valuation until the conversion
		// below actually fails, and every path out of this function leaves the
		// field as it is set here — an explicit null, never an unspecified key
		// (the contract requires it on every position, and an unspecified
		// nullable does not marshal as one).
		MarketValueGap: nullable.NewNullNullable[apitypes.MarketValueGap](),
	}
	q, found := quotes[p.InstrumentID]
	if !found {
		return out, nil
	}
	minor, currency, ok, err := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q)
	if err != nil {
		// The valuation does not fit in an int64 — a broken quantity or price,
		// not absent data — so it is surfaced as a request error rather than
		// published as the same null a position without a quote gets.
		return apitypes.Position{}, err
	}
	if !ok {
		return out, nil
	}
	out.Price = nullable.NewNullableWithValue(q.Price.String())
	out.PriceOn = nullable.NewNullableWithValue(q.On.Format("2006-01-02"))

	// A raw market valuation can be denominated in a currency other than the
	// position's own (for a bond, market_value_currency is the face value's
	// currency — see marketValue's doc comment — which can legitimately
	// differ from the position's trading/settlement currency, e.g. RUB for
	// an OFZ with a USD face value). Left as-is, the cost and market-value
	// columns would sit in two different currencies in the same row, and
	// subtracting one from the other for unrealized_pnl_minor would silently
	// mix them into a meaningless number.
	//
	// Rather than leave that valuation unusable, convert it into the
	// position's own currency (never the space's base currency — the goal is
	// comparability with cost_minor, which is always in p.Currency, not with
	// other rows) using today's fx rate ("today" is the only sensible answer
	// for "what is this holding worth right now", as opposed to some
	// historical rate). The original figure is preserved in
	// market_value_source_currency/_minor — for transparency (a UI tooltip
	// names it: «Пересчитано из 1 000,00 €») and because it, not the converted
	// figure, is what positionInBase carries on into the base currency. Nothing
	// below in THIS function reads it back.
	//
	// The rate comes from the request's memo (rateFor) and the arithmetic is
	// Convert's own (applyTo), which together produce exactly what
	// conv.Convert produced when this called it directly — Convert resolves
	// the rate the same way and multiplies it the same way, and from != to is
	// guaranteed here, so its identity short-circuit never applied anyway.
	// What changes is that this lookup is now shareable: it is the one pair on
	// the page whose target is the position's currency instead of the base
	// one, and going through the memo is what lets the prefetch cover it and
	// what lets a page of bonds in the same currency pay for it once.
	if currency != p.Currency {
		rl := h.rateFor(ctx, currency, p.Currency, now, cache)
		switch err := rl.err; {
		case err == nil:
			converted, convErr := rl.applyTo(minor)
			if convErr != nil {
				// The rate is there; the product is not an int64. The
				// missing-rate branch below answers ITS problem by publishing
				// the raw figure in its own currency, and that answer is not
				// available here: it would put an unconverted valuation on
				// screen wearing exactly the marks of a missing rate, which is
				// the one thing this refusal must not resemble.
				return apitypes.Position{}, convErr
			}
			out.MarketValueSourceCurrency = nullable.NewNullableWithValue(currency)
			out.MarketValueSourceMinor = nullable.NewNullableWithValue(minor)
			minor, currency = converted, p.Currency
		case errors.Is(err, marketdata.ErrNoRate):
			// No rate to convert with: fall back to publishing the raw,
			// unconverted figure (as before this change) rather than hiding
			// it. market_value_source_* stay null — nothing was converted.
			// unrealized_pnl_minor is left null below, same as any other
			// currency mismatch.
			//
			// THIS IS THE POINT WHERE THE VALUATION'S OWN GAP IS DECIDED, so it
			// is where the gap is named. It is not a second reading of the
			// outcome: positionInBase withholds the base-currency valuation by
			// asking this very field (see its guard), so the flag a client
			// captions the cell with and the figure the client does not get are
			// one decision. Comparing market_value_currency with p.Currency
			// afterwards would answer the same question today — this branch is
			// the only way the two can differ — and would be a second answer
			// waiting to disagree with this one.
			out.MarketValueGap = nullable.NewNullableWithValue(apitypes.NoRateValuationCurrency)
		default:
			// A genuine failure (DB error, canceled context) — not "no rate
			// available" — must not be silently swallowed into "no
			// conversion happened", which would misrepresent an outage as a
			// normal missing-rate case. Propagate it like any other
			// request-time error (see handleList).
			return apitypes.Position{}, err
		}
	}

	out.MarketValueMinor = nullable.NewNullableWithValue(minor)
	out.MarketValueCurrency = nullable.NewNullableWithValue(currency)
	// Both operands are in the same currency whenever this fires, and it is the
	// guard that makes that true rather than any property of the data: either
	// the valuation arrived in p.Currency already (the ordinary share/etf row,
	// whose quote happens to be denominated in the currency its operations are
	// — expected, never guaranteed, see marketValue), or the conversion above
	// brought it there. Where neither held, the profit is left null instead of
	// struck across two currencies. So this is exact integer subtraction on
	// minor units, never a mix of currencies and never a rounding operation.
	//
	// It is a GUARDED subtraction all the same (#83). Both operands fit in an
	// int64 and a difference of two such figures need not, once their signs
	// differ, and there is more than one way for the signs to differ.
	//
	// The valuation goes negative when a bond's face value does: a bond is valued
	// at face value × price, and face_value_minor is a plain BIGINT that nothing
	// checks for sign on the way in (#93). THE BASIS GOES NEGATIVE WITH NO
	// BROKEN FIELD ANYWHERE, which is why this guard does not retire when #93 is
	// closed: Position.CostMinor is accumulated with a bare += in the engine (see
	// addLot), each buy adding at most its amount plus its fee — 2×10^15 minor
	// units, the largest the write side admits of either — so about 4612 buys of
	// the same instrument, every one of them individually valid and none of them
	// carrying a broken field, take the running total past math.MaxInt64 and wrap
	// it into a large negative basis. Position's
	// realized total accumulates the same way (see realize). Neither is this
	// package's to fix in passing: the engine is a pure fold and changing what it
	// answers is a change to every figure derived from it.
	//
	// The wrapped answer would be an enormous PROFIT on a row that is merely
	// broken, which is exactly the kind of figure this screen must not invent.
	// Refused rather than published, and never as one of the nulls beside it:
	// those mean data has yet to arrive (see money.ErrOverflow).
	if currency == p.Currency {
		unrealized, err := money.Sub(minor, p.CostMinor)
		if err != nil {
			return apitypes.Position{}, fmt.Errorf("%w: a valuation of %d less a basis of %d", err, minor, p.CostMinor)
		}
		out.UnrealizedPnlMinor = nullable.NewNullableWithValue(unrealized)
	}
	return out, nil
}

// nullableValue extracts the value from a nullable.Nullable[T] API field,
// treating "unspecified" and "explicit null" the same way (nil). toAPI
// leaves market_value_minor/market_value_currency/unrealized_pnl_minor
// unspecified — rather than explicitly null — whenever there's no usable
// quote or valuation (see toAPI), so this reads that "no value" state
// regardless of which of the two wire representations produced it.
func nullableValue[T any](n nullable.Nullable[T]) *T {
	v, err := n.Get()
	if err != nil {
		return nil
	}
	return &v
}

// rateKey identifies one memoized fx rate lookup: the pair being converted AND
// the date its rate must come from. The date belongs in the key because this
// handler no longer converts everything at today's rate — each lot is valued at
// the rate of the day it was acquired and each income operation at the rate of
// the day it occurred (see positionInBase) — so a cache keyed by currency alone
// would serve the first date's rate for every later date, producing wrong
// numbers that look entirely plausible on screen. Copied from
// operation.rateKey, which keys the journal's per-operation conversions the
// same way and for the same reason.
//
// The date is held as its YYYY-MM-DD string rather than a time.Time so the
// key compares the calendar date itself, immune to two otherwise-equal
// time.Time values differing in monotonic clock reading or *time.Location
// pointer (which would merely cost extra lookups, but would do so invisibly).
//
// The TARGET currency is part of the key because this screen has two of them.
// Almost everything here converts into the space's base currency, but a bond
// whose valuation is denominated in its face currency is brought into the
// POSITION's currency instead — that is the whole point of that conversion,
// comparability with cost_minor (see toAPI) — so one request can legitimately
// ask for the SAME source currency under two different targets: a bond with a
// USD face value held in a EUR position needs USD->EUR today, while a plain
// USD position on the same screen needs USD->RUB today — both "USD, today"
// (see TestPositionsSharedRateMemoKeepsTargetsApart, which is exactly this
// pair of positions). A key naming only the source would collide the two,
// filing one position's rate where the other looks for its own: the same
// silently-plausible wrong number the date is in the key to prevent.
// operation.rateKey has one target and says so; this one cannot.
type rateKey struct {
	from string
	to   string
	on   string
}

// newRateKey is the only place a lookup becomes a key. Both halves of the memo
// build one — the loop asking for a rate (rateFor) and the prefetch filing the
// answers before it (prewarmRates) — and a key spelled two ways would file
// every prefetched answer where nothing looks for it: no wrong number, just a
// batch paid for and then ignored, which is precisely the kind of failure that
// leaves no trace.
func newRateKey(from, to string, on time.Time) rateKey {
	return rateKey{from: from, to: to, on: on.Format("2006-01-02")}
}

// rateLookup memoizes one rateKey's resolved fx rate — the rate itself, the
// date it actually came from, and the resolution error — so a request hits the
// fx rate store at most once per distinct (pair, date). A position with many
// lots usually has few distinct purchase dates, and positions sharing a
// currency share every lookup; "today" is one entry for the whole request.
// Mirrors account's and operation's identically named type.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// applyTo converts amountMinor at this rate — multiply as decimals, round the
// product once, half-away-from-zero — which is exactly what
// marketdata.Converter.Convert does with the rate it resolves.
//
// Both figures this file strikes from a single rate go through it: a bond's
// valuation brought into the position's own currency (toAPI, which used to
// call Convert and now shares the memo like everything else) and that
// valuation carried on into the base currency (positionInBase). One statement
// of how a rate becomes money, so the two cannot round differently from each
// other. It is deliberately the same step marketdata.Converter.Convert applies
// — but that is two statements of one rule, not one, and only tests hold them
// together (Convert has no production caller left in this package). If
// Convert's scale handling ever changes, for zero-decimal currencies or
// anything else, this has to be changed with it. Amounts summed from many
// rates do not come through
// here — they must round once for the whole sum, not once per term (see
// sumInBase).
//
// A product that does not fit in an int64 of minor units is refused rather
// than wrapped (money.ErrOverflow, #27). Callers surface that as a request
// error: it is a figure this server cannot state, not a figure it has yet to
// learn, and every null on this screen means the latter.
func (rl *rateLookup) applyTo(amountMinor int64) (int64, error) {
	minor, err := money.Minor(decimal.NewFromInt(amountMinor).Mul(rl.rate))
	if err != nil {
		return 0, fmt.Errorf("%w: %d at a rate of %s", err, amountMinor, rl.rate)
	}
	return minor, nil
}

// datedMinor is one amount denominated in the position's own currency,
// together with the date whose fx rate values it: a lot's remaining cost with
// the day that lot was acquired, an income operation's amount with the day it
// occurred.
type datedMinor struct {
	minor int64
	on    time.Time
}

// rateFor resolves from->to on date on, memoized in cache for the rest of the
// request. The returned rateLookup carries the resolution error rather than
// returning it, because callers must tell marketdata.ErrNoRate (an expected
// outcome that nulls in_base) apart from a genuine failure (which fails the
// request) — see positionInBase.
//
// The cache is normally already full when this is called: handleList prefetches
// every rate the screen was expected to want in one round trip (see
// prewarmRates). A MISS IS NOT AN ERROR — it is the whole safety net. Whatever
// the enumeration failed to predict, or the batch failed to fetch, is resolved
// here one pair at a time exactly as it was before any of that existed, so the
// figures on the screen never depend on the prefetch being complete or even on
// its having succeeded. Only their cost does.
func (h *Handler) rateFor(ctx context.Context, from, to string, on time.Time, cache map[rateKey]*rateLookup) *rateLookup {
	key := newRateKey(from, to, on)
	rl, ok := cache[key]
	if !ok {
		rate, date, err := h.conv.Rate(ctx, from, to, on)
		rl = &rateLookup{rate: rate, date: date, err: err}
		cache[key] = rl
	}
	return rl
}

// sumInBase converts every amount at the fx rate of its OWN date and returns
// the total in currency to. Every amount is multiplied as a decimal and only
// the total is rounded, once, half-away-from-zero — the same final step
// marketdata.Converter.Convert applies to a single amount. Rounding each term
// instead could drift from the true total by a minor unit per term, and the
// total is the figure actually published.
//
// ok is false when at least one date has no rate at all
// (marketdata.ErrNoRate): the caller must then publish nothing rather than a
// total quietly missing one of its terms. err is reserved for genuine
// failures (DB error, canceled context, or a total too large to be an int64 of
// minor units), which must fail the request instead.
//
// The overflow guard sits on the TOTAL, because the total is the published
// figure: every term can be an ordinary amount and their sum still leave the
// range. It is an error and not ok=false for the reason that distinction
// exists at all — ok=false is answered with a null the screen reads as data
// that has yet to arrive, and a sum too large to state is not waiting for
// anything.
func (h *Handler) sumInBase(ctx context.Context, amounts []datedMinor, from, to string, cache map[rateKey]*rateLookup) (minor int64, ok bool, err error) {
	total := decimal.Zero
	for _, a := range amounts {
		rl := h.rateFor(ctx, from, to, a.on, cache)
		if rl.err != nil {
			if errors.Is(rl.err, marketdata.ErrNoRate) {
				return 0, false, nil
			}
			return 0, false, rl.err
		}
		total = total.Add(decimal.NewFromInt(a.minor).Mul(rl.rate))
	}
	minor, err = money.Minor(total)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %d terms totalling %s %s", err, len(amounts), total, to)
	}
	return minor, true, nil
}

// lotTerms flattens the lots a position still holds into the terms of ONE sum
// — its basis — each term carrying the date whose fx rate values it: the day
// THAT lot was acquired, never today's (see positionInBase for why).
//
// dated is false as soon as one lot does not know when it was acquired (see
// portfolio.Lot.AcquiredOn): it arrived by a transfer whose purchase dates
// were never recorded. Its basis is real money, but there is no date to value
// it at, and every candidate date — the transfer's, another lot's, today's —
// would be a number this handler made up. What the caller does about that is
// the caller's decision (positionInBase publishes nothing at all); this
// function only refuses to invent the date.
//
// It is realizedTerms' twin for the held side, and like realizedTerms it is
// called from two places: by the code that converts these terms and by the
// enumeration that prefetches their rates (see rateQueries). That is the point
// of it being a function at all — one statement of which dates the basis
// needs, so the prefetch cannot come to disagree with the sum it is
// prefetching for.
// It asks anyUndatedLot first and walks the lots a second time to build the
// terms, rather than bailing out mid-loop as it used to. The second walk costs
// a nil check per lot and no allocation; what it buys is that the answer
// published as `undated_lot` and the answer published as has_undated_lots are
// one predicate rather than two that must agree (see anyUndatedLot). The
// dereference below is safe for exactly that reason, and a broken predicate
// would panic here rather than quietly value a lot at a date it does not have.
func lotTerms(lots []Lot) (terms []datedMinor, dated bool) {
	if anyUndatedLot(lots) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(lots))
	for _, l := range lots {
		terms = append(terms, datedMinor{minor: l.CostMinor, on: *l.AcquiredOn})
	}
	return terms, true
}

// incomeTerms flattens a position's income operations into the terms of ONE
// sum, each at the rate of the day it occurred (see incomeByInstrument for
// which operations those are, and positionInBase for why not one rate for the
// total).
//
// There is no undated case: an operation always has the day it occurred on.
// Like lotTerms and realizedTerms, it is called both by the sum and by the
// enumeration that prefetches the sum's rates.
func incomeTerms(income []Operation) []datedMinor {
	terms := make([]datedMinor, 0, len(income))
	for _, o := range income {
		terms = append(terms, datedMinor{minor: o.AmountMinor, on: o.OccurredOn})
	}
	return terms
}

// realizedTerms flattens every disposal a position has made into the terms of
// ONE sum — the position's realized result — each term carrying the date whose
// fx rate values it.
//
// A disposal contributes three kinds of term, and the dates are the whole
// point: the proceeds and the fee happened on the day of the disposal, while
// each parcel of basis it retired was paid for on the day THAT parcel was
// bought (НК РФ ст. 210 п. 5). Converting the event's own net result at one
// rate — even the correct rate for its own day — would price the expense on a
// day it was not incurred and quietly cancel the currency's move between
// purchase and sale out of the answer, which is a real part of the result and
// not a rounding of it.
//
// The engine records amortizations as disposals alongside sales, and transfers
// as none (see Realization), so which events reach this function is settled
// there rather than re-decided here.
//
// dated is false whenever any retired parcel does not know when it was bought
// (see Lot.AcquiredOn): there is no date to ask the fx table about, and every
// candidate — the disposal's own day, another parcel's, today's — would be a
// number this handler made up. The caller publishes nothing for the realized
// figure then; it does NOT drop the terms it could value, since a result
// missing part of its expense reads as a larger profit, not as a gap.
//
// It is lotTerms' twin for the realized side, and like lotTerms it asks
// anyUndatedRealization first and walks the events a second time to build the
// terms, rather than bailing out mid-loop as it used to (see anyUndatedLot):
// the answer published as has_undated_realizations and the answer published
// as RealizedTotal's `undated` gap are one predicate rather than two that must
// agree. The dereference below is safe for exactly that reason.
func realizedTerms(events []Realization) (terms []datedMinor, dated bool) {
	if anyUndatedRealization(events) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(events)*2)
	for _, e := range events {
		terms = append(terms,
			datedMinor{minor: e.ProceedsMinor, on: e.OccurredOn},
			datedMinor{minor: -e.FeeMinor, on: e.OccurredOn},
		)
		for _, r := range e.Released {
			terms = append(terms, datedMinor{minor: -r.CostMinor, on: *r.AcquiredOn})
		}
	}
	return terms, true
}

// baseGap names WHY a base-currency figure could not be struck. The two kinds
// are not the same news to the person reading the screen — one closes on its
// own and the other never will — and this type exists so the answer travels
// from the code that knows it to the payload, instead of being reconstructed
// downstream from flags that only correlate with it.
type baseGap uint8

const (
	// gapNone: nothing was missing; there is a figure.
	gapNone baseGap = iota
	// gapNoRate: every date the sum needs is known, but the fx table has no
	// rate for at least one of them yet. The backfill closes that gap on its
	// own and the figure appears later.
	gapNoRate
	// gapUndated: a parcel of basis does not know when it was bought (see
	// Lot.AcquiredOn), so there is no date to ask the fx table about and none
	// will ever be recovered — nobody wrote it down.
	gapUndated
)

// inBaseGap names WHICH TERM stopped a position's whole in_base object, and is
// what the contract's Position.in_base_gap publishes.
//
// It is baseGap's neighbour and deliberately not baseGap itself. That type
// answers for the account's realized total, which sums MANY positions, so the
// most it can name is the KIND of gap — no single term exists to point at, and
// it needs a `both` value because two kinds really can be true of one total at
// once. Here there is exactly one position, its terms are the very figures a
// row shows side by side, and the kind alone is not enough to caption them
// with: «нет курса» is false about a lot whose DATE nobody recorded, and
// «нет курса на дату покупки» is false over a market valuation whose row failed
// on today's rate. So this vocabulary names terms, and it needs no `both` — see
// apiInBaseGap for why one value is enough.
type inBaseGap uint8

const (
	// inBaseStruck: nothing was missing; there is an object.
	inBaseStruck inBaseGap = iota
	// inBaseSameCurrency: the position is already denominated in the base
	// currency, so there is no object and nothing to explain either — its own
	// figures ARE the base-currency ones. Named separately from inBaseStruck
	// purely for readability at the call site (see positionInBase's early
	// return): nothing downstream tells the two apart — apiInBaseGap maps both
	// to "no cause published" and handleList separates "struck" from "nothing
	// to convert" by checking the *PositionInBase pointer, never this value.
	inBaseSameCurrency
	// inBaseUndatedLot: a lot still held does not know when it was acquired
	// (see Lot.AcquiredOn and lotTerms), so there is no date to ask the fx
	// table about. The one member that never resolves on its own.
	inBaseUndatedLot
	// inBaseNoRateLotDate: every lot is dated, but the fx table has no rate for
	// at least one of those days, nor for any earlier one.
	inBaseNoRateLotDate
	// inBaseNoRateIncomeDate: the same, for the day one of the position's
	// income operations occurred.
	inBaseNoRateIncomeDate
	// inBaseNoRateToday: no rate today for the pair the market valuation needs
	// — the currency THAT VALUATION is in against the base one. Only a position
	// with a valuation to convert ever asks for it.
	inBaseNoRateToday
)

// apiInBaseGap maps a gap onto the contract's vocabulary. ok is false for the
// two members that are not gaps at all (inBaseStruck, inBaseSameCurrency),
// which publish no cause: one has a figure and the other has nothing to
// convert, and the client tells those apart by comparing the position's
// currency with the base one.
//
// EXACTLY ONE CAUSE IS EVER PUBLISHED, and that is a decision rather than a
// limitation of the type. positionInBase stops at the first term it cannot
// value, and it looks at them in the order the constants above are declared —
// which puts inBaseUndatedLot, the only member that no backfill will ever
// close, ahead of the three that it will. A row that has both a dateless lot
// and a missing rate therefore reports the dateless lot, and never promises a
// converted figure that is not coming. Among the three closeable ones, whichever
// is reported is true, and closing it reports the next: the caption converges
// instead of ever claiming more than the server knows. baseGap needs `both` for
// the opposite reason — it aggregates positions, so a permanent gap in one and a
// closeable gap in another are simultaneously true of the single total it
// describes.
func apiInBaseGap(g inBaseGap) (apitypes.InBaseGap, bool) {
	switch g {
	case inBaseUndatedLot:
		return apitypes.UndatedLot, true
	case inBaseNoRateLotDate:
		return apitypes.NoRateLotDate, true
	case inBaseNoRateIncomeDate:
		return apitypes.NoRateIncomeDate, true
	case inBaseNoRateToday:
		return apitypes.NoRateToday, true
	default:
		return "", false
	}
}

// realizedInBase is the position's realized result in currency to, or an
// explicit null with the kind of gap that stopped it. It answers for both
// readers of that figure — the position's own in_base block and the account's
// realized total — so the two can never disagree.
//
// ROUNDING HAPPENS ONCE, HERE, FOR THE WHOLE POSITION. The published quantity
// is one number per position, so that number is what gets rounded: every term
// of every disposal is multiplied as a decimal and only the total is rounded,
// half-away-from-zero, exactly as cost_minor and income_minor already are (see
// sumInBase). Rounding each disposal's own result first is the tempting shape —
// it reads like "convert each deal" — and it drifts from the true total by up to
// half a minor unit per disposal, in a figure the owner may well reconcile
// against a broker's report by hand. Nothing per-disposal is published, so
// nothing is made to agree by rounding earlier; the day a per-disposal
// breakdown IS published, each of those figures becomes a published quantity in
// its own right, is rounded once itself, and the contract will have to say that
// their sum can differ from this total by a unit — which is a statement about
// two roundings of the same money, not a defect in either. That per-disposal
// total, not this one, will be the legally correct figure to report on such a
// line-item breakdown: a tax authority computes the base per trade and sums
// the trades, not the reverse (НК РФ ст. 210 п. 5 taxes each disposal on its
// own), so the sum-of-rounded-rows answer is the one the law asks for even
// though this field will keep publishing the single round-once-per-position
// total for the reasons above.
//
// Null (rather than an error) covers the two ways a term can fail to be valued:
// no fx rate for a date the sum needs, and no purchase date for a parcel it
// retired. Both leave the sum one term short, and a total quietly missing a term
// is an invented number that looks exactly like a real one on screen. Neither
// touches the rest of the object: cost, income and the valuation are answers to
// their own questions, struck from their own dates, and none of them depends on
// a parcel that has already been sold.
//
// A non-nil error is a genuine failure (DB error, canceled context) that the
// caller must surface as a request error — never rendered as the null above,
// which would tell the owner their sale is unconvertible when the truth is that
// this server is having a bad minute.
func (h *Handler) realizedInBase(ctx context.Context, p *Position, to string, cache map[rateKey]*rateLookup) (nullable.Nullable[int64], baseGap, error) {
	if p.Currency == to {
		// Nothing to convert: the position's own realized result already IS
		// the base-currency one. The published `in_base` object is null for
		// such a position, but the figure is not unknown, and the account's
		// total would silently lose a real term if this answered otherwise.
		return nullable.NewNullableWithValue(p.RealizedPnLMinor), gapNone, nil
	}
	terms, dated := realizedTerms(p.Realizations)
	if !dated {
		return nullable.NewNullNullable[int64](), gapUndated, nil
	}
	minor, ok, err := h.sumInBase(ctx, terms, p.Currency, to, cache)
	if err != nil {
		return nullable.Nullable[int64]{}, gapNone, err
	}
	if !ok {
		return nullable.NewNullNullable[int64](), gapNoRate, nil
	}
	return nullable.NewNullableWithValue(minor), gapNone, nil
}

// realizedTotals adds up what an account's closed deals have locked in, folding
// each position in as it is built.
//
// THE SERVER ADDS, THE CLIENT RENDERS. Nothing here converts and nothing here
// rounds — every term was converted and rounded once already, for its own
// position — so a client could in principle do this addition itself and get the
// same integers. It does not, for the same reason the accounts screen's totals
// are computed here and not there: a figure the interface shows is derived in
// one place, so the policy behind it (what rounds, which positions count, what
// a gap suppresses) can change in one place and every reader keeps agreeing on
// the answer. The single standing exception to that rule is the profit
// percentage, and it is an exception precisely because a percentage is not
// money.
//
// Two forms, because the screen has two modes and the server cannot know which
// one is on: by currency (each position in its own, never added across
// currencies) and in the space's base currency (one figure, or none at all).
type realizedTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	inBaseMinor  int64
	undated      bool
	noRate       bool
}

func newRealizedTotals(baseCurrency string) *realizedTotals {
	return &realizedTotals{baseCurrency: baseCurrency, byCurrency: make(map[string]int64)}
}

// add folds one position in: nativeMinor is its realized result in its own
// currency, which the contract publishes unconditionally, and baseMinor the
// same result in the base currency — null with the gap that stopped it.
//
// The base sum keeps accumulating even after a gap is seen: the terms cost
// nothing to add, and result() drops the partial sum rather than publishing it.
//
// BOTH ACCUMULATIONS ARE GUARDED, and int64 addition is what they are guarded
// against. Every term arriving here is an ordinary figure — it had to survive
// money.Minor to be an int64 at all, and realizedInBase rounded it once for its
// own position — and a total of ordinary terms can still leave the range, at
// which point Go's + wraps as silently as decimal.IntPart() does. The same
// argument sumInBase and marketdata.ConvertMany already stand on: the guard
// belongs on the figure that gets published, and here that figure is the sum
// (#83). Reachable through the base total in particular, where no fx rate is
// bounded from above.
//
// A REFUSAL IS AN ERROR AND NOT A GAP. `undated` and `no_rate` say a term could
// not be valued and the figure may yet appear; this total's terms are all
// present and one of them is broken, so it is handed to the caller, which fails
// the request (see handleList). Nothing is half-added on the way out: both
// totals are computed before either is stored, so a refused position leaves the
// accumulator exactly as it found it.
func (rt *realizedTotals) add(currency string, nativeMinor int64, baseMinor nullable.Nullable[int64], gap baseGap) error {
	native, err := money.Add(rt.byCurrency[currency], nativeMinor)
	if err != nil {
		return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
			err, currency, nativeMinor, rt.byCurrency[currency])
	}
	inBase := rt.inBaseMinor
	switch gap {
	case gapUndated:
		rt.undated = true
	case gapNoRate:
		rt.noRate = true
	default:
		// gapNone: the figure was struck, so there is one to add.
		if inBase, err = money.Add(rt.inBaseMinor, baseMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, rt.baseCurrency, baseMinor.MustGet(), rt.inBaseMinor)
		}
	}
	rt.byCurrency[currency], rt.inBaseMinor = native, inBase
	return nil
}

// result is the account's answer as the contract publishes it.
//
// Rounding: the per-position figures added here were each rounded once, for
// their own position, and this total is their exact integer sum. Re-deriving it
// from the raw disposal terms and rounding once for the whole account would be
// the more accurate single number — and would put the header a minor unit away
// from the very rows it stands over, in the same response. See
// RealizedTotal.in_base in the API contract.
func (rt *realizedTotals) result() apitypes.RealizedTotal {
	out := apitypes.RealizedTotal{
		BaseCurrency: rt.baseCurrency,
		ByCurrency:   make([]apitypes.RealizedCurrencyTotal, 0, len(rt.byCurrency)),
	}
	for currency, minor := range rt.byCurrency {
		out.ByCurrency = append(out.ByCurrency, apitypes.RealizedCurrencyTotal{
			Currency:         currency,
			RealizedPnlMinor: minor,
		})
	}
	sort.Slice(out.ByCurrency, func(i, j int) bool {
		return out.ByCurrency[i].Currency < out.ByCurrency[j].Currency
	})

	// A total missing one of its terms is an invented number — smaller or
	// larger than the truth and indistinguishable from a real one on screen —
	// so nothing at all is published in its place, and the reason is named
	// instead of left for the reader to guess from the rows.
	switch {
	case rt.undated && rt.noRate:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.Both)
	case rt.undated:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.Undated)
	case rt.noRate:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	default:
		out.InBase = nullable.NewNullableWithValue(rt.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	return out
}

// incomeByInstrument groups the journal's instrument-attributed income
// operations by instrument, so each position's income can be converted
// payment by payment at each payment's own rate — Position.IncomeMinor is a
// single total that has already lost the dates and could only ever be
// converted at one of them.
//
// The type list must stay in lockstep with the engine's own notion of income
// (Compute: dividend and coupon add to IncomeMinor, tax subtracts from it via
// its negative amount; entries without an instrument are cash-level and never
// reach a position). The sum of each group therefore equals that position's
// IncomeMinor exactly, and TestPositionInBaseIncomeUsesEachOperationsOwnRate
// exercises all three types, so a list that drifts from the engine's fails
// there instead of silently publishing a base income smaller than the income
// the same row reports in its own currency.
func incomeByInstrument(ops []Operation) map[uuid.UUID][]Operation {
	out := make(map[uuid.UUID][]Operation)
	for _, o := range ops {
		if o.InstrumentID == nil {
			continue
		}
		switch o.Type {
		case TypeDividend, TypeCoupon, TypeTax:
			out[*o.InstrumentID] = append(out[*o.InstrumentID], o)
		}
	}
	return out
}

// positionInBase expresses a position's cost, market value, unrealized P&L,
// income and realized P&L in baseCurrency. Every amount is valued at the fx
// rate that answers its own question, which is the whole point of this
// function:
//
//   - cost_minor sums the FIFO lots still held (p.Lots), each converted at the
//     rate of the day THAT lot was acquired. It is deliberately not
//     p.CostMinor times today's rate: that would price the basis as if the
//     whole position had been bought this morning — a question nobody asks —
//     and, by applying one rate to both sides of the subtraction below, would
//     cancel the currency's own move straight out of the profit, leaving
//     base-currency profit as nothing but position-currency profit times a
//     rate.
//   - income_minor sums the position's income operations, each at the rate of
//     the day it occurred (income, unlike the lots, is passed in: see
//     incomeByInstrument).
//   - market_value_minor uses TODAY's rate, from the currency the valuation is
//     REALLY denominated in — a bond's face currency when the row had to
//     convert it (market_value_source_currency), the position's own otherwise.
//     It is the one current figure here, because "what is this holding worth"
//     is a question about now.
//   - unrealized_pnl_minor is that valuation minus that basis, both already in
//     baseCurrency, so it is exact integer subtraction with no second
//     rounding. Base-currency profit therefore INCLUDES the currency's
//     revaluation, and can differ from the position-currency profit in size
//     and even in sign — the owner's decision (2026-07-29): the two are honest
//     answers to two different questions, and the interface is what explains
//     which is which.
//   - realized_pnl_minor sums the disposals already made (p.Realizations), each
//     one's proceeds and fee at the rate of ITS day and each parcel of basis it
//     retired at the rate of the day THAT parcel was bought. It is the one
//     figure here that is settled: both of its ends are past events with dates
//     of their own, so unlike the unrealized figure above it will never move
//     again. It is passed IN rather than computed here (see realizedInBase),
//     because the account's own total needs exactly the same figure whether or
//     not this object survives — a settled result does not become unknowable
//     because today's rate is missing — and computing it twice would let the
//     two answers drift apart.
//
// fees_minor is deliberately excluded (owner feedback — not carried into
// PositionInBase at all, see the API contract).
//
// EVERY FIGURE IS CONVERTED FROM THE CURRENCY IT IS ACTUALLY IN, ONCE. cost
// and income are in the position's own currency and always were. The market
// valuation may not be: a bond's is born in its face currency and toAPI brings
// it into the position's so the row can compare it with cost_minor (see
// toAPI). This object does NOT continue from that converted figure — it goes
// back to the original, market_value_source_minor in
// market_value_source_currency, and converts that straight into the base
// currency. One multiplication, one rounding. Chaining the two conversions
// rounded the money to a whole cent in a currency that appears nowhere in the
// answer, and then multiplied that lost fraction by the second rate: 1 000,00 €
// through the dollar came out 9 999 990 kopecks instead of 10 000 000, under a
// tooltip saying it had been converted from euros (#39). It had not been.
//
// market_value_minor is still published in_base ONLY when market_value_currency
// equals p.Currency, and unrealized_pnl_minor follows it. THAT RULE WITHHOLDS
// NOTHING THAT COULD BE COMPUTED, and the reason is checkable rather than a
// principle. Every fx row this program stores is quoted in RUB: the seed writes
// <currency>/RUB rows (cmd/babki/seed.go) and the only provider behind the
// refresh jobs is the CBR one, which publishes nothing else (marketdata/cbr).
// marketdata.resolveRate reaches a pair only as a direct row, as the inverse of
// one, or as a bridge X->RUB->Y. So for a valuation denominated in F on a
// position in P: this rule fires only after the F->P conversion in toAPI
// failed, which in a RUB-quoted table means F->RUB is missing or P->RUB is. A
// pair the table cannot resolve today it cannot resolve for any earlier day
// either (Store.FxRateOn takes the nearest EARLIER row), so a missing P->RUB
// has already taken cost_minor or income_minor down with it and this function
// returned nil far above, before the rule was reached at all. What is left is a
// missing F->RUB — and then F->baseCurrency has no route either, whatever the
// base currency is. The figure is absent because there is none, not because one
// is being kept back.
//
// The rule is nevertheless written out rather than left to that argument,
// because the argument is about the rate TABLE rather than about this function.
// The day one non-RUB-quoted row exists — a direct GBP/USD, say, which nothing
// in this codebase can currently create — a valuation stuck in GBP on a USD
// position becomes expressible in RUB, and withholding it turns into a real
// choice that would have to be argued for here instead of merely explained.
//
// The step "P->RUB missing has already nulled this object" holds only because
// no lot can be dated later than today: Service rejects an operation dated in
// the future (see its occurred_on check), and Store.FxRateOn resolves the
// nearest EARLIER row, so a table that answers any lot's date answers today's
// too. Contrapositive: no answer today means no answer on any lot date either,
// and cost_minor was unstrikable before this branch was reached.
//
// ONE row still slips through, and the written-out rule covers it. A position
// holding no lot and having received no income sums nothing, needs no rate to do
// it, and so survives a missing P->RUB — for it, F->RUB may well exist and the
// base-currency valuation is genuinely computable. What the rule withholds there
// is the zero of a closed row.
//
// Asymmetry between this object and the position's own figures is ordinary, not
// something this rule prevents: realized_pnl_minor goes null here while
// Position.realized_pnl_minor is published either way, and one undated lot nulls
// this whole object while every native figure stays. Null here is honest in all
// of those cases: the frontend falls back to showing the raw amount in its own
// currency with a "not converted" marker.
//
// It returns no object — render in_base as null, the WHOLE object, never
// partially populated — together with the inBaseGap naming the term that
// stopped it, when p.Currency already equals baseCurrency (nothing
// to convert, and so no gap either: that one is inBaseSameCurrency, and the
// position's own figures ARE the base-currency ones), when any single rate the
// object needs is missing
// (marketdata.ErrNoRate): one lot's, one income operation's, or today's — and
// today's is needed only when there is a valuation for it to convert, so a
// position with no usable quote publishes its basis and its income without
// ever asking for a rate for today (rate_on is then null, saying exactly
// that). Today's is now asked for the VALUATION's currency against the base
// one, not the position's, so on a bond priced off a foreign face value a
// missing rate there takes cost_minor and income_minor down with it as well —
// unreachable in a RUB-quoted table, by the same kind of argument as the rule
// above and spelled out at the today's-rate block below, and stated here
// because it is a fact about the whole object rather than about the valuation
// alone. It also returns
// no object when a single lot does not know WHEN it was acquired
// (Lot.AcquiredOn nil),
// which leaves no date to ask for a rate in the first place. A basis summed
// from only the lots that happened to convert is an invented number, smaller
// than the truth and indistinguishable from a real one on screen; it would
// drag the P&L along with it. The two causes are one rule — a term that cannot
// be valued voids the whole sum — and differ only in whether the date is
// missing or the rate for it is. This differs from
// market_value_minor/unrealized_pnl_minor inside the returned object, which
// are null when there is no usable quote or the valuation isn't in
// p.Currency, and from realized_pnl_minor, which is null when a disposal's own
// day has no rate or a parcel it retired has no purchase date — those nulls are
// confined to the figures that could not be struck.
//
// What decides between the two is whether the unvaluable term belongs to the
// object as a whole or to one figure in it. A lot still held sits inside
// cost_minor's sum, and cost_minor is what unrealized_pnl_minor is measured
// against, so its failure travels; a parcel sold last year sits inside nothing
// but the realized sum, and taking the rest of the position down with it would
// hide a basis and a valuation that are perfectly well known.
//
// A non-nil error means a genuine failure (DB error, canceled context) that
// the caller must surface as a request error — never silently rendered as
// null, which would misrepresent an outage as "nothing to convert". The gap
// returned beside such an error is inBaseStruck and means nothing: an outage is
// not one of the gaps this vocabulary describes, and handleList reads the error
// first (mirroring realizedInBase, which returns gapNone on the same path).
//
// EVERY RETURN NAMES BOTH AT ONCE, which is the point of the second result
// rather than a flag computed beside the call: the sentence a reader is shown
// and the figure they are not shown leave this function in one statement, so
// the caption cannot come to describe a different failure than the one that
// actually happened. handleList closes the loop from the other end — it
// publishes the object only when the gap says there was none (see there).
// now is the request's one reading of "today" (see toAPI, which takes it for
// the same reason): the valuation this object converts was itself brought into
// p.Currency at today's rate a moment earlier, and the two must mean the same
// day even for a request that crosses UTC midnight.
func (h *Handler) positionInBase(ctx context.Context, p *Position, apiPos apitypes.Position, income []Operation, baseCurrency string, realizedMinor nullable.Nullable[int64], now time.Time, cache map[rateKey]*rateLookup) (*apitypes.PositionInBase, inBaseGap, error) {
	if p.Currency == baseCurrency {
		return nil, inBaseSameCurrency, nil
	}

	lots, dated := lotTerms(p.Lots)
	if !dated {
		// One lot does not know when it was acquired, so its basis cannot be
		// valued at all (see lotTerms). The whole object goes, exactly as it
		// does when one lot's date has no fx rate: a basis summed from only the
		// lots that could be converted is smaller than the truth, looks like an
		// ordinary figure on screen, and drags the profit down with it. Nothing
		// is published rather than something wrong, and the position still
		// shows every figure it has in its own currency.
		//
		// This is checked before any rate is asked for, which is also what
		// settles the answer for a position that has this gap AND a missing
		// rate: the permanent cause is the one reported (see apiInBaseGap).
		return nil, inBaseUndatedLot, nil
	}
	costMinor, ok, err := h.sumInBase(ctx, lots, p.Currency, baseCurrency, cache)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateLotDate, nil
	}

	incomeMinor, ok, err := h.sumInBase(ctx, incomeTerms(income), p.Currency, baseCurrency, cache)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateIncomeDate, nil
	}

	// Unlike the two sums above, a realized result that cannot be struck nulls
	// only itself: its terms are disposals already made, and nothing else in
	// this object is computed from them (see realizedInBase).
	out := &apitypes.PositionInBase{
		CostMinor:        costMinor,
		IncomeMinor:      incomeMinor,
		RealizedPnlMinor: realizedMinor,
		Currency:         baseCurrency,
	}

	// Which figure this object converts, and out of which currency. The GUARD
	// comes first and the rule it enforces is unchanged: a valuation that never
	// reached the position's currency is not carried into the base one (see this
	// function's doc comment — in a table where every rate is quoted in RUB,
	// there is no base-currency figure to carry).
	//
	// WHAT IT ASKS changed with the gap. It used to compare
	// market_value_currency with p.Currency, which is toAPI's outcome read back
	// out of the payload; it now asks toAPI's own answer, market_value_gap, set
	// in the branch where the conversion into p.Currency failed. The two are
	// equivalent today — that branch is the only way the two currencies can
	// differ, and the `c == nil` half of the old condition was already
	// redundant, since toAPI sets market_value_minor and market_value_currency
	// together and a nil valuation is handled below regardless. What the change
	// buys is that the cause published to the reader and the figure withheld
	// from them are one decision instead of two that must keep agreeing: a
	// caption saying the valuation could not be converted, over a converted
	// valuation, is precisely the failure this whole change is about.
	marketValueMinor := nullableValue(apiPos.MarketValueMinor)
	valuationCurrency := p.Currency
	if nullableValue(apiPos.MarketValueGap) != nil {
		marketValueMinor = nil
	}
	// Only then is the converted figure swapped back for the original it was
	// converted FROM, so the base-currency answer is struck in one step.
	//
	// `marketValueMinor != nil` here is UNREACHABLE INSURANCE, not part of the
	// rule: toAPI sets the two source fields only in the branch where the
	// conversion succeeded, which is not the branch that sets the gap, so the
	// guard above never struck the figure this would put back. Removing the
	// conjunct leaves the whole suite green, and it is kept anyway — not as a
	// second, quieter statement of the rule (the guard states it), but so that a
	// future toAPI which set the source fields on a path that did NOT convert
	// could not resurrect, here, a valuation the guard has just withheld. What no
	// test can pin, a comment has to say.
	if src, srcCurrency := nullableValue(apiPos.MarketValueSourceMinor), nullableValue(apiPos.MarketValueSourceCurrency); marketValueMinor != nil && src != nil && srcCurrency != nil {
		marketValueMinor, valuationCurrency = src, *srcCurrency
	}
	if marketValueMinor == nil {
		// No valuation this object may convert — no usable quote, or one the
		// guard above withheld because it never reached the position's own
		// currency. Nothing is being kept back in that second case: the
		// conversion out of its own currency needs the very rate whose absence
		// stopped it from reaching p.Currency (see the doc comment).
		// rate_on goes with it, because rate_on is the DATE OF that figure and
		// of nothing else here: the basis and the income are struck at the
		// rates of their own many dates, and a date published beside them
		// would be naming the day of a value this object does not contain (the
		// one place in this payload where the label claimed slightly more than
		// the object held — see PositionInBase.rate_on in the contract).
		//
		// Today's rate is not asked for at all in this branch, and that is the
		// point rather than an optimization: it is required by exactly the
		// figures it values, so a position with no quote publishes the cost and
		// the income it does know instead of going null over a rate that would
		// have appeared nowhere in the answer. Before, the object was refused
		// whenever today's rate was missing, and stayed correct only by
		// coincidence — Store.FxRateOn resolves the nearest EARLIER date, so a
		// table holding any lot's rate holds one for today too, and the two
		// conditions happened to fire together on real data. Coincidence is not
		// a rule, and TestPositionInBasePublishedWithoutTodaysRateWhenThereIsNoQuote
		// pins the rule.
		out.MarketValueMinor = nullable.NewNullNullable[int64]()
		out.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.RateOn = nullable.NewNullNullable[string]()
		// The object stands, so nothing stopped IT — which is why this returns
		// no gap even though a figure inside it is null. The one case that has
		// something to explain is already published on the position itself, by
		// the code that decided it (Position.market_value_gap, set in toAPI);
		// the other, a position with no usable quote, has no valuation to
		// withhold and nothing to caption.
		return out, inBaseStruck, nil
	}

	// Today's rate values the market valuation and supplies rate_on — the one
	// date in this object that is both unambiguous and worth disclosing, since
	// it is how fresh the "what is it worth now" figure is, and per
	// Store.FxRateOn it can be older than today whenever the rate table is
	// stale. Without it the valuation cannot be struck, and since the profit
	// below is measured against a basis that would then be published beside a
	// valuation that is not, the whole object goes rather than half of it.
	//
	// The pair is the VALUATION's currency against the base one, which for a
	// bond priced off a foreign face value is neither the position's currency
	// nor the one any other figure in this object is converted from. That is
	// the whole point (see the doc comment): one screen can therefore ask
	// about EUR->RUB for a row whose every other figure is USD->RUB, and
	// rateQueries enumerates it from the same marketValue call that decides it.
	//
	// It also widens what the ErrNoRate below can take down. Before, only the
	// POSITION currency's today-rate could null this object; now a missing rate
	// for a bond's FACE currency nulls cost_minor and income_minor too, though
	// neither is converted through it. Nothing reaches that today: this line
	// runs only when the valuation did arrive in p.Currency, so toAPI resolved
	// F->p.Currency, so — every rate this program stores being quoted in RUB —
	// the table holds F->RUB; and cost_minor was struck through
	// p.Currency->baseCurrency at each lot's own date, so RUB->baseCurrency sits
	// there on a day no later than today, which by Store.FxRateOn's
	// nearest-earlier rule answers for today as well; F->baseCurrency is the two
	// of them bridged. (A position holding no lot strikes its basis without any
	// rate at all and escapes the second half of that — it has nothing but zeros
	// to lose here.) It is written down because what changed is the SHAPE of the
	// failure rather than anything reachable: the day a non-RUB-quoted row
	// exists, this refusal becomes reachable, and it refuses figures that have
	// nothing to do with the valuation.
	today := h.rateFor(ctx, valuationCurrency, baseCurrency, now, cache)
	if today.err != nil {
		if errors.Is(today.err, marketdata.ErrNoRate) {
			return nil, inBaseNoRateToday, nil
		}
		return nil, inBaseStruck, today.err
	}
	valuation, err := today.applyTo(*marketValueMinor)
	if err != nil {
		// Too large to state in the base currency. The missing rate handled
		// just above nulls the whole object and names a gap, because the figure
		// it stops is one the fx backfill will supply later; this one is not
		// waiting for anything, so it fails the request rather than joining
		// that null. The gap it returns beside the error is inBaseStruck — not
		// a cause, because nothing here is a gap the caller should publish: a
		// non-nil error means the request dies and no gap is ever read.
		return nil, inBaseStruck, err
	}
	out.MarketValueMinor = nullable.NewNullableWithValue(valuation)
	// Guarded for the same reason the position's own unrealized figure is (see
	// toAPI): two int64s of opposite sign can differ by more than an int64, and
	// a negative valuation needs nothing worse than a negative face value to
	// arrive. Both operands have been converted already, so this is the last
	// arithmetic between here and the wire.
	unrealized, err := money.Sub(valuation, costMinor)
	if err != nil {
		// Named, because money.Sub names no figure and a 500 whose log says only
		// "does not fit" tells whoever reads it nothing about which row to look at.
		return nil, inBaseStruck, fmt.Errorf("%w: a base valuation of %d less a basis of %d in %s",
			err, valuation, costMinor, baseCurrency)
	}
	out.UnrealizedPnlMinor = nullable.NewNullableWithValue(unrealized)
	// rate_on names the rate that was actually applied, and there is not always
	// one to name: a valuation already denominated in the base currency (an OFZ
	// with a ruble face value in a dollar account of a ruble space) is the
	// answer as it stands, and rateFor hands back a rate of 1 on the ZERO date
	// — marketdata resolves nothing for from == to, deliberately, so that an
	// identity conversion cannot disclose a staleness that does not exist. That
	// zero date formats as "0001-01-01", so the choice here is between an
	// explicit null and a caption naming a rate that had no part in the figure.
	// Null, then — for the same reason the branch above publishes one.
	if today.date.IsZero() {
		out.RateOn = nullable.NewNullNullable[string]()
	} else {
		out.RateOn = nullable.NewNullableWithValue(today.date.Format("2006-01-02"))
	}
	return out, inBaseStruck, nil
}

// rateQueries enumerates every fx rate the loop in handleList is about to ask
// for, so one RatesOn call can resolve them all and every rateFor below finds
// its answer already in the memo. A screen holding thirty positions bought on
// a hundred days between them costs one round trip for the lot, instead of one
// per distinct date per position (#40, #53).
//
// IT IS DERIVED FROM THE CODE THAT CONSUMES THE RATES WHEREVER THAT IS
// POSSIBLE, RATHER THAN WRITTEN BESIDE IT. Every historical date here comes
// out of lotTerms, incomeTerms or realizedTerms — the same three functions the
// sums themselves are built from — and the one pair whose target is not the
// base currency comes out of marketValue, the same function toAPI values the
// position with. That leaves no list of DATES to keep in step with a second
// list of dates, which is where this codebase has been bitten before: two
// computations of one value drift apart, and a prefetch is the worst place for
// it, since the two disagree in silence.
//
// ONE thing is not derived, and it is a DATE rather than a pair: the two
// valuation queries below are written `now` by hand. toAPI and positionInBase
// both value a holding at today's rate as a flat decision rather than by
// building terms, so there is nothing here to call for it and these two lines
// restate that choice. Their PAIRS are derived like everything else — both come
// out of the one marketValue call in the loop, so neither restates which
// currency a bond's valuation is really in. If the valuation ever stops being
// struck at today's rate, these two dates have to follow — and nothing will
// make them, because the consequence is a missed prefetch, not a wrong figure
// (see below).
//
// Completeness is an optimization, not a correctness condition, and the
// asymmetry is deliberate. Asking for a rate the loop turns out not to need
// (the valuation below whose conversion fails, say) costs one row in a query
// that was happening anyway. FAILING to ask for one costs nothing but a round
// trip either, because rateFor resolves whatever it does not find, exactly as
// it did before this existed. What would be dangerous is naming a DIFFERENT
// pair than the loop asks for and having its answer read as the loop's — and
// that cannot happen, because the memo is keyed by the pair and the day
// themselves (see rateKey), so a mis-enumerated rate is filed where nothing
// looks for it.
//
// The slice this builds names one query per TERM before it is returned —
// appendTermQueries asks once per lot, per income operation, per released
// parcel — so a position with many lots landing on a handful of purchase
// dates repeats the same (pair, day) many times over. That costs nothing at
// the database (RatesOn collapses duplicates before it ever queries the
// store), but it is not free: prewarmRates below walks this same slice again
// to file each answer in the memo, and RatesOn's own resolution walks it once
// more to build its result. Both of those passes are O(terms) unless this one
// hands them O(distinct queries) instead, so the return statement collapses
// the slice through dedupeQueries before handing it back — keyed with rateKey,
// the same identity the memo itself uses, rather than a second answer to what
// makes two queries "the same".
func rateQueries(
	positions map[uuid.UUID]*Position,
	instruments map[uuid.UUID]instrument.Instrument,
	quotes map[uuid.UUID]marketdata.Quote,
	income map[uuid.UUID][]Operation,
	baseCurrency string,
	now time.Time,
) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	for _, p := range positions {
		inst, known := instruments[p.InstrumentID]
		q, quoted := quotes[p.InstrumentID]
		if known && quoted {
			// marketValue's error is deliberately ignored here, as
			// operation.rateQueries ignores amountTerms': this pass only
			// predicts which rates to fetch, and the loop calls the same
			// function on the same position moments later and fails from the
			// place that knows which figure it was building (see toAPI). A
			// valuation that cannot be struck simply asks for no rate, which is
			// also all this function could usefully do about it.
			if _, currency, valued, _ := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q); valued {
				if currency != p.Currency {
					// toAPI brings a valuation denominated in the face
					// currency into the position's own — the one lookup on
					// this page whose target is not the base currency.
					out = append(out, marketdata.RateQuery{From: currency, To: p.Currency, On: now})
				}
				if p.Currency != baseCurrency {
					// positionInBase carries that valuation on into the base
					// currency FROM THE SAME `currency` toAPI converted it out
					// of, not from the position's — the valuation is converted
					// once, from where it really is (#39, see positionInBase).
					// So this is `currency` here too, and both queries come out
					// of the one marketValue call above rather than restating
					// which currency a bond's valuation is in.
					//
					// It asks for today's rate only when there is a valuation
					// to convert. Whether there still is one depends on the
					// conversion just above having succeeded, which is not
					// known until it runs — so this asks whenever a valuation
					// exists at all, and over-asks in exactly the case where
					// the position ends up publishing no in_base valuation
					// anyway. It also asks when `currency` IS the base
					// currency, which positionInBase resolves as an identity
					// without touching the store: a wasted row in a batch that
					// was happening anyway, and the alternative — a second
					// place that knows identities are free — is how an
					// enumeration comes to disagree with its consumer.
					out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
				}
			}
		}
		if p.Currency == baseCurrency {
			// Nothing else on this position converts: positionInBase and
			// realizedInBase both short-circuit on it.
			continue
		}
		if lots, dated := lotTerms(p.Lots); dated {
			out = appendTermQueries(out, lots, p.Currency, baseCurrency)
		}
		out = appendTermQueries(out, incomeTerms(income[p.InstrumentID]), p.Currency, baseCurrency)
		if realized, dated := realizedTerms(p.Realizations); dated {
			out = appendTermQueries(out, realized, p.Currency, baseCurrency)
		}
	}
	return dedupeQueries(out)
}

// appendTermQueries asks for the rate of every term's own date — which is what
// sumInBase will do with the same terms, one at a time.
func appendTermQueries(dst []marketdata.RateQuery, terms []datedMinor, from, to string) []marketdata.RateQuery {
	for _, t := range terms {
		dst = append(dst, marketdata.RateQuery{From: from, To: to, On: t.on})
	}
	return dst
}

// dedupeQueries collapses queries onto one entry per distinct (pair, day),
// keeping the first occurrence's own On value. It exists because
// appendTermQueries asks once per TERM rather than once per distinct date:
// 500 lots settling on 5 purchase dates produce 500 queries for 5 answers,
// and every one of the three passes rateQueries and its callers make over
// this slice — this function's own accumulation aside — pays for every
// repeat, not just the distinct ones the store ends up billed for.
//
// Filters in place (out := queries[:0]) rather than allocating a second
// slice: the read index is always at or ahead of the write index, so
// overwriting queries as it is walked never clobbers an element still to be
// read.
//
// Keyed with rateKey/newRateKey — the same identity the request's rate memo
// itself uses (see rateFor) — rather than a second, independent notion of
// "same query" that could disagree with it. That also sidesteps the usual
// time.Time hazard for free: two dates naming the same calendar day but
// differing in *time.Location or monotonic reading collapse here exactly as
// they already collapse in the memo, because newRateKey reduces both to the
// same YYYY-MM-DD string.
func dedupeQueries(queries []marketdata.RateQuery) []marketdata.RateQuery {
	seen := make(map[rateKey]bool, len(queries))
	out := queries[:0]
	for _, q := range queries {
		k := newRateKey(q.From, q.To, q.On)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, q)
	}
	return out
}

// prewarmRates resolves queries in one round trip and files each answer in the
// request's memo under the key rateFor will look it up by.
//
// NOTHING HERE FAILS THE REQUEST, and that is not laziness. This call buys
// speed, not truth: every figure below is struck by rateFor, which resolves
// whatever it does not find in the memo. A batch that fails leaves the memo
// empty and the screen is computed exactly as it was before any prefetch
// existed. An outage is met again by the very next lookup and reported from the
// code that knows which figure it was resolving and can tell a missing rate (a
// gap on the screen) from an outage (a 500). Failing here would move that
// judgement to a place that cannot make it, and would turn into an error page
// every request the fallback could have served correctly.
//
// WHICH ANSWERS GET FILED IS NOT DECIDED HERE. Rates.Answered walks the
// queries and hands back only the ones the batch resolved, so a query it never
// answered leaves no entry and rateFor resolves it itself, while a query
// answered with "no rate" arrives carrying marketdata.ErrNoRate and is filed as
// the honest gap it is. That rule is one statement for all three screens that
// warm a memo this way (see marketdata.Rates.Answered); only the key an answer
// is filed under is this package's own.
//
// The error is dropped here and reported one layer down. A failure specific to
// the BATCH statement — a timeout on the one large query, say — leaves the
// screen correct and slow, since rateFor resolves every figure per pair, so
// there is nothing for this handler to tell the user, and an error page would be
// a worse outcome than a slow one. But there IS something to tell whoever runs
// this: the optimization has stopped working and no request will ever say so.
// That warning is written where the batch actually dies, which is the only place
// all four survivors of such a failure pass through
// (marketdata.Converter.fetchRates, #70).
func (h *Handler) prewarmRates(ctx context.Context, queries []marketdata.RateQuery, cache map[rateKey]*rateLookup) {
	if len(queries) == 0 {
		return
	}
	resolved, err := h.conv.RatesOn(ctx, queries)
	if err != nil {
		return
	}
	for q, res := range resolved.Answered(queries) {
		cache[newRateKey(q.From, q.To, q.On)] = &rateLookup{rate: res.Rate, date: res.RateDate, err: res.Err}
	}
}

// handleList computes an account's positions by replaying its full
// operations journal through Compute. Closed positions (quantity zero) are
// included: realized P&L and income on them remain meaningful history.
// Results are sorted by instrument name for a stable, human-friendly order.
//
// Known MVP behavior: like GET .../operations, the account is looked up by
// (space_id, account_id) only, with no separate existence/ownership check.
// A wrong or nonexistent accountId therefore yields an empty journal and an
// empty position list — the same 200 response as an account with no
// activity — rather than a 404. This is single-tenant software, so no data
// ever crosses a space boundary; the caller just can't distinguish "no
// positions" from "not your account".
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}

	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	ops, err := h.ops.ListForEngine(r.Context(), p.SpaceID, accountID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	positions, err := Compute(ops)
	if err != nil {
		// Practically unreachable: every write to the journal already passes
		// through this same engine (operation.Service.Create/Delete replay
		// the journal to check consistency before committing), so a stored
		// journal that fails to replay here would mean the data is already
		// corrupted rather than a normal request-time error.
		httpjson.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	instrumentIDs := make([]uuid.UUID, 0, len(positions))
	for id := range positions {
		instrumentIDs = append(instrumentIDs, id)
	}
	// One batched round trip for every position's catalog row and one for
	// every position's quote, never one per position (N+1) — see
	// instrumentStore and quoteStore.
	instruments, err := h.instruments.ByIDs(r.Context(), instrumentIDs)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	quotes, err := h.quotes.LatestQuotes(r.Context(), instrumentIDs)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	// One reading of "today" for the whole request: the market valuations, the
	// base-currency figures struck from them and the prefetch that resolves
	// both must name the same calendar day, even for the one request a year
	// that starts before UTC midnight and finishes after it.
	now := time.Now().UTC()

	// Both scoped to this request only: see positionInBase/rateKey and
	// incomeByInstrument. The journal is already in hand, so grouping its
	// income entries costs one pass and no extra round trip.
	rates := make(map[rateKey]*rateLookup)
	income := incomeByInstrument(ops)

	// One round trip for every rate the loop below is about to want. It is a
	// cache warm-up and nothing more: what it misses, and everything it
	// resolves if it fails outright, the loop resolves for itself (see
	// rateQueries, prewarmRates and rateFor).
	h.prewarmRates(r.Context(), rateQueries(positions, instruments, quotes, income, sp.BaseCurrency, now), rates)

	totals := newRealizedTotals(sp.BaseCurrency)
	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, ok := instruments[pos.InstrumentID]
		if !ok {
			// The journal names an instrument the catalog has no row for. A
			// foreign key (operations.instrument_id, ON DELETE RESTRICT) makes
			// this unreachable, and it is answered anyway rather than skipped:
			// a skipped position is a holding missing from the portfolio with
			// every total quietly smaller and nothing on screen saying so. The
			// same 404 the one-at-a-time read published through
			// family.WriteError(pgx.ErrNoRows) before it was batched.
			httpjson.Error(w, http.StatusNotFound, "not found")
			return
		}
		apiPos, err := h.toAPI(r.Context(), pos, inst, quotes, now, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}

		// Struck once and used twice: it is this position's
		// in_base.realized_pnl_minor below and one term of the account's total
		// beside it. The total takes it even when the in_base object turns out
		// to be absent — a settled result is not made unknowable by a missing
		// quote or by a lot still held whose purchase date nobody wrote down.
		realizedMinor, realizedGap, err := h.realizedInBase(r.Context(), pos, sp.BaseCurrency, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if err := totals.add(apiPos.Currency, apiPos.RealizedPnlMinor, realizedMinor, realizedGap); err != nil {
			family.WriteError(w, err)
			return
		}

		inBase, gap, err := h.positionInBase(r.Context(), pos, apiPos, income[pos.InstrumentID], sp.BaseCurrency, realizedMinor, now, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		// THE GAP DECIDES WHETHER THERE IS AN OBJECT TO PUBLISH, not a second
		// look at the pointer. positionInBase names both in one statement, and
		// this reads the naming: a cause is published only with no object beside
		// it, and an object only with no cause. Should the two ever be returned
		// together by mistake, this drops the object and keeps the honest
		// caption — the wrong way round would put a converted figure under a
		// sentence saying it could not be converted, which is the one outcome
		// worse than today's vague-but-true phrase.
		if apiGap, missing := apiInBaseGap(gap); missing {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			apiPos.InBaseGap = nullable.NewNullableWithValue(apiGap)
		} else {
			apiPos.InBaseGap = nullable.NewNullNullable[apitypes.InBaseGap]()
			// No gap covers two answers — the object was struck, or the position
			// is already in the base currency and there was nothing to convert
			// — and the pointer is what tells them apart. Both publish a null
			// gap: nothing is missing in either, and a client separates them by
			// comparing the position's currency with the base one.
			if inBase != nil {
				apiPos.InBase = nullable.NewNullableWithValue(*inBase)
			} else {
				apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			}
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	// Every figure above was computed FIFO within this one account, which is
	// the only rule this application implements. Whether that is the rule the
	// owner's country actually applies is a separate fact, and it ships with
	// the numbers rather than only in the session: this response is where a
	// reader takes cost_minor, realized_pnl_minor and unrealized_pnl_minor
	// from, so it is where the statement of what they are — and are not — has
	// to be available. It describes the computation, not any one row, so it is
	// attached once to the response and is present even when the account holds
	// nothing at all.
	httpjson.Write(w, http.StatusOK, apitypes.PositionsResponse{
		Positions:      out,
		CostBasisRules: family.CostBasisRulesAPI(sp.CostBasisRules()),
		// Added here rather than by whoever renders the list: see
		// realizedTotals, and RealizedTotal in the API contract.
		RealizedTotal: totals.result(),
	})
}
