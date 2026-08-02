package portfolio

import (
	"context"
	"errors"
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

// converter is the subset of *marketdata.Converter this handler needs: a
// single fx conversion, used to bring a market valuation denominated in a
// different currency than the position (see toAPI) into the position's own
// currency, plus Rate, used to convert a whole position's amounts into the
// space's base currency (see positionInBase) while resolving the underlying
// rate at most once per (currency, date) pair per request — each lot and
// each income operation is valued at the rate of its own date, so the same
// currency can need several different rates within one request (see
// rateKey). Local interface (mirroring
// journalStore/quoteStore) so tests can inject a fake in place of a real
// *marketdata.Converter to control exactly which currency pairs have a
// resolvable rate, including forcing marketdata.ErrNoRate —
// *marketdata.Converter satisfies this structurally, no conversion needed at
// the call site.
type converter interface {
	Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error)
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
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
	instruments *instrument.Store
	quotes      quoteStore
	conv        converter
	spaces      spaceStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments *instrument.Store, quotes quoteStore, conv converter, spaces spaceStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
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
// quote's own currency (q.Currency) — price and instrument currency always
// agree here.
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
// intermediate round.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote) (minor int64, currency string, ok bool) {
	switch instType {
	case instrument.TypeShare, instrument.TypeETF:
		return q.Price.Mul(quantity).Shift(centsPerUnit).Round(0).IntPart(), q.Currency, true
	case instrument.TypeBond:
		if faceValueMinor == nil || faceCurrency == nil {
			return 0, "", false
		}
		return decimal.NewFromInt(*faceValueMinor).Mul(q.Price).Shift(-centsPerUnit).Mul(quantity).Round(0).IntPart(), *faceCurrency, true
	default:
		// currency, crypto, metal, custom: no defined valuation model yet.
		return 0, "", false
	}
}

// hasUndatedLots reports whether any lot still held has no acquisition date
// (see Lot.AcquiredOn). It is published as a fact about the position rather
// than left to be inferred from in_base being null, because null has two
// causes and they are not the same news: a missing fx rate is a gap the
// backfill job closes on its own, an unrecorded purchase date never resolves.
// Only the position that HAS one can tell them apart, so it says which.
//
// It scans the same slice positionInBase walks and stops at the first hit —
// the question is whether any exists, not how many.
func hasUndatedLots(p *Position) bool {
	return slices.ContainsFunc(p.Lots, func(l Lot) bool { return l.AcquiredOn == nil })
}

// toAPI builds one position's API representation, including its market
// valuation and — when that valuation isn't already in the position's own
// currency — an fx conversion into it (conv.Convert is only ever called
// when that conversion is actually needed).
//
// err is non-nil only for a genuine conversion failure (a Store/DB error, a
// canceled context) — never for marketdata.ErrNoRate, which is an expected,
// handled outcome (see below), not a request failure.
func toAPI(ctx context.Context, conv converter, p *Position, inst instrument.Instrument, quotes map[uuid.UUID]marketdata.Quote) (apitypes.Position, error) {
	out := apitypes.Position{
		Instrument:       instrumentToAPI(inst),
		Quantity:         p.Quantity.String(),
		CostMinor:        p.CostMinor,
		Currency:         p.Currency,
		RealizedPnlMinor: p.RealizedPnLMinor,
		IncomeMinor:      p.IncomeMinor,
		FeesMinor:        p.FeesMinor,
		HasUndatedLots:   hasUndatedLots(p),
	}
	q, found := quotes[p.InstrumentID]
	if !found {
		return out, nil
	}
	minor, currency, ok := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q)
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
	// other rows) using today's fx rate (time.Now().UTC(): "today" is the
	// only sensible answer for "what is this holding worth right now", as
	// opposed to some historical rate). The original figure is preserved in
	// market_value_source_currency/_minor purely for transparency (e.g. a UI
	// tooltip) — it plays no further part in the computation below.
	if currency != p.Currency {
		converted, err := conv.Convert(ctx, minor, currency, p.Currency, time.Now().UTC())
		switch {
		case err == nil:
			out.MarketValueSourceCurrency = nullable.NewNullableWithValue(currency)
			out.MarketValueSourceMinor = nullable.NewNullableWithValue(minor)
			minor, currency = converted, p.Currency
		case errors.Is(err, marketdata.ErrNoRate):
			// No rate to convert with: fall back to publishing the raw,
			// unconverted figure (as before this change) rather than hiding
			// it. market_value_source_* stay null — nothing was converted.
			// unrealized_pnl_minor is left null below, same as any other
			// currency mismatch.
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
	// Both operands are now guaranteed to be in the same currency whenever
	// this fires — either they always agreed (share/etf), or a successful
	// conversion just brought market_value into p.Currency above — so this
	// is exact integer subtraction on minor units, never a mix of
	// currencies and never a rounding operation.
	if currency == p.Currency {
		out.UnrealizedPnlMinor = nullable.NewNullableWithValue(minor - p.CostMinor)
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

// rateKey identifies one memoized fx rate lookup: a currency AND the date its
// rate must come from. The date belongs in the key because this handler no
// longer converts everything at today's rate — each lot is valued at the rate
// of the day it was acquired and each income operation at the rate of the day
// it occurred (see positionInBase) — so a cache keyed by currency alone would
// serve the first date's rate for every later date, producing wrong numbers
// that look entirely plausible on screen. Copied from operation.rateKey,
// which keys the journal's per-operation conversions the same way and for the
// same reason.
//
// The date is held as its YYYY-MM-DD string rather than a time.Time so the
// key compares the calendar date itself, immune to two otherwise-equal
// time.Time values differing in monotonic clock reading or *time.Location
// pointer (which would merely cost extra lookups, but would do so invisibly).
//
// The target currency is not part of the key: one request converts everything
// into one base currency, and the cache never outlives the request.
type rateKey struct {
	currency string
	on       string
}

// rateLookup memoizes one (currency, date) pair's resolved fx rate — the rate
// itself, the date it actually came from, and the resolution error — so a
// request hits the fx rate store at most once per distinct pair. A position
// with many lots usually has few distinct purchase dates, and positions
// sharing a currency share every lookup; "today" is one pair for the whole
// request. Mirrors account's and operation's identically named type.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
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
func (h *Handler) rateFor(ctx context.Context, from, to string, on time.Time, cache map[rateKey]*rateLookup) *rateLookup {
	key := rateKey{currency: from, on: on.Format("2006-01-02")}
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
// failures (DB error, canceled context), which must fail the request instead.
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
	return total.Round(0).IntPart(), true, nil
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

// positionInBase expresses a position's cost, market value, unrealized P&L
// and income in baseCurrency. Every amount is valued at the fx rate that
// answers its own question, which is the whole point of this function:
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
//   - market_value_minor uses TODAY's rate. It is the one current figure here,
//     because "what is this holding worth" is a question about now.
//   - unrealized_pnl_minor is that valuation minus that basis, both already in
//     baseCurrency, so it is exact integer subtraction with no second
//     rounding. Base-currency profit therefore INCLUDES the currency's
//     revaluation, and can differ from the position-currency profit in size
//     and even in sign — the owner's decision (2026-07-29): the two are honest
//     answers to two different questions, and the interface is what explains
//     which is which.
//
// fees_minor and realized_pnl_minor are deliberately excluded (owner feedback
// — not carried into PositionInBase at all, see the API contract).
//
// Only a valuation denominated in the position's own currency may be
// converted with the position's own rate. cost and income always are. The
// market valuation is not: when no rate was available to bring it into the
// position's currency, toAPI publishes it raw, in market_value_currency (a
// bond's face_currency, say EUR, on a USD position). Multiplying that EUR
// figure by the USD->RUB rate and labeling the product "RUB" would be a
// silently wrong number — and, since it equals the converted cost whenever
// cost and valuation coincide, would read as "profit exactly zero". So
// market_value_minor is published in_base only when market_value_currency
// equals p.Currency, and unrealized_pnl_minor follows it. Null there is
// honest: the frontend falls back to showing the raw amount in its own
// currency with a "not converted" marker.
//
// It returns (nil, nil) — render in_base as null, the WHOLE object, never
// partially populated — when p.Currency already equals baseCurrency (nothing
// to convert), when any single rate the object needs is missing
// (marketdata.ErrNoRate): today's, one lot's, or one income operation's, or
// when a single lot does not know WHEN it was acquired (Lot.AcquiredOn nil),
// which leaves no date to ask for a rate in the first place. A basis summed
// from only the lots that happened to convert is an invented number, smaller
// than the truth and indistinguishable from a real one on screen; it would
// drag the P&L along with it. The two causes are one rule — a term that cannot
// be valued voids the whole sum — and differ only in whether the date is
// missing or the rate for it is. This differs from
// market_value_minor/unrealized_pnl_minor inside the returned object, which
// are null when there is no usable quote or the valuation isn't in
// p.Currency — that nulls just those two figures.
//
// A non-nil error means a genuine failure (DB error, canceled context) that
// the caller must surface as a request error — never silently rendered as
// null, which would misrepresent an outage as "nothing to convert".
func (h *Handler) positionInBase(ctx context.Context, p *Position, apiPos apitypes.Position, income []Operation, baseCurrency string, cache map[rateKey]*rateLookup) (*apitypes.PositionInBase, error) {
	if p.Currency == baseCurrency {
		return nil, nil
	}

	// Today's rate values the market valuation and supplies rate_on, so
	// without it there is no object to publish at all.
	today := h.rateFor(ctx, p.Currency, baseCurrency, time.Now().UTC(), cache)
	if today.err != nil {
		if errors.Is(today.err, marketdata.ErrNoRate) {
			return nil, nil
		}
		return nil, today.err
	}

	lots := make([]datedMinor, 0, len(p.Lots))
	for _, l := range p.Lots {
		if l.AcquiredOn == nil {
			// This lot does not know when it was acquired (see
			// portfolio.Lot.AcquiredOn): it arrived by a transfer whose
			// purchase dates were never recorded. Its basis is real money, but
			// there is no date to value it at, and every candidate date — the
			// transfer's, another lot's, today's — would be a number this
			// handler made up.
			//
			// So the whole object goes, exactly as it does when one lot's date
			// has no fx rate: a basis summed from only the lots that could be
			// converted is smaller than the truth, looks like an ordinary
			// figure on screen, and drags the profit down with it. Nothing is
			// published rather than something wrong, and the position still
			// shows every figure it has in its own currency.
			return nil, nil
		}
		lots = append(lots, datedMinor{minor: l.CostMinor, on: *l.AcquiredOn})
	}
	costMinor, ok, err := h.sumInBase(ctx, lots, p.Currency, baseCurrency, cache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	payments := make([]datedMinor, 0, len(income))
	for _, o := range income {
		payments = append(payments, datedMinor{minor: o.AmountMinor, on: o.OccurredOn})
	}
	incomeMinor, ok, err := h.sumInBase(ctx, payments, p.Currency, baseCurrency, cache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	out := &apitypes.PositionInBase{
		CostMinor:   costMinor,
		IncomeMinor: incomeMinor,
		Currency:    baseCurrency,
		// rate_on names the rate behind market_value_minor only: the basis and
		// the income are each valued at several rates of their own dates, so no
		// single date could describe them. Today's rate is the one date that is
		// both unambiguous and worth disclosing — it is how fresh the "what is
		// it worth now" figure is, and per Store.FxRateOn it can be older than
		// today whenever the rate table is stale. The historical nature of the
		// basis is explained in the interface instead (see the API contract).
		RateOn: today.date.Format("2006-01-02"),
	}

	marketValueMinor := nullableValue(apiPos.MarketValueMinor)
	if c := nullableValue(apiPos.MarketValueCurrency); c == nil || *c != p.Currency {
		marketValueMinor = nil
	}
	if marketValueMinor == nil {
		out.MarketValueMinor = nullable.NewNullNullable[int64]()
		out.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		return out, nil
	}
	valuation := decimal.NewFromInt(*marketValueMinor).Mul(today.rate).Round(0).IntPart()
	out.MarketValueMinor = nullable.NewNullableWithValue(valuation)
	out.UnrealizedPnlMinor = nullable.NewNullableWithValue(valuation - costMinor)
	return out, nil
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
	// One batched round trip for every position's quote, never one per
	// position (N+1) — see quoteStore's doc comment.
	quotes, err := h.quotes.LatestQuotes(r.Context(), instrumentIDs)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	// Both scoped to this request only: see positionInBase/rateKey and
	// incomeByInstrument. The journal is already in hand, so grouping its
	// income entries costs one pass and no extra round trip.
	rates := make(map[rateKey]*rateLookup)
	income := incomeByInstrument(ops)
	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, err := h.instruments.ByID(r.Context(), pos.InstrumentID)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		apiPos, err := toAPI(r.Context(), h.conv, pos, inst, quotes)
		if err != nil {
			family.WriteError(w, err)
			return
		}

		inBase, err := h.positionInBase(r.Context(), pos, apiPos, income[pos.InstrumentID], sp.BaseCurrency, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if inBase != nil {
			apiPos.InBase = nullable.NewNullableWithValue(*inBase)
		} else {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
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
	})
}
