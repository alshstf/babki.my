package portfolio

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// withAccountTax puts the account's own tax withholdings on the realized
// total. They belong to no position, so they are kept apart from the
// positions' sum.
func withAccountTax(total apitypes.RealizedTotal, ops []Operation) apitypes.RealizedTotal {
	total.TaxWithheldByCurrency = taxWithheldFromAccount(ops)
	return total
}

// taxWithheldFromAccount sums, per currency, the tax charged to the account
// rather than to a paper (paper taxes are already in that position's income).
// Russian brokers withhold it when money is withdrawn, against the year's base
// (20 of 21 such entries on the owner's account fall on a withdrawal day), so
// it is never divided per position.
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
	// Ordered by currency code: map order is random.
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// cashToAPI values one currency's balance in the base currency: worth at
// today's rate, cost at each held parcel's arrival rate. Own-currency figures
// are not published: money's result exists only against another currency. A
// negative balance has no cost; its valuation is still struck.
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
		// The valuation stands and the cost does not, so the profit is null too, with
		// the reason.
		out.InBase.CostMinor = nullable.NewNullNullable[int64]()
		out.InBase.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		// One cause is published, this one: a held parcel's missing rate. The realized
		// result is withheld with it rather than published under the same caption.
		out.InBase.RealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNoRateLotDate)
		return out, nil
	}
	out.InBase.CostMinor = nullable.NewNullableWithValue(cost)
	if p.Minor < 0 {
		// An overdraft has no gain: value less a structural zero cost would show the
		// whole debt as a profit (−157 751,84 ₽ on the owner's account). Its value and
		// its realized result stay.
		out.InBase.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.InBase.Gap = nullable.NewNullableWithValue(apitypes.CashGapNegativeBalance)
	} else {
		pnl, err := money.Sub(value, cost)
		if err != nil {
			return apitypes.CashPosition{}, fmt.Errorf("%w: the %s balance worth %d against a cost of %d", err, p.Currency, value, cost)
		}
		out.InBase.UnrealizedPnlMinor = nullable.NewNullableWithValue(pnl)
	}

	// What the money has already earned: each departure at its day's rate against
	// its parcels at theirs. It gaps on its own.
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

// cashRealizedInBase is the banked currency result: each departure's value on
// its day less its parcels' values on their arrival days, summed and rounded
// once via sumInBase.
func (h *Handler) cashRealizedInBase(ctx context.Context, p *CashPosition, to string, cache map[rateKey]*rateLookup) (int64, bool, error) {
	if len(p.Realizations) == 0 {
		// Nothing has left: nought, and no rate is asked for.
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
