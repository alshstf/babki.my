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

// instrumentToAPI mirrors instrument's unexported mapping: each module owns its
// own domain-to-API mapping.
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

// anyUndatedLot reports whether the held basis contains a piece with no
// purchase date. It is published (has_undated_lots) because a null in_base
// has two causes — a rate the backfill will supply, or a date that will never
// be known — and it also decides in_base_gap's `undated_lot`, so the two
// cannot disagree. A lot with no date and no cost counts as bought for nothing
// instead (has_unknown_cost).
func anyUndatedLot(lots []Lot) bool {
	return slices.ContainsFunc(lots, func(l Lot) bool { return DatelessBasis(l.AcquiredOn, l.CostMinor) })
}

// hasUndatedLots publishes anyUndatedLot.
func hasUndatedLots(p *Position) bool {
	return anyUndatedLot(p.Lots)
}

// anyUndatedRealization reports whether a disposal retired a piece with no
// purchase date — anyUndatedLot for the sold side, which the held lots cannot
// show. It is published (has_undated_realizations) and decides the account
// total's `undated` gap, so the two cannot disagree. Pieces with no date and
// no cost do not count.
func anyUndatedRealization(events []Realization) bool {
	for _, e := range events {
		if slices.ContainsFunc(e.Released, func(r ReleasedLot) bool { return DatelessBasis(r.AcquiredOn, r.CostMinor) }) {
			return true
		}
	}
	return false
}

// hasUnknownCost reports whether any basis, held or sold, arrived with no
// price and counts as bought for nothing (see UnknownCost).
func hasUnknownCost(p *Position) bool {
	return slices.ContainsFunc(p.Lots, func(l Lot) bool { return UnknownCost(l.Quantity, l.CostMinor) }) ||
		soldUnknownCost(p)
}

// soldUnknownCost reports whether a disposal released such a parcel, so its
// whole proceeds went into the result.
func soldUnknownCost(p *Position) bool {
	for _, e := range p.Realizations {
		if slices.ContainsFunc(e.Released, func(r ReleasedLot) bool { return UnknownCost(r.Quantity, r.CostMinor) }) {
			return true
		}
	}
	return false
}

// hasUndatedRealizations publishes anyUndatedRealization.
func hasUndatedRealizations(p *Position) bool {
	return anyUndatedRealization(p.Realizations)
}

// incomeByCurrencyToAPI publishes the income as the engine kept it: per
// currency, in its order, unconverted and unsummed. Empty is [], never null.
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

// toAPI builds one position's representation, converting its valuation into
// the position's currency when needed. now is the request's single "today",
// shared with positionInBase and the prefetch. err is a real failure, never
// ErrNoRate.
func (h *Handler) toAPI(ctx context.Context, p *Position, inst instrument.Instrument, quotes map[uuid.UUID]marketdata.Quote, now time.Time, rates *marketdata.RateMemo) (apitypes.Position, error) {
	out := apitypes.Position{
		Instrument: instrumentToAPI(inst),
		Quantity:   p.Quantity.String(),
		CostMinor:  p.CostMinor,
		// The cost's currency; on an income-only position a label (the lowest code
		// among payments), which the contract cannot mark as such.
		Currency: p.Currency,
		// Null when a disposal settled in another currency: there is no figure in any
		// one currency. in_base.realized_pnl_minor carries it converted.
		RealizedPnlMinor: realizedToAPI(p),
		// Only the income in the position's currency, which the screen shows under
		// its sign. Income in other currencies is in income_by_currency and, converted,
		// in in_base.income_minor.
		IncomeMinor:      p.IncomeMinorIn(p.Currency),
		IncomeByCurrency: incomeByCurrencyToAPI(p.IncomeByCurrency),
		// Only fees in the position's currency; others are not published (nothing
		// renders them), as the contract states.
		FeesMinor:              p.FeesMinorIn(p.Currency),
		HasUndatedLots:         hasUndatedLots(p),
		HasUndatedRealizations: hasUndatedRealizations(p),
		HasUnknownCost:         hasUnknownCost(p),
		// Explicit null by default (the contract requires the field); overwritten with
		// a cause below.
		MarketValueGap: nullable.NewNullNullable[apitypes.MarketValueGap](),
	}
	// Settled needs no valuation, so it survives every early return; Total waits
	// for one in the position's currency.
	settled, err := settledToAPI(p)
	if err != nil {
		return apitypes.Position{}, err
	}
	out.SettledMinor = settled
	out.TotalMinor = nullable.NewNullNullable[int64]()
	// The quote is judged last, inside marketValue, so causes a quote would not
	// close are reported first.
	q, quoted := quotes[p.InstrumentID]
	minor, currency, gap, err := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, quoted)
	if err != nil {
		// An overflowing valuation is a request error, not the no-valuation null.
		return apitypes.Position{}, err
	}
	if apiGap, missing := apiMarketValueGap(gap); missing {
		// No valuation: publish the reason marketValue decided, never inferred from a
		// nil value (#78).
		out.MarketValueGap = nullable.NewNullableWithValue(apiGap)
		return out, nil
	}
	out.Price = nullable.NewNullableWithValue(q.Price.String())
	out.PriceOn = nullable.NewNullableWithValue(q.On.Format("2006-01-02"))
	if q.Source == ManualPriceSource {
		byHand := true
		out.PriceByHand = &byHand
	}
	// Only a bond gets the money price and the accrued interest, past every gap,
	// so it has a face (marketValue reported otherwise).
	out.AccruedInterestMinor = nullable.NewNullNullable[int64]()
	if face, _, ok := bondFace(inst.FaceValueMinor, inst.FaceCurrency, q, quoted); inst.Type == instrument.TypeBond && ok {
		perUnit, err := pricePerUnitMinor(face, q.Price)
		if err != nil {
			return apitypes.Position{}, fmt.Errorf("%w: %s%% of a face value of %s", err, q.Price, face)
		}
		out.PriceMoneyMinor = nullable.NewNullableWithValue(perUnit)
		if q.Bond != nil && q.Bond.Accrued != nil {
			accrued, err := money.Minor(q.Bond.Accrued.Shift(centsPerUnit))
			if err != nil {
				return apitypes.Position{}, fmt.Errorf("%w: accrued interest of %s", err, q.Bond.Accrued)
			}
			out.AccruedInterestMinor = nullable.NewNullableWithValue(accrued)
		}
	}

	// A bond's valuation is in its face currency, which may differ from the
	// position's. Convert it into the position's currency at today's rate, so cost
	// and value are comparable; the original is kept in market_value_source_* for
	// the tooltip and for positionInBase. The memo and applyRate give exactly what
	// Convert did, and let the prefetch cover this pair.
	if currency != p.Currency {
		rl := rates.Rate(ctx, currency, p.Currency, now)
		switch err := rl.Err; {
		case err == nil:
			converted, convErr := applyRate(rl, minor)
			if convErr != nil {
				// Rate found, product overflows: a request error, which must not look like a
				// missing rate.
				return apitypes.Position{}, convErr
			}
			out.MarketValueSourceCurrency = nullable.NewNullableWithValue(currency)
			out.MarketValueSourceMinor = nullable.NewNullableWithValue(minor)
			minor, currency = converted, p.Currency
		case errors.Is(err, marketdata.ErrNoRate):
			// No rate: publish the unconverted figure, with the gap decided here.
			// positionInBase reads this same gap, so the caption and the withheld figure
			// are one decision.
			apiGap, _ := apiMarketValueGap(valuationNoRateValuationCurrency)
			out.MarketValueGap = nullable.NewNullableWithValue(apiGap)
		default:
			// A real failure, not "no rate": propagate it.
			return apitypes.Position{}, err
		}
	}

	out.MarketValueMinor = nullable.NewNullableWithValue(minor)
	out.MarketValueCurrency = nullable.NewNullableWithValue(currency)
	// Both operands are in the position's currency here, by the guard above. The
	// subtraction is still overflow-checked (#83): CostMinor accumulates with
	// plain addition in the engine, so thousands of valid buys could wrap it. The
	// wrapped answer would be an enormous false profit, so it is refused.
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

// nullableValue reads a nullable field, unspecified and null alike as nil.
func nullableValue[T any](n nullable.Nullable[T]) *T {
	v, err := n.Get()
	if err != nil {
		return nil
	}
	return &v
}

// realizedToAPI publishes the realized result or null, keeping both halves of
// RealizedPnL together.
func realizedToAPI(p *Position) nullable.Nullable[int64] {
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		return nullable.NewNullNullable[int64]()
	}
	return nullable.NewNullableWithValue(minor)
}

// settledToAPI is what the position has locked in: realized result plus
// income. Null unless both are wholly in the position's currency; a partial
// sum would understate it. The base-currency object carries both converted.
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

// incomeIsAllIn reports whether every payment is in currency; no payments
// passes.
func incomeIsAllIn(p *Position, currency string) bool {
	for _, e := range p.IncomeByCurrency {
		if e.Currency != currency {
			return false
		}
	}
	return true
}

// totalToAPI is the settled result plus today's unrealized gain; null when
// either half is.
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
