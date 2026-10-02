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
