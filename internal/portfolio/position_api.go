package portfolio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

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
//
// A lot with no date and no cost is not undated in this sense: it is counted as
// bought for nothing, and nought needs no date (see DatelessBasis). It is
// reported by has_unknown_cost instead.
func anyUndatedLot(lots []Lot) bool {
	return slices.ContainsFunc(lots, func(l Lot) bool { return DatelessBasis(l.AcquiredOn, l.CostMinor) })
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
//
// As with anyUndatedLot, a parcel with no date and no cost does not count: it
// was sold as bought for nothing, which needs no date (see DatelessBasis).
func anyUndatedRealization(events []Realization) bool {
	for _, e := range events {
		if slices.ContainsFunc(e.Released, func(r ReleasedLot) bool { return DatelessBasis(r.AcquiredOn, r.CostMinor) }) {
			return true
		}
	}
	return false
}

// hasUnknownCost reports whether any of the position's basis — still held or
// already sold — arrived with no purchase price and counts as bought for
// nothing (see UnknownCost). Published as Position.has_unknown_cost, so the
// paper itself can say that its profit is overstated by what was really paid.
func hasUnknownCost(p *Position) bool {
	return slices.ContainsFunc(p.Lots, func(l Lot) bool { return UnknownCost(l.Quantity, l.CostMinor) }) ||
		soldUnknownCost(p)
}

// soldUnknownCost reports whether a disposal released a parcel counted as bought
// for nothing, so that the whole of what it sold for went into the realized
// result.
func soldUnknownCost(p *Position) bool {
	for _, e := range p.Realizations {
		if slices.ContainsFunc(e.Released, func(r ReleasedLot) bool { return UnknownCost(r.Quantity, r.CostMinor) }) {
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
		HasUnknownCost:         hasUnknownCost(p),
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
	if q.Source == ManualPriceSource {
		byHand := true
		out.PriceByHand = &byHand
	}
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
