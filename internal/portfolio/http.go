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

// valuationGap names WHY a position has no market valuation, or why the one it
// has could not be brought into the position's own currency. It is what the
// contract's Position.market_value_gap publishes (see apiMarketValueGap), and
// it exists for the reason inBaseGap does: the answer travels from the code
// that decides it to the payload, instead of being reconstructed downstream
// from an absent figure.
//
// THE THREE CAUSES OF AN ABSENT VALUATION ARE NOT THE SAME NEWS, which is the
// whole of #78. Before this vocabulary the screen said «Нет котировки» over
// every empty valuation cell, and two of the three rows it said that over have
// a quote sitting right there: one whose type this program computes nothing
// for, and a bond whose face value nobody recorded. Both sent the reader off to
// wait for data that was not missing.
type valuationGap uint8

const (
	// valuationStruck: there is a valuation. It is also what marketValue
	// returns beside a refusal (see its err), where it means nothing: the
	// caller reads the error first and never publishes a gap on that path —
	// exactly as positionInBase returns inBaseStruck beside its own errors.
	valuationStruck valuationGap = iota
	// valuationTypeNotPriced: this program has no valuation model for the
	// instrument's type. Reported whether or not a quote exists, because a
	// quote closes nothing here.
	valuationTypeNotPriced
	// valuationNoFaceValue: a bond with no face value recorded. Its quote is a
	// percentage of face value, so there is nothing to take the percentage of,
	// and a quote closes nothing here either.
	valuationNoFaceValue
	// valuationNoQuote: the type is priced, the catalog row is complete, and no
	// price has been stored yet. The only one of the three an arriving quote
	// closes — which is why the two above are decided ahead of it.
	valuationNoQuote
	// valuationNoRateValuationCurrency: there IS a valuation, and the fx table
	// could not convert it out of the currency it is denominated in into the
	// position's own. Decided in toAPI rather than in marketValue, which knows
	// nothing about rates.
	valuationNoRateValuationCurrency
)

// apiMarketValueGap maps a gap onto the contract's vocabulary. ok is false for
// valuationStruck, which is not a gap at all and publishes no cause.
func apiMarketValueGap(g valuationGap) (apitypes.MarketValueGap, bool) {
	switch g {
	case valuationTypeNotPriced:
		return apitypes.TypeNotPriced, true
	case valuationNoFaceValue:
		return apitypes.NoFaceValue, true
	case valuationNoQuote:
		return apitypes.NoQuote, true
	case valuationNoRateValuationCurrency:
		return apitypes.NoRateValuationCurrency, true
	default:
		return "", false
	}
}

// marketValue computes a position's market value from the latest quote for
// its instrument, in minor units, plus the currency that value is
// denominated in. gap is valuationStruck when there is a figure, and otherwise
// names which of the three absences applies, in which case the caller must
// leave market_value_minor/market_value_currency/price/price_on null rather
// than publish a zero or misleading figure — and publish the gap, so that the
// dash on screen carries the reason it is there.
//
// quoted says whether q is a real quote rather than a zero value, and this
// function takes it rather than being called only when it is true. That is what
// puts the two permanent causes AHEAD of the missing quote: the switch below
// decides the type before it looks at the quote at all, and the bond branch
// checks its face value first. A crypto row with no quote therefore answers
// `type_not_priced`, not `no_quote` — because a quote arriving for it would
// still value nothing, and «no quote» would name the one thing whose arrival
// changes something. It is the same ordering rule apiInBaseGap states for
// in_base_gap: the cause no backfill closes is reported ahead of the ones it
// does.
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
// err is that refusal and NOTHING ELSE, which is why it is separate from gap.
// A gap says no valuation exists, and the caller answers it by publishing
// nulls plus the reason. An overflow answered that way would be a broken
// journal wearing the face of absent market data.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote, quoted bool) (minor int64, currency string, gap valuationGap, err error) {
	switch instType {
	case instrument.TypeShare, instrument.TypeETF:
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		minor, err = money.Minor(q.Price.Mul(quantity).Shift(centsPerUnit))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s", err, quantity, q.Price)
		}
		return minor, q.Currency, valuationStruck, nil
	case instrument.TypeBond:
		// Before the quote, deliberately: a bond's price is a percentage of
		// face value, so a quote arriving for a bond with no face value
		// recorded would still value nothing, and «no quote» would be an
		// answer the reader could act on where there is none.
		if faceValueMinor == nil || faceCurrency == nil {
			return 0, "", valuationNoFaceValue, nil
		}
		if !quoted {
			return 0, "", valuationNoQuote, nil
		}
		minor, err = money.Minor(decimal.NewFromInt(*faceValueMinor).Mul(q.Price).Shift(-centsPerUnit).Mul(quantity))
		if err != nil {
			return 0, "", valuationStruck, fmt.Errorf("%w: %s at %s%% of a face value of %d", err, quantity, q.Price, *faceValueMinor)
		}
		return minor, *faceCurrency, valuationStruck, nil
	default:
		// currency, crypto, metal, custom — and any type added to
		// instrument.Type without a branch above. This program computes no
		// value for them. The quote is not consulted, and that is the point:
		// such an instrument can carry a perfectly good quote (the seed's
		// sub-cent share is deliberately a SHARE for exactly this reason), and
		// the row still has no valuation. Nothing is coming — no decision to
		// write such a model has been taken — so this gap must never be
		// captioned as one that will close.
		return 0, "", valuationTypeNotPriced, nil
	}
}

// pricePerUnitMinor is what ONE bond costs in money at the quoted price, and is
// published beside the quote itself (Position.price_money_minor).
//
// IT IS FOR BONDS ALONE, because a bond is the only paper here whose quote is
// not money: it is a percentage of face value, and a reader comparing it with
// the cost and the valuation on the same row — both money — had to multiply by
// the face value in their head. For a share the quote already IS money per unit
// and this would restate it, so the caller does not ask.
//
// STRUCK ON ITS OWN, NOT DIVIDED OUT OF THE VALUATION. The two are the same
// product in a different order (face x price/100, times the quantity or not),
// and dividing would round a second time on a figure that had already been
// rounded once — the published value is the one that gets rounded, and this is
// a published value. It is deliberately NOT computed on the client either: the
// client does no money arithmetic, which is the rule that keeps every rounding
// decision in one place.
//
// The overflow refusal is marketValue's argument narrowed: this product has no
// quantity in it, so it fits wherever the valuation does — but the guard costs
// nothing and the alternative to it is a wrapped number that looks like a price.
func pricePerUnitMinor(faceValueMinor int64, price decimal.Decimal) (int64, error) {
	return money.Minor(decimal.NewFromInt(faceValueMinor).Mul(price).Shift(-centsPerUnit))
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

// incomeByCurrencyToAPI publishes a position's income exactly as the engine
// kept it: one entry per currency the payments arrived in, in the engine's own
// order, each figure in its own currency's minor units.
//
// IT CONVERTS NOTHING, SUMS NOTHING AND REORDERS NOTHING, and each of the three
// would be this function answering a question the engine deliberately left
// alone. Converting needs rates the engine has never held; summing puts two
// currencies' minor units in one int64; and the order is already the property
// the engine maintains it for — by currency code, so that the same payments
// recorded in a different journal order draw the same row (see
// Position.IncomeByCurrency).
//
// An empty income is published as an empty ARRAY, never as null. The contract
// requires the field, and "no payment of any kind" is a statement a reader is
// entitled to, distinct from an entry that happens to be zero.
func incomeByCurrencyToAPI(income []CurrencyMinor) []apitypes.PositionCurrencyIncome {
	out := make([]apitypes.PositionCurrencyIncome, 0, len(income))
	for _, e := range income {
		out = append(out, apitypes.PositionCurrencyIncome{
			Currency:    e.Currency,
			IncomeMinor: e.Minor,
		})
	}
	return out
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
		Instrument: instrumentToAPI(inst),
		Quantity:   p.Quantity.String(),
		CostMinor:  p.CostMinor,
		// The currency of the position's cost, and on ONE KIND OF ROW a
		// convention rather than a fact: a position whose journal holds nothing
		// but payments — the paper was bought before the import window, or
		// arrived by transfer — never learned what it was priced in, and
		// carries the lowest currency code among those payments so that the
		// same money always draws the same row (see Position.Currency, which
		// spells out both halves). Published as-is either way: the contract has
		// one currency field and no way to say "this one is only a label", so
		// widening it is a change to the contract rather than something this
		// function may decide.
		Currency: p.Currency,
		// THE POSITION'S REALIZED RESULT, OR NOTHING AT ALL. It goes null when a
		// disposal settled in another currency than the basis it retired — a yuan
		// bond redeemed for rubles — and then there is no figure to put here IN
		// ANY CURRENCY: proceeds in one currency and basis in another have a
		// difference that is a quantity of neither, and this object holds no rate
		// to bridge them (see Position.RealizedPnL).
		//
		// The result is not lost with it. in_base.realized_pnl_minor converts each
		// disposal's own terms at each term's own date and out of each term's own
		// currency, so a row carrying that object publishes the very figure this
		// field cannot — and the null here is about which currency to name it in,
		// not about anything being unknown.
		RealizedPnlMinor: realizedToAPI(p),
		// INCOME IN THE POSITION'S OWN CURRENCY, AND NOTHING ELSE. This is one
		// int64 published beside `currency`, and the screen renders it under
		// that sign — so the only figure it can carry honestly is the one
		// denominated in that currency. A position's income may arrive in
		// several (see Position.IncomeByCurrency: a yuan bond pays rubles), and
		// neither answer to "what else could go here" is available: summing them
		// puts kopecks under a yuan sign, and converting them needs a rate this
		// object neither has nor publishes.
		//
		// SO INCOME IN ANOTHER CURRENCY IS NOT IN THIS FIGURE AT ALL, and a
		// position that received only such income has a zero here. That is an
		// omission rather than a false figure — every kopeck this number
		// contains really is in `currency` — and it is no longer an omission
		// from the RESPONSE: IncomeByCurrency below publishes the whole income
		// unconverted, one entry per currency, and the contract says in as many
		// words that this field is one of its terms rather than a summary of it.
		// The base-currency figure is the other complete answer, WHERE IT
		// EXISTS: in_base.income_minor converts every payment out of the
		// currency it actually arrived in (see positionInBase), so a row
		// carrying that object shows the whole income as a single number. A row
		// that does not carry it — because the position's currency IS the base
		// one, or because some other term of the object could not be valued —
		// has the per-currency list and this field, and nothing is hidden by
		// either absence.
		IncomeMinor:      p.IncomeMinorIn(p.Currency),
		IncomeByCurrency: incomeByCurrencyToAPI(p.IncomeByCurrency),
		// FEES IN THE POSITION'S OWN CURRENCY, AND NOTHING ELSE — the same rule
		// as the income above, applied to the same shape (Position.FeesByCurrency).
		// A commission charged in another currency is not in this figure and is
		// not published anywhere else either: unlike the income there is no
		// fees_by_currency, because nothing renders one. That is an omission the
		// contract states rather than a figure that lies — every kopeck here
		// really is in `currency`.
		FeesMinor:              p.FeesMinorIn(p.Currency),
		HasUndatedLots:         hasUndatedLots(p),
		HasUndatedRealizations: hasUndatedRealizations(p),
		// The starting value, and the answer for a row where the valuation is
		// struck and needs no explaining. Every other path below overwrites it
		// with a named cause. It is an explicit null, never an unspecified key
		// (the contract requires the field on every position, and an
		// unspecified nullable does not marshal as one).
		MarketValueGap: nullable.NewNullNullable[apitypes.MarketValueGap](),
	}
	// Settled needs no valuation — both its halves are past events — so it is
	// set here, once, and survives every early return below. Total is the one
	// that waits: it has an unrealized half, and that half exists only where a
	// valuation was struck in this position's own currency.
	settled, err := settledToAPI(p)
	if err != nil {
		return apitypes.Position{}, err
	}
	out.SettledMinor = settled
	out.TotalMinor = nullable.NewNullNullable[int64]()
	// The quote is looked up first and JUDGED LAST: marketValue takes `quoted`
	// and decides the type and the face value before it, so the two causes an
	// arriving quote would not close are reported ahead of the one it would
	// (see marketValue). Nothing here re-decides that order.
	q, quoted := quotes[p.InstrumentID]
	minor, currency, gap, err := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, quoted)
	if err != nil {
		// The valuation does not fit in an int64 — a broken quantity or price,
		// not absent data — so it is surfaced as a request error rather than
		// published as the same null a position without a valuation gets.
		return apitypes.Position{}, err
	}
	if apiGap, missing := apiMarketValueGap(gap); missing {
		// No valuation, and the dash the client renders in its place carries
		// the reason it is there. Published from the one function that decided
		// it, never inferred here from market_value_minor being nil — that
		// inference is what said «Нет котировки» over a crypto row holding a
		// perfectly good quote (#78).
		out.MarketValueGap = nullable.NewNullableWithValue(apiGap)
		return out, nil
	}
	out.Price = nullable.NewNullableWithValue(q.Price.String())
	out.PriceOn = nullable.NewNullableWithValue(q.On.Format("2006-01-02"))
	// Only a bond gets the money price, and only here — past every gap, so a row
	// without a valuation carries the same null for this as it does for the
	// quote. The face value is non-nil by construction on this path: marketValue
	// answers valuationNoFaceValue for a bond without one, and that gap returned
	// above.
	//
	// NO TEST SEPARATES THE TWO CONDITIONS, and nothing can: the catalog refuses
	// a face value on anything but a bond (instrument.checkFaceType), and the
	// importer sets a nominal only on bonds, so a non-bond carrying one is not a
	// row this program can produce. The type check states the rule all the same
	// — for a share the price already IS money and this field would restate it,
	// and for any type marketValue does not multiply by the face value, the
	// number would be unrelated to the valuation beside it.
	if inst.Type == instrument.TypeBond && inst.FaceValueMinor != nil {
		perUnit, err := pricePerUnitMinor(*inst.FaceValueMinor, q.Price)
		if err != nil {
			return apitypes.Position{}, fmt.Errorf("%w: %s%% of a face value of %d", err, q.Price, *inst.FaceValueMinor)
		}
		out.PriceMoneyMinor = nullable.NewNullableWithValue(perUnit)
	}

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
			//
			// It goes through apiMarketValueGap like the three absences above,
			// so every wire value this file publishes comes out of one mapping.
			// The `ok` result is dropped because the argument is a literal
			// constant: only valuationStruck answers false, and this is not it.
			apiGap, _ := apiMarketValueGap(valuationNoRateValuationCurrency)
			out.MarketValueGap = nullable.NewNullableWithValue(apiGap)
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
	// #93 closed the write side's own hole — checkFacePair
	// (internal/instrument/http.go) now bounds a face value's sign and magnitude
	// at the one door that writes it, so a bond's valuation can no longer go
	// negative through a broken face_value_minor. THIS GUARD DOES NOT RETIRE ALL
	// THE SAME, because a broken field is not the only way the basis goes
	// negative: Position.CostMinor is accumulated with a bare += in the engine
	// (see addLot, engine.go:702), each buy adding at most its amount plus its
	// fee — 2×10^15 minor units, the largest the write side admits of either —
	// so about 4612 buys of the same instrument, every one of them individually
	// valid and none of them carrying a broken field, take the running total past
	// math.MaxInt64 and wrap it into a large negative basis. Position's
	// realized total accumulates the same way (see realize, engine.go:212).
	// Neither is this package's to fix in passing: the engine is a pure fold and
	// changing what it answers is a change to every figure derived from it.
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
		total, err := totalToAPI(out.SettledMinor, out.UnrealizedPnlMinor, p.InstrumentID)
		if err != nil {
			return apitypes.Position{}, err
		}
		out.TotalMinor = total
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

// datedMinor is one amount, the currency it is denominated in, and the date
// whose fx rate values it: a lot's remaining cost with the day that lot was
// acquired, an income payment's amount with the day it occurred.
//
// THE CURRENCY TRAVELS WITH THE TERM rather than being one argument for the
// whole sum, because one sum's terms need not share it. A position's income can
// arrive in several currencies at once — a dollar share paying a ruble dividend
// with a ruble tax withheld (see portfolio.Position.IncomeByCurrency) — and
// converting those payments out of the position's currency would apply a dollar
// rate to an amount of rubles. The basis and the disposals are in the position's
// own currency by construction, and their terms say so one by one like every
// other.
type datedMinor struct {
	minor int64
	from  string
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

// sumInBase converts every amount at the fx rate of its OWN date, out of its
// OWN currency, and returns the total in currency to. Every amount is
// multiplied as a decimal and only
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
func (h *Handler) sumInBase(ctx context.Context, amounts []datedMinor, to string, cache map[rateKey]*rateLookup) (minor int64, ok bool, err error) {
	total := decimal.Zero
	for _, a := range amounts {
		rl := h.rateFor(ctx, a.from, to, a.on, cache)
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
func lotTerms(lots []Lot, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedLot(lots) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(lots))
	for _, l := range lots {
		// The position's currency, passed in: a lot's cost is what was paid for
		// the paper, and Position.Currency is exactly the currency that was paid
		// (every operation that adds a lot settles it — see
		// Type.mustMatchPositionCurrency).
		terms = append(terms, datedMinor{minor: l.CostMinor, from: currency, on: *l.AcquiredOn})
	}
	return terms, true
}

// incomeTerms flattens a position's income operations into the terms of ONE
// sum, each at the rate of the day it occurred and OUT OF THE CURRENCY IT
// ARRIVED IN (see incomeByInstrument for which operations those are, and
// positionInBase for why not one rate for the total).
//
// The currency is read off each operation rather than taken from the position,
// and that is the difference between this and its two sibling functions. A
// position's payments need not share the currency its cost is in — a yuan bond
// pays its coupons in rubles, a dollar share's dividend and the tax withheld on
// it arrive in rubles (see portfolio.Position.IncomeByCurrency) — so one sum
// here can hold terms in three currencies, each converted out of its own. Using
// the position's currency for all of them would multiply an amount of rubles by
// a dollar rate and publish the product.
//
// There is no undated case: an operation always has the day it occurred on.
// Like lotTerms and realizedTerms, it is called both by the sum and by the
// enumeration that prefetches the sum's rates.
func incomeTerms(income []Operation) []datedMinor {
	terms := make([]datedMinor, 0, len(income))
	for _, o := range income {
		terms = append(terms, datedMinor{minor: o.AmountMinor, from: o.Currency, on: o.OccurredOn})
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
func realizedTerms(events []Realization, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedRealization(events) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(events)*2)
	for _, e := range events {
		// TWO CURRENCIES, AND EACH TERM IS TAGGED WITH ITS OWN. The proceeds and
		// the fee are in the currency the disposal settled in
		// (Realization.Currency) — usually the position's and not always, since
		// a yuan bond may be redeemed for rubles. The retired basis is in the
		// position's, always: what the purchases behind it were denominated in
		// is exactly what the currency rule settles
		// (Operation.mustMatchPositionCurrency).
		//
		// This is what makes the base-currency figure exist for a position whose
		// own-currency figure does not: nothing here ever subtracts one currency
		// from another — every term is converted first, at its own date, and only
		// then summed.
		terms = append(terms,
			datedMinor{minor: e.ProceedsMinor, from: e.Currency, on: e.OccurredOn},
			datedMinor{minor: -e.FeeMinor, from: e.Currency, on: e.OccurredOn},
		)
		for _, r := range e.Released {
			terms = append(terms, datedMinor{minor: -r.CostMinor, from: currency, on: *r.AcquiredOn})
		}
	}
	return terms, true
}

// realizedToAPI publishes a position's realized result, or the null that says
// there is none to publish.
//
// It is a function rather than an expression at the call site so that the two
// halves of Position.RealizedPnL cannot be separated: the figure is reachable
// only together with the answer to whether it is one.
func realizedToAPI(p *Position) nullable.Nullable[int64] {
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		return nullable.NewNullNullable[int64]()
	}
	return nullable.NewNullableWithValue(minor)
}

// settledToAPI is what the position HAS LOCKED IN: the result of the disposals
// it has already made, plus what the paper has paid it. Both halves are past
// events with dates of their own, so this figure never moves again — which is
// the whole reason it is published apart from the one that includes the
// valuation.
//
// IT NEEDS EVERY TERM IN THE POSITION'S OWN CURRENCY, and goes null otherwise
// rather than summing what it happens to have:
//
//   - the realized result may not exist at all (a disposal settled in another
//     currency — see Position.RealizedPnL), and there is then nothing to add;
//   - the income figure beside it is only the part denominated in this
//     currency (see Position.IncomeByCurrency). A yuan bond paid in rubles has
//     income this figure cannot see, and adding the visible part would publish
//     a number smaller than the truth under a name that says "everything".
//
// The base-currency object carries the same two figures with every term
// converted, so a row that has one publishes there what it cannot publish here.
func settledToAPI(p *Position) (nullable.Nullable[int64], error) {
	realized, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency || !incomeIsAllIn(p, p.Currency) {
		return nullable.NewNullNullable[int64](), nil
	}
	sum, err := money.Add(realized, p.IncomeMinorIn(p.Currency))
	if err != nil {
		return nullable.Nullable[int64]{}, fmt.Errorf(
			"%w: settled result of instrument %s, a realized %d and income of %d",
			err, p.InstrumentID, realized, p.IncomeMinorIn(p.Currency))
	}
	return nullable.NewNullableWithValue(sum), nil
}

// incomeIsAllIn reports whether every payment this position received is
// denominated in one currency — the one asked about.
//
// Read off the LIST rather than by comparing IncomeMinorIn against some other
// total: the list is what holds the currencies, and a position that received
// nothing has an empty one and passes, which is right — nothing is missing from
// a sum of no payments.
func incomeIsAllIn(p *Position, currency string) bool {
	for _, e := range p.IncomeByCurrency {
		if e.Currency != currency {
			return false
		}
	}
	return true
}

// totalToAPI is the settled result plus what the holding is worth beyond its
// basis today. It is the answer to "what has this paper come to", and it mixes
// two kinds of certainty on purpose — the settled half is final, the unrealized
// half moves every day — which is why the settled figure stays published beside
// it rather than being folded away.
//
// Null whenever either half is, and for the halves' own reasons: no valuation,
// a valuation in another currency, or a settled figure that does not exist.
func totalToAPI(settled, unrealized nullable.Nullable[int64], instrument uuid.UUID) (nullable.Nullable[int64], error) {
	if settled.IsNull() || !settled.IsSpecified() || unrealized.IsNull() || !unrealized.IsSpecified() {
		return nullable.NewNullNullable[int64](), nil
	}
	sum, err := money.Add(settled.MustGet(), unrealized.MustGet())
	if err != nil {
		return nullable.Nullable[int64]{}, fmt.Errorf(
			"%w: total result of instrument %s, a settled %d and an unrealized %d",
			err, instrument, settled.MustGet(), unrealized.MustGet())
	}
	return nullable.NewNullableWithValue(sum), nil
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
		return apitypes.InBaseGapUndatedLot, true
	case inBaseNoRateLotDate:
		return apitypes.InBaseGapNoRateLotDate, true
	case inBaseNoRateIncomeDate:
		return apitypes.InBaseGapNoRateIncomeDate, true
	case inBaseNoRateToday:
		return apitypes.InBaseGapNoRateToday, true
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
	if minor, inOneCurrency := p.RealizedPnL(); p.Currency == to && inOneCurrency {
		// Nothing to convert: the position's own realized result already IS
		// the base-currency one. The published `in_base` object is null for
		// such a position, but the figure is not unknown, and the account's
		// total would silently lose a real term if this answered otherwise.
		//
		// THE SHORTCUT NEEDS BOTH HALVES. A position whose currency is already
		// the base one can still have sold into another — a ruble account
		// holding a yuan bond redeemed for rubles is that row seen from the
		// other side — and there the position's own figure does not exist to be
		// handed over, while the base-currency one is perfectly strikeable from
		// the terms below. Testing only the currency published a null for a
		// figure this handler could compute.
		return nullable.NewNullableWithValue(minor), gapNone, nil
	}
	terms, dated := realizedTerms(p.Realizations, p.Currency)
	if !dated {
		return nullable.NewNullNullable[int64](), gapUndated, nil
	}
	minor, ok, err := h.sumInBase(ctx, terms, to, cache)
	if err != nil {
		return nullable.Nullable[int64]{}, gapNone, err
	}
	if !ok {
		return nullable.NewNullNullable[int64](), gapNoRate, nil
	}
	return nullable.NewNullableWithValue(minor), gapNone, nil
}

// withAccountTax puts the account's own tax withholdings on the realized total.
//
// Attached here rather than inside realizedTotals because it is not a total OF
// the positions: the accumulator walks positions and this walks the journal's
// cash-level entries, which belong to no position at all. Keeping them apart is
// what stops the two ever being added into one figure by accident.
func withAccountTax(total apitypes.RealizedTotal, ops []Operation) apitypes.RealizedTotal {
	total.TaxWithheldByCurrency = taxWithheldFromAccount(ops)
	return total
}

// taxWithheldFromAccount sums the tax charged against the ACCOUNT — the
// entries that name no security — per currency.
//
// THE ATTRIBUTED TAX IS DELIBERATELY NOT HERE. A withholding the broker tied to
// a paper is already inside that position's income (the engine books a tax as
// negative income, see Compute), so counting it again in a figure a reader is
// invited to subtract would take the same money twice.
//
// What is left is the tax nothing on the positions screen can otherwise
// account for. In Russia the broker withholds it when money is taken OUT,
// against the year's accumulated base rather than against any one disposal: on
// the owner's own account 20 of these 21 entries fall on the same day as a
// withdrawal, and three have no disposal in the preceding month at all. That is
// exactly why it is summed per account and never divided per position.
func taxWithheldFromAccount(ops []Operation) []apitypes.CurrencyAmount {
	byCurrency := map[string]int64{}
	for _, o := range ops {
		if o.Type != TypeTax || o.InstrumentID != nil {
			continue
		}
		byCurrency[o.Currency] += o.AmountMinor
	}
	out := make([]apitypes.CurrencyAmount, 0, len(byCurrency))
	for currency, minor := range byCurrency {
		out = append(out, apitypes.CurrencyAmount{Currency: currency, AmountMinor: minor})
	}
	// By currency code, for the reason every other per-currency list in this
	// package is: a map's order is random and these figures go on a screen.
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// cashToAPI values one currency's balance in the base currency and publishes it
// as the contract's CashPosition.
//
// TWO RATES FOR TWO QUESTIONS, the same pair every other holding on this screen
// uses. What the money is worth is today's question and takes today's rate. What
// it COST is a past one and takes the rate of the day each parcel arrived — the
// parcels being what the queue left behind, oldest spent first (see
// portfolio.Cash). A balance valued entirely at today's rate would report a
// profit of exactly nought on every account, for ever.
//
// THE OWN-CURRENCY FIGURES ARE NOT PUBLISHED because there is nothing to
// publish: a thousand yuan cost a thousand yuan and is worth a thousand yuan,
// and a profit column of nought in every row is an answer to a question nobody
// asked. The base currency is where this money has a result at all — and where
// the base currency IS this currency, the rate is one and the result is an
// honest nought.
//
// A NEGATIVE BALANCE HAS NO COST. Nothing is held, so nothing was paid for it:
// the parcels are empty by construction and the sum over them is zero. The
// valuation is still struck — money owed in yuan is worth something in rubles —
// so the profit on such a row is the whole of that negative valuation, which is
// true and is what a reader should see while the journal is missing the
// purchases behind it.
func (h *Handler) cashToAPI(ctx context.Context, p *CashPosition, base string, now time.Time, cache map[rateKey]*rateLookup) (apitypes.CashPosition, error) {
	out := apitypes.CashPosition{
		Currency:    p.Currency,
		AmountMinor: p.Minor,
		InBase: apitypes.CashInBase{
			Currency: base,
			Gap:      nullable.NewNullNullable[apitypes.CashGap](),
		},
	}
	value, ok, err := h.sumInBase(ctx, []datedMinor{{minor: p.Minor, from: p.Currency, on: now}}, base, cache)
	if err != nil {
		return apitypes.CashPosition{}, err
	}
	if !ok {
		out.InBase.ValueMinor = nullable.NewNullNullable[int64]()
		out.InBase.CostMinor = nullable.NewNullNullable[int64]()
		out.InBase.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNoRateToday)
		return out, nil
	}
	out.InBase.ValueMinor = nullable.NewNullableWithValue(value)

	terms := make([]datedMinor, 0, len(p.Lots))
	for _, l := range p.Lots {
		terms = append(terms, datedMinor{minor: l.Minor, from: p.Currency, on: l.On})
	}
	cost, ok, err := h.sumInBase(ctx, terms, base, cache)
	if err != nil {
		return apitypes.CashPosition{}, err
	}
	if !ok {
		// The valuation stands and the cost does not, so the profit cannot be
		// struck either — and the two nulls are published with the reason
		// rather than with the valuation subtracted from nothing.
		out.InBase.CostMinor = nullable.NewNullNullable[int64]()
		out.InBase.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		// ONE CAUSE IS PUBLISHED, and this one is chosen over the realized
		// result's: a parcel still held without a rate is the gap a reader can
		// act on, and the realized figure below would be reported with the same
		// two words on an account where it is perfectly strikeable. The realized
		// result is withheld with it rather than published beside a null cost —
		// half an answer under one caption is what the single cause exists to
		// prevent.
		out.InBase.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNoRateLotDate)
		return out, nil
	}
	out.InBase.CostMinor = nullable.NewNullableWithValue(cost)
	if p.Minor < 0 {
		// AN OVERDRAFT HAS NO GAIN TO REPORT. Nothing is held, so the cost is a
		// structural nought, and value less nought is the WHOLE OF THE DEBT
		// wearing the name of a profit — on the owner's own account that put
		// -157 751,84 ₽ in a column headed «Прибыль» and folded the same figure
		// into what the account had supposedly earned. What it really says is
		// that the journal is missing the purchases behind those dollars.
		//
		// The two figures that ARE true stay: what the debt is worth (money owed
		// in dollars is worth something in rubles) and what the money that has
		// already left earned on the way out.
		out.InBase.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNegativeBalance)
	} else {
		pnl, err := money.Sub(value, cost)
		if err != nil {
			return apitypes.CashPosition{}, fmt.Errorf("%w: the %s balance worth %d against a cost of %d", err, p.Currency, value, cost)
		}
		out.InBase.UnrealizedPnlMinor = nullable.NewNullableWithValue(pnl)
	}

	// WHAT THIS MONEY HAS ALREADY EARNED, struck from the days it actually
	// happened on: each departure's proceeds at the rate of the day it left,
	// against its parcels at the rates of the days they came. Nothing here is
	// today's — both ends are past, which is why this figure is final.
	//
	// It gaps ON ITS OWN. A missing rate behind a disposal says nothing about
	// the balance still held, so the valuation and the unrealized figure above
	// stand, and only this one is withheld — the same argument the positions'
	// realized result already stands on (see realizedInBase).
	realized, ok, err := h.cashRealizedInBase(ctx, p, base, cache)
	if err != nil {
		return apitypes.CashPosition{}, err
	}
	if !ok {
		out.InBase.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNoRateDisposalDate)
		return out, nil
	}
	out.InBase.RealizedPnlMinor = nullable.NewNullableWithValue(realized)
	return out, nil
}

// cashRealizedInBase is the currency result this money has already banked: for
// every departure, what it came to on the day it went, less what its parcels
// were worth on the days they arrived.
//
// ONE SUM, NOT A SUM OF ROUNDED PIECES. Both sides go through sumInBase, which
// multiplies every term as a decimal and rounds once — the same treatment a
// position's realized result gets, and for the same reason: the published figure
// is the total, so the total is what may be rounded.
func (h *Handler) cashRealizedInBase(ctx context.Context, p *CashPosition, to string, cache map[rateKey]*rateLookup) (int64, bool, error) {
	if len(p.Realizations) == 0 {
		// Nothing has left, so the result is nought rather than unknown — and
		// no rate is asked for, which matters on an account whose money has
		// never moved out of a currency the fx table cannot reach.
		return 0, true, nil
	}
	proceeds := make([]datedMinor, 0, len(p.Realizations))
	var costs []datedMinor
	for _, r := range p.Realizations {
		proceeds = append(proceeds, datedMinor{minor: r.Minor(), from: p.Currency, on: r.OccurredOn})
		for _, l := range r.Released {
			costs = append(costs, datedMinor{minor: l.Minor, from: p.Currency, on: l.On})
		}
	}
	gotProceeds, ok, err := h.sumInBase(ctx, proceeds, to, cache)
	if err != nil || !ok {
		return 0, false, err
	}
	gotCost, ok, err := h.sumInBase(ctx, costs, to, cache)
	if err != nil || !ok {
		return 0, false, err
	}
	minor, err := money.Sub(gotProceeds, gotCost)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s departures worth %d against a cost of %d", err, p.Currency, gotProceeds, gotCost)
	}
	return minor, true, nil
}

// accountTotals adds up what the account HAS MADE — every position's total
// (realized result + income + unrealized revaluation) plus the account's own
// charges, which no position can see.
//
// It is realizedTotals' bigger sibling and works the same way: two forms because
// the screen has two modes, the server adds and the client renders, and the
// terms added are the ROUNDED per-position figures published in the same
// response rather than the raw terms re-summed (see AccountTotal.in_base in the
// contract — a header that disagreed with the rows one field away would be a
// number nobody could check).
//
// TWO ASSUMPTIONS RIDE ALONG WITH IT, and both are published as counts rather
// than buried:
//
//   - A HOLDING NOTHING PRICES GOES IN AT NOUGHT. Its basis counts as spent and
//     nothing counts as held, so the total is lower than the truth by whatever
//     the paper is really worth. This is the owner's decision, taken over the
//     alternative of publishing no total at all while a single frozen fund sits
//     in the account, and it is the conservative reading of a paper nobody can
//     sell. zeroValued counts them and zeroValuedCost says how much basis went
//     in that way, so the size of the assumption is a number.
//   - A HOLDING THAT DOES NOT KNOW WHAT IT COST goes in with its whole market
//     value as profit, which pushes the total the other way. Nothing can be done
//     about it here — the broker never sent the price — so it is counted and
//     named too.
//
// THE ACCOUNT'S OWN CHARGES are the terms no row carries: interest credited on
// the cash, commissions booked as operations of their own, and the tax taken
// from the account rather than from a payment. Commissions charged ON a trade
// are deliberately NOT among them: a purchase capitalizes its commission into
// the lot's cost and a disposal subtracts its own from the proceeds, so both are
// already inside the position figures being added here, and taking them again
// would charge the owner twice for one commission.
type accountTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	// unknowable names the currencies whose bucket is missing a term, for the
	// same reason realizedTotals.notInOneCurrency does: a position that
	// realized into another currency, or was paid in one, has no own-currency
	// total to add, and a bucket short a term is a number that reads as a
	// result rather than as a gap.
	unknowable     map[string]bool
	inBaseMinor    int64
	undated        bool
	noRate         bool
	zeroValued     int
	zeroValuedCost map[string]int64
	unknownCost    int
	// undatedPositions counts the holdings left out of the base figure because
	// their purchase dates were never recorded (see addPosition).
	undatedPositions int
	// noRateCurrencies names the money this account holds that could not be
	// valued. It is filled from the CASH alone, where the missing rate is
	// exactly that currency against the base one — a position's gap can be
	// about the currency its valuation is struck in rather than its own, and
	// naming that one would be a guess (see AccountTotal.no_rate_currencies).
	noRateCurrencies map[string]bool
}

func newAccountTotals(baseCurrency string) *accountTotals {
	return &accountTotals{
		baseCurrency:     baseCurrency,
		byCurrency:       make(map[string]int64),
		unknowable:       make(map[string]bool),
		zeroValuedCost:   make(map[string]int64),
		noRateCurrencies: make(map[string]bool),
	}
}

// addPosition folds one row in, in both currencies at once.
//
// The four cases it separates are the whole of the rule. A row with a total
// contributes it. A row with no valuation AT ALL contributes its settled result
// less its basis — the paper counted at nought — and is counted as such. A row
// whose total is missing for any other reason (nothing settled in this currency,
// or a valuation struck in a currency this row cannot be compared with) makes
// the bucket unknowable, because the term genuinely does not exist rather than
// being nought. And the base figure answers all three again from its own object,
// which carries its own arithmetic (see PositionInBase).
func (at *accountTotals) addPosition(p apitypes.Position, inBase *apitypes.PositionInBase, gap inBaseGap, realizedGap baseGap) error {
	// The paper is held and cost nothing to hold: a transfer the broker sent
	// with no price attached. Counted before anything else, because it is true
	// of the row whatever else is or is not known about it.
	if p.Quantity != "0" && p.CostMinor == 0 {
		at.unknownCost++
	}

	native, nativeOK := rowTotal(p.TotalMinor, p.SettledMinor, p.CostMinor, p.MarketValueGap.IsSpecified() && !p.MarketValueGap.IsNull())
	if !nativeOK {
		at.unknowable[p.Currency] = true
	} else {
		sum, err := money.Add(at.byCurrency[p.Currency], native.minor)
		if err != nil {
			return fmt.Errorf("%w: the account's total in %s, adding %d to %d",
				err, p.Currency, native.minor, at.byCurrency[p.Currency])
		}
		at.byCurrency[p.Currency] = sum
		if native.atZero {
			// COUNTED FROM THE ROW'S OWN CURRENCY, deliberately, even though the
			// mark stands beside the base figure. The base branch below writes
			// off the same paper for the same reason — nothing prices it — so
			// counting there instead would say the same thing, and counting in
			// both would say it twice.
			//
			// The one row the two branches disagree about is a paper with no
			// quote whose payments arrived in a third currency: it has no
			// own-currency total to write off (the bucket is unknowable) while
			// its base figure is struck perfectly well. Such a row goes into the
			// base total at nought and is NOT counted here — the figure it
			// qualifies is right and the count beside it is one short. Left as
			// it is rather than papered over, because the alternative shapes
			// each say something false: counting on the base branch would count
			// nothing at all on an account with no conversion objects, and
			// counting on both would double every ordinary row.
			at.zeroValued++
			cost, err := money.Add(at.zeroValuedCost[p.Currency], p.CostMinor)
			if err != nil {
				return fmt.Errorf("%w: the basis counted at nought in %s, adding %d to %d",
					err, p.Currency, p.CostMinor, at.zeroValuedCost[p.Currency])
			}
			at.zeroValuedCost[p.Currency] = cost
		}
	}

	// The row's own gap, mapped onto the two words this total has. The dateless
	// lot is the one that never closes, so it lands on `undated`; the three rate
	// gaps land on `no_rate`, which says a figure may yet appear. Both of the
	// non-gaps fall through: one has an object, the other has nothing to
	// convert because the row is already in the base currency.
	switch gap {
	case inBaseUndatedLot:
		// LEFT OUT, NOT SUPPRESSING. Nobody knows when this paper was bought, so
		// its basis has no day whose rate could value it — and unlike a missing
		// rate, that never resolves: a date cannot arrive later. Taking the
		// whole account's figure down for ever over it answered nothing, and on
		// the owner's own journal it left five accounts of six blank.
		//
		// The count is what keeps this honest, and it is the same bargain the
		// owner struck for a paper nothing prices: publish the figure, and say
		// what it rests on.
		at.undatedPositions++
		return nil
	case inBaseNoRateLotDate, inBaseNoRateIncomeDate, inBaseNoRateToday:
		at.noRate = true
		return nil
	}
	// No gap covers two shapes: an object was struck, or the position is
	// already in the base currency and there was nothing to convert — in which
	// case its own figures ARE the base ones. Reading the pointer is what tells
	// them apart, exactly as handleList does when it publishes them.
	base, baseOK := native, nativeOK
	if inBase != nil {
		base, baseOK = rowTotal(inBase.TotalMinor, inBase.SettledMinor, inBase.CostMinor,
			p.MarketValueGap.IsSpecified() && !p.MarketValueGap.IsNull())
	}
	if !baseOK {
		// The base figure has no settled result to build on, and WHY decides
		// what to do about it. A disposal whose parcels have no acquisition day
		// can never be valued — the same permanent gap the branch above answers,
		// so the paper is left out and counted. A missing RATE is the opposite:
		// the figure appears when the backfill catches up, and publishing a
		// total without this paper meanwhile would quietly change it later.
		if realizedGap == gapUndated {
			at.undatedPositions++
			return nil
		}
		at.noRate = true
		return nil
	}
	sum, err := money.Add(at.inBaseMinor, base.minor)
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding %d to %d",
			err, at.baseCurrency, base.minor, at.inBaseMinor)
	}
	at.inBaseMinor = sum
	return nil
}

// rowTotalValue is one row's contribution and whether the paper behind it was
// counted at nought.
type rowTotalValue struct {
	minor  int64
	atZero bool
}

// rowTotal reads one row's contribution out of the three figures the contract
// publishes for it. Shared by both currencies because the shapes are identical:
// PositionInBase carries a total, a settled result and a basis under the same
// names and the same nullability rules as the row itself.
//
// unvalued is the row's own answer to "is there a valuation at all" — the gap,
// which is non-null on exactly that row and null both when a valuation was
// struck and when the row is a closed position with nothing left to value.
func rowTotal(total, settled nullable.Nullable[int64], costMinor int64, unvalued bool) (rowTotalValue, bool) {
	if !total.IsNull() {
		return rowTotalValue{minor: total.MustGet()}, true
	}
	if unvalued && !settled.IsNull() {
		// Counted at nought: the settled result stands, and the basis of what
		// is still held is written off. money.Sub rather than a bare minus —
		// both terms survived money.Minor, their difference need not (#83).
		minor, err := money.Sub(settled.MustGet(), costMinor)
		if err != nil {
			return rowTotalValue{}, false
		}
		return rowTotalValue{minor: minor, atZero: costMinor != 0}, true
	}
	return rowTotalValue{}, false
}

// addCharge folds one of the account's own charges in: interest, a commission
// booked as its own operation, or tax taken from the account. baseMinor is the
// same amount converted at the rate of the day it was charged, null when no rate
// for that day exists.
func (at *accountTotals) addCharge(currency string, minor int64, baseMinor nullable.Nullable[int64]) error {
	sum, err := money.Add(at.byCurrency[currency], minor)
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding a charge of %d to %d",
			err, currency, minor, at.byCurrency[currency])
	}
	at.byCurrency[currency] = sum
	if baseMinor.IsNull() {
		at.noRate = true
		return nil
	}
	base, err := money.Add(at.inBaseMinor, baseMinor.MustGet())
	if err != nil {
		return fmt.Errorf("%w: the account's total in %s, adding a charge of %d to %d",
			err, at.baseCurrency, baseMinor.MustGet(), at.inBaseMinor)
	}
	at.inBaseMinor = base
	return nil
}

// addCash folds one currency's money in — the currency's own result, and the
// only term of this total that has nothing to do with any paper.
//
// BASE CURRENCY ONLY, and that is not an omission. In its own currency a
// thousand yuan cost a thousand yuan and is worth a thousand yuan: there is no
// result to add, today or ever, and adding a nought to each bucket would be
// noise. In the base currency the same money has both halves of one — what it
// earned on the way out (realized) and what it is worth against what it cost
// (unrealized) — and those are exactly the two figures a reader means by "what
// did my currency do".
//
// WHY BOTH HALVES AND NOT ONE. Take a hundred thousand rubles to dollars at 100
// and back at 120: the money made twenty thousand rubles and the balances
// afterwards value to nought, because everything it earned is in the departure.
// Take dollars and hold them: nothing has left, and everything is in the
// unrealized half. An account does both.
//
// NOTHING HERE IS DOUBLE-COUNTED WITH THE PAPERS. A share bought with dollars
// spends dollar parcels — banking the currency's move up to that day — and the
// share's own basis is struck at the rate of the day it was bought, so the two
// figures meet at that day and neither covers the other's ground.
func (at *accountTotals) addCash(c apitypes.CashPosition) error {
	if c.Currency == at.baseCurrency {
		// Rubles in a ruble space: both halves are structurally nought, and
		// asking for them would be asking a rate of one to say something.
		return nil
	}
	// An overdraft contributes what it EARNED and nothing else. Its unrealized
	// half does not exist (see cashToAPI), and that absence is not a gap in the
	// data: there is no gain on money the account does not have, so nothing is
	// withheld over it and no currency is named.
	halves := []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor, c.InBase.UnrealizedPnlMinor}
	if !c.InBase.Gap.IsNull() && c.InBase.Gap.MustGet() == apitypes.CashGapNegativeBalance {
		halves = []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor}
	}
	for _, half := range halves {
		if half.IsNull() {
			// A rate behind this money is missing, so the account has no single
			// figure — and the currency is named, because «нет курса» alone
			// cannot tell a gap that closes when the backfill catches up from
			// one that never closes at all. The Bank of Russia publishes no rate
			// for XAU, the code the broker uses for gold, and no amount of
			// waiting will produce one.
			at.noRate = true
			at.noRateCurrencies[c.Currency] = true
			continue
		}
		sum, err := money.Add(at.inBaseMinor, half.MustGet())
		if err != nil {
			return fmt.Errorf("%w: the account's total in %s, adding %d of currency result to %d",
				err, at.baseCurrency, half.MustGet(), at.inBaseMinor)
		}
		at.inBaseMinor = sum
	}
	return nil
}

// result is the account's answer as the contract publishes it.
func (at *accountTotals) result() apitypes.AccountTotal {
	out := apitypes.AccountTotal{
		BaseCurrency:             at.baseCurrency,
		ByCurrency:               make([]apitypes.AccountCurrencyTotal, 0, len(at.byCurrency)),
		ZeroValuedPositions:      at.zeroValued,
		ZeroValuedCostByCurrency: make([]apitypes.CurrencyAmount, 0, len(at.zeroValuedCost)),
		NoRateCurrencies:         make([]string, 0, len(at.noRateCurrencies)),
		UndatedPositions:         at.undatedPositions,
		UnknownCostPositions:     at.unknownCost,
	}
	// Every currency that has a bucket OR a hole in one: a currency whose only
	// news is that it cannot be totalled must still say so, or the account
	// silently has one fewer currency than it holds.
	seen := make(map[string]bool, len(at.byCurrency)+len(at.unknowable))
	for currency := range at.byCurrency {
		seen[currency] = true
	}
	for currency := range at.unknowable {
		seen[currency] = true
	}
	for currency := range seen {
		entry := apitypes.AccountCurrencyTotal{
			Currency:    currency,
			AmountMinor: nullable.NewNullableWithValue(at.byCurrency[currency]),
		}
		if at.unknowable[currency] {
			entry.AmountMinor = nullable.NewNullNullable[int64]()
		}
		out.ByCurrency = append(out.ByCurrency, entry)
	}
	sort.Slice(out.ByCurrency, func(i, j int) bool {
		return out.ByCurrency[i].Currency < out.ByCurrency[j].Currency
	})
	for currency, minor := range at.zeroValuedCost {
		out.ZeroValuedCostByCurrency = append(out.ZeroValuedCostByCurrency,
			apitypes.CurrencyAmount{Currency: currency, AmountMinor: minor})
	}
	sort.Slice(out.ZeroValuedCostByCurrency, func(i, j int) bool {
		return out.ZeroValuedCostByCurrency[i].Currency < out.ZeroValuedCostByCurrency[j].Currency
	})

	for currency := range at.noRateCurrencies {
		out.NoRateCurrencies = append(out.NoRateCurrencies, currency)
	}
	sort.Strings(out.NoRateCurrencies)

	switch {
	case at.undated && at.noRate:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.Both)
	case at.undated:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.Undated)
	case at.noRate:
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	default:
		out.InBase = nullable.NewNullableWithValue(at.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	return out
}

// accountCharges are the journal entries the account is charged or credited
// DIRECTLY, which no position figure contains: interest on the cash, every
// commission booked as an operation of its own, and the tax taken from the
// account rather than withheld from a payment.
//
// EACH EXCLUSION IS A CLAIM ABOUT THE ENGINE, and each is checked there rather
// than assumed here. A commission charged on a trade is capitalized into the
// lot's cost (buy) or subtracted from the proceeds (sell), so it is already
// inside the totals being added — while an operation of type fee touches
// nothing but Position.FeesByCurrency, which no published total reads, and would
// vanish entirely if it were not taken here. A tax attributed to an instrument
// is already inside that position's income; one without an instrument reaches no
// position at all. Interest is refused by type and never reaches a position
// either.
//
// Amount and fee are taken together — an entry's cash effect is its amount less
// its own commission, the same formula the reconciliation uses — so a charge
// that carries one does not lose it.
func accountCharges(ops []Operation) []Operation {
	var out []Operation
	for _, o := range ops {
		switch o.Type {
		case TypeInterest, TypeFee:
			out = append(out, o)
		case TypeTax:
			if o.InstrumentID == nil {
				out = append(out, o)
			}
		}
	}
	return out
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
	// notInOneCurrency names the currencies whose bucket is missing a term
	// because some position in it realized into another currency and has no
	// own-currency figure at all (see Position.RealizedPnL). The bucket is
	// published as a null rather than as the sum of the rest: a total quietly
	// short one of its terms is an invented number that looks exactly like a
	// real one, which is the rule the base total below already stands on.
	notInOneCurrency map[string]bool
	inBaseMinor      int64
	undated          bool
	noRate           bool
}

func newRealizedTotals(baseCurrency string) *realizedTotals {
	return &realizedTotals{
		baseCurrency:     baseCurrency,
		byCurrency:       make(map[string]int64),
		notInOneCurrency: make(map[string]bool),
	}
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
func (rt *realizedTotals) add(currency string, nativeMinor nullable.Nullable[int64], baseMinor nullable.Nullable[int64], gap baseGap) error {
	native := rt.byCurrency[currency]
	if nativeMinor.IsNull() {
		// The position has no figure in its own currency, so this bucket has
		// none either — and it is marked BEFORE the base total is touched
		// below, because the two answers are independent: the base figure is
		// struck from the disposals' own terms and survives perfectly well
		// (see realizedInBase).
		rt.notInOneCurrency[currency] = true
	} else {
		var err error
		if native, err = money.Add(rt.byCurrency[currency], nativeMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, currency, nativeMinor.MustGet(), rt.byCurrency[currency])
		}
	}
	inBase := rt.inBaseMinor
	switch gap {
	case gapUndated:
		rt.undated = true
	case gapNoRate:
		rt.noRate = true
	default:
		// gapNone: the figure was struck, so there is one to add.
		var err error
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
		total := apitypes.RealizedCurrencyTotal{
			Currency:         currency,
			RealizedPnlMinor: nullable.NewNullableWithValue(minor),
		}
		if rt.notInOneCurrency[currency] {
			total.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		}
		out.ByCurrency = append(out.ByCurrency, total)
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
// operations by instrument, so each position's income can be converted payment
// by payment — at each payment's own rate, and out of each payment's own
// currency. Position.IncomeByCurrency answers neither question: it has kept the
// currencies but summed the dates away, and a total per currency could only
// ever be converted at one date of the many behind it.
//
// The type list must stay in lockstep with the engine's own notion of income
// (Compute: dividend, coupon and tax all book income, the tax through its
// negative amount; entries without an instrument are cash-level and never reach
// a position). Group by group and currency by currency, these operations
// therefore add up to exactly that position's IncomeByCurrency, and
// TestPositionInBaseIncomeUsesEachOperationsOwnRate exercises all three types,
// so a list that drifts from the engine's fails there instead of silently
// publishing a base income smaller than the income the same row reports in its
// own currency.
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
//     the day it occurred AND out of the currency that payment arrived in,
//     which need not be the position's: a yuan bond pays its coupons in rubles
//     (see Position.IncomeByCurrency). Income, unlike the lots, is passed in —
//     see incomeByInstrument. This figure is therefore the position's WHOLE
//     income, unlike the position's own income_minor beside it, which can only
//     carry the part denominated in the position's currency (see toAPI).
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
// EVERY FIGURE IS CONVERTED FROM THE CURRENCY IT IS ACTUALLY IN, ONCE. cost is
// in the position's own currency and always was. Income is in whichever
// currency each payment arrived in — one sum here can hold rubles, dollars and
// yuan, each term converted out of its own (see incomeTerms). The market
// valuation may be in a third: a bond's is born in its face currency and toAPI brings
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
// has already taken cost_minor down with it and this function returned nil far
// above, before the rule was reached at all. What is left is a
// missing F->RUB — and then F->baseCurrency has no route either, whatever the
// base currency is. The figure is absent because there is none, not because one
// is being kept back.
//
// cost_minor is named alone there, and income_minor is not named beside it any
// more: a payment is converted out of the currency it arrived in, so a
// position's income need never touch P at all (see incomeTerms). It is the
// lots, whose costs are in P by construction, that carry the argument.
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
// SOME ROWS STILL SLIP THROUGH, and the written-out rule covers them. A
// position holding no lot asks for no rate on P at all, and neither does its
// income unless a payment happened to arrive in P — so such a row survives a
// missing P->RUB, and for it F->RUB may well exist and the base-currency
// valuation be genuinely computable. What the rule withholds there is the zero
// of a closed row: with no lot there is no quantity (the lots' quantities sum to
// Position.Quantity), and a valuation is a price times nothing.
//
// Asymmetry between this object and the position's own figures is ordinary, not
// something this rule prevents: realized_pnl_minor goes null here while
// Position.realized_pnl_minor is published either way, and one undated lot nulls
// this whole object while every native figure stays. Null here is honest in all
// of those cases: the frontend falls back to showing the raw amount in its own
// currency with a "not converted" marker.
//
// WHEN p.Currency IS THE BASE CURRENCY THIS OBJECT IS NOT BUILT, and that is
// where widening income leaves a hole in the screen rather than filling one.
// Such a row has nothing to convert about its cost — its own figures ARE the
// base-currency ones — but it can still have received a payment in another
// currency, and this object is the only place a payment is ever converted. That
// payment then appears nowhere at all: not in the position's own income_minor,
// which carries the position's currency alone (see toAPI), and not here,
// because there is no here. What decides it is the shape of the contract: this
// object is specified as null on such a row, and income_minor is a single
// int64. Closing it means giving a position a per-currency income field, which
// is the next piece of work and not a decision this function may take alone.
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
	// NOTHING TO CONVERT — ALMOST ALWAYS. A position denominated in the base
	// currency publishes every figure it has already in that currency, and a
	// second object repeating them under the same sign would say nothing.
	//
	// THE EXCEPTION IS A DISPOSAL THAT SETTLED IN A THIRD CURRENCY. Then the
	// position's own realized figure does not exist at all
	// (Position.RealizedPnL), while a base-currency one is perfectly strikeable
	// from the disposal's terms — and this object is the only place the payload
	// has to put it. Returning early here published a position whose realized
	// result appeared NOWHERE: a null in its own currency, and no object beside
	// it to carry the answer.
	//
	// The rest of the object is then a set of identity conversions, and that is
	// the point rather than a cost: every figure in it equals the position's
	// own, so nothing about it can contradict the row it stands on.
	if _, inOneCurrency := p.RealizedPnL(); p.Currency == baseCurrency && inOneCurrency {
		return nil, inBaseSameCurrency, nil
	}

	lots, dated := lotTerms(p.Lots, p.Currency)
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
	costMinor, ok, err := h.sumInBase(ctx, lots, baseCurrency, cache)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateLotDate, nil
	}

	// Each payment out of the currency IT arrived in (see incomeTerms), which
	// is why this sum takes no source currency: a position's income can hold
	// three currencies at once and the position's own is not necessarily one of
	// them.
	incomeMinor, ok, err := h.sumInBase(ctx, incomeTerms(income), baseCurrency, cache)
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
	// Both terms are already converted here — each at the rate of its own date —
	// and the income one covers every payment whatever currency it arrived in.
	// So this figure exists on rows where the position-currency one cannot,
	// which is the whole reason it is worth publishing twice.
	if !realizedMinor.IsNull() && realizedMinor.IsSpecified() {
		settled, err := money.Add(realizedMinor.MustGet(), incomeMinor)
		if err != nil {
			return nil, inBaseStruck, fmt.Errorf("%w: settled result of instrument %s in %s, a realized %d and income of %d",
				err, p.InstrumentID, baseCurrency, realizedMinor.MustGet(), incomeMinor)
		}
		out.SettledMinor = nullable.NewNullableWithValue(settled)
	} else {
		out.SettledMinor = nullable.NewNullNullable[int64]()
	}
	out.TotalMinor = nullable.NewNullNullable[int64]()

	// Which figure this object converts, and out of which currency. The GUARD
	// comes first and the rule it enforces is unchanged: a valuation that never
	// reached the position's currency is not carried into the base one (see this
	// function's doc comment — in a table where every rate is quoted in RUB,
	// there is no base-currency figure to carry).
	//
	// WHAT IT ASKS changed with the gap. It used to compare
	// market_value_currency with p.Currency, which is toAPI's outcome read back
	// out of the payload; it now asks toAPI's own answer, market_value_gap. The
	// two are equivalent today — the failed conversion is the only way the two
	// currencies can differ, and the `c == nil` half of the old condition was
	// already redundant, since toAPI sets market_value_minor and
	// market_value_currency together and a nil valuation is handled below
	// regardless. What the change buys is that the cause published to the reader
	// and the figure withheld from them are one decision instead of two that
	// must keep agreeing: a
	// caption saying the valuation could not be converted, over a converted
	// valuation, is precisely the failure this whole change is about.
	//
	// ANY gap withholds it, which is the same rule stated once for four values
	// rather than one guard per value (#78 gave the field three more). Three of
	// them say no valuation was struck at all, and on those rows this line
	// changes nothing — market_value_minor is already nil. The fourth is the one
	// it was written for. That the guard is now partly redundant is what makes
	// it right: "a gap of any kind means there is no valuation this object may
	// carry" holds for whatever value is added next, where a guard naming
	// `no_rate_valuation_currency` would silently stop covering it.
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
	total, err := totalToAPI(out.SettledMinor, out.UnrealizedPnlMinor, p.InstrumentID)
	if err != nil {
		return nil, inBaseStruck, err
	}
	out.TotalMinor = total
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
// THE SOURCE CURRENCIES COME OFF THE SAME TERMS, and now they must: a term
// carries the currency it is denominated in (see datedMinor), so a position's
// income — which need not be in the position's currency at all — enumerates one
// pair per payment, in whatever currency that payment arrived, without this
// function having to know that income is the sum which behaves so.
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
	cash map[string]*CashPosition,
	baseCurrency string,
	now time.Time,
) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	// The money's own two questions, asked for every currency: what it is worth
	// today, and what each parcel still held was worth on the day it arrived
	// (see cashToAPI). Left out of this warm-up, each of them becomes a lookup
	// of its own inside the loop below — which is the N+1 the round-trip test
	// guards, and which caught exactly this omission.
	for currency, p := range cash {
		if currency == baseCurrency {
			continue
		}
		out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
		for _, l := range p.Lots {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
		}
		// And the days its departures happened on, plus the days the parcels
		// they took had arrived — the two ends of the result already banked.
		for _, r := range p.Realizations {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: r.OccurredOn})
			for _, l := range r.Released {
				out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
			}
		}
	}
	for _, p := range positions {
		inst, known := instruments[p.InstrumentID]
		q, quoted := quotes[p.InstrumentID]
		if known {
			// marketValue's error is deliberately ignored here, as
			// operation.rateQueries ignores amountTerms': this pass only
			// predicts which rates to fetch, and the loop calls the same
			// function on the same position moments later and fails from the
			// place that knows which figure it was building (see toAPI). A
			// valuation that cannot be struck simply asks for no rate, which is
			// also all this function could usefully do about it.
			//
			// `quoted` is passed in rather than gating the call, exactly as in
			// toAPI: this must ask the same function the same question the loop
			// will, and marketValue answers `no quote` itself now.
			if _, currency, gap, _ := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, quoted); gap == valuationStruck {
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
		if lots, dated := lotTerms(p.Lots, p.Currency); dated {
			out = appendTermQueries(out, lots, baseCurrency)
		}
		out = appendTermQueries(out, incomeTerms(income[p.InstrumentID]), baseCurrency)
		if realized, dated := realizedTerms(p.Realizations, p.Currency); dated {
			out = appendTermQueries(out, realized, baseCurrency)
		}
	}
	return dedupeQueries(out)
}

// appendTermQueries asks for the rate of every term's own date, out of every
// term's own currency — which is what sumInBase will do with the same terms,
// one at a time. Both halves come off the term itself, so a sum whose terms are
// in three currencies prefetches three pairs without this function knowing that
// income is the sum that does it.
func appendTermQueries(dst []marketdata.RateQuery, terms []datedMinor, to string) []marketdata.RateQuery {
	for _, t := range terms {
		dst = append(dst, marketdata.RateQuery{From: t.from, To: to, On: t.on})
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
	// The money the account holds, folded from the same journal by the same pure
	// function the positions come from. Built BEFORE the warm-up so its rates
	// are asked for in the same batch as everything else's.
	cashPositions, err := Cash(ops)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.prewarmRates(r.Context(), rateQueries(positions, instruments, quotes, income, cashPositions, sp.BaseCurrency, now), rates)

	totals := newRealizedTotals(sp.BaseCurrency)
	account := newAccountTotals(sp.BaseCurrency)
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

		// Folded in AFTER the in_base object is decided, because the account's
		// total reads that object: it is the one place the row's converted
		// figures exist, and re-striking them here would be the second
		// computation of one value this package keeps warning about.
		if err := account.addPosition(apiPos, inBase, gap, realizedGap); err != nil {
			family.WriteError(w, err)
			return
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	// THE MONEY, beside the papers. Valued here rather than in the fold for the
	// reason every rate on this page is applied here: the calculating core holds
	// none.
	cash := make([]apitypes.CashPosition, 0, len(cashPositions))
	for _, p := range CashByCurrency(cashPositions) {
		one, err := h.cashToAPI(r.Context(), p, sp.BaseCurrency, now, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		cash = append(cash, one)
		if err := account.addCash(one); err != nil {
			family.WriteError(w, err)
			return
		}
	}

	// The account's own charges, each converted at the rate of the day it was
	// charged — the same rule every other past event on this screen follows
	// (НК РФ ст. 210 п. 5 for the money that is tax, and plain consistency for
	// the rest: a commission taken in 2019 was that many rubles in 2019).
	for _, o := range accountCharges(ops) {
		minor, err := money.Sub(o.AmountMinor, o.FeeMinor)
		if err != nil {
			family.WriteError(w, fmt.Errorf("%w: a charge of %d less its own fee of %d", err, o.AmountMinor, o.FeeMinor))
			return
		}
		base := nullable.NewNullableWithValue(minor)
		if o.Currency != sp.BaseCurrency {
			converted, ok, err := h.sumInBase(r.Context(), []datedMinor{{minor: minor, from: o.Currency, on: o.OccurredOn}}, sp.BaseCurrency, rates)
			if err != nil {
				family.WriteError(w, err)
				return
			}
			if !ok {
				base = nullable.NewNullNullable[int64]()
			} else {
				base = nullable.NewNullableWithValue(converted)
			}
		}
		if err := account.addCharge(o.Currency, minor, base); err != nil {
			family.WriteError(w, err)
			return
		}
	}

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
		RealizedTotal: withAccountTax(totals.result(), ops),
		AccountTotal:  account.result(),
		Cash:          cash,
	})
}
