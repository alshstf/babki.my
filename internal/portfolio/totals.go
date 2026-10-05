package portfolio

import (
	"fmt"
	"sort"

	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// accountTotals adds up what the account has made: every position's total
// (realized + income + unrealized) plus the account's own charges. It adds the
// rounded per-position figures published beside it, so the header agrees with
// the rows.
//
// Two assumptions are published as counts: a holding nothing prices counts at
// nought (the owner's decision — its basis is spent, nothing is held), and a
// holding with unknown cost counts its whole value as profit.
//
// The account's own charges are interest, standalone commissions and account
// level tax; commissions on trades are already inside the position figures.
type accountTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	// unknowable names currencies whose bucket lacks a term (a position with no
	// own-currency total); the bucket is then null rather than short.
	unknowable     map[string]bool
	inBaseMinor    int64
	noRate         bool
	zeroValued     int
	zeroValuedCost map[string]int64
	unknownCost    int
	// undatedPositions counts holdings left out of the base figure for lack of
	// purchase dates.
	undatedPositions int
	// noRateCurrencies names the cash currencies that could not be valued; only
	// cash, where the missing pair is exactly that currency to base.
	noRateCurrencies map[string]bool
	// cashFXMinor is the currency result on the account's money within
	// inBaseMinor (decision Р-5, its own line). cashFXSeen: some foreign money
	// contributed; cashFXGap: a rate behind it is missing.
	cashFXMinor int64
	cashFXSeen  bool
	cashFXGap   bool
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

// addPosition folds one row in, in both currencies. A row with a total adds
// it; a row with no valuation adds its settled result less its basis (counted
// at nought); any other missing total makes the bucket unknowable. The base
// figure is answered again from the row's own in_base object.
func (at *accountTotals) addPosition(p apitypes.Position, inBase *apitypes.PositionInBase, gap inBaseGap, realizedGap baseGap) error {
	// Unknown cost is counted first: it is true of the row whatever else is
	// known.
	if p.HasUnknownCost {
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
			// Counted from the row's own currency only, so it is not counted twice. A
			// quoteless paper paid in a third currency goes into the base total at nought
			// uncounted — a known undercount, preferred to the alternatives.
			at.zeroValued++
			cost, err := money.Add(at.zeroValuedCost[p.Currency], p.CostMinor)
			if err != nil {
				return fmt.Errorf("%w: the basis counted at nought in %s, adding %d to %d",
					err, p.Currency, p.CostMinor, at.zeroValuedCost[p.Currency])
			}
			at.zeroValuedCost[p.Currency] = cost
		}
	}

	// The row's gap maps onto this total's two words: an undated lot is
	// `undated`, the rate gaps `no_rate`.
	switch gap {
	case inBaseUndatedLot:
		// Left out and counted, not suppressing the total: an unknown purchase date
		// never resolves, and blanking the account forever answered nothing.
		at.undatedPositions++
		return nil
	case inBaseNoRateLotDate, inBaseNoRateIncomeDate, inBaseNoRateToday:
		at.noRate = true
		return nil
	}
	// No gap: either an object was struck or the row is already in the base
	// currency, told apart by the pointer.
	base, baseOK := native, nativeOK
	if inBase != nil {
		base, baseOK = rowTotal(inBase.TotalMinor, inBase.SettledMinor, inBase.CostMinor,
			p.MarketValueGap.IsSpecified() && !p.MarketValueGap.IsNull())
	}
	if !baseOK {
		// Why the base settled result is missing decides: undated disposals are
		// permanent, so the paper is left out and counted; a missing rate will
		// resolve, so the total is withheld meanwhile.
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

// rowTotalValue is one row's contribution, and whether it counted at nought.
type rowTotalValue struct {
	minor  int64
	atZero bool
}

// rowTotal reads one row's contribution from its total, settled result and
// basis, in either currency (the shapes match). unvalued is whether the row
// has no valuation at all.
func rowTotal(total, settled nullable.Nullable[int64], costMinor int64, unvalued bool) (rowTotalValue, bool) {
	if !total.IsNull() {
		return rowTotalValue{minor: total.MustGet()}, true
	}
	if unvalued && !settled.IsNull() {
		// Counted at nought: settled result less the held basis, overflow-checked
		// (#83).
		minor, err := money.Sub(settled.MustGet(), costMinor)
		if err != nil {
			return rowTotalValue{}, false
		}
		return rowTotalValue{minor: minor, atZero: costMinor != 0}, true
	}
	return rowTotalValue{}, false
}

// addCharge folds one account charge in; baseMinor is it converted at its own
// day's rate, null without one.
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

// addCash folds one currency's money in, in the base currency only: in its
// own currency money has no result. Both halves count — realized (what it
// earned leaving) and unrealized (what it is worth against what it cost). It
// does not overlap the papers: a share's basis is struck at its purchase day's
// rate, where the money's result stops.
func (at *accountTotals) addCash(c apitypes.CashPosition) error {
	if c.Currency == at.baseCurrency {
		// Base-currency money has no result.
		return nil
	}
	// An overdraft contributes only its realized half; it has no unrealized one,
	// and that is not a gap.
	halves := []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor, c.InBase.UnrealizedPnlMinor}
	if !c.InBase.Gap.IsNull() && c.InBase.Gap.MustGet() == apitypes.CashGapNegativeBalance {
		halves = []nullable.Nullable[int64]{c.InBase.RealizedPnlMinor}
	}
	at.cashFXSeen = true
	for _, half := range halves {
		if half.IsNull() {
			at.cashFXGap = true
			// A missing rate withholds the figure and names the currency: it may resolve,
			// or never (the CBR publishes no rate for the broker's gold code, XAU).
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
		fx, err := money.Add(at.cashFXMinor, half.MustGet())
		if err != nil {
			return fmt.Errorf("%w: the account's currency result in %s, adding %d to %d",
				err, at.baseCurrency, half.MustGet(), at.cashFXMinor)
		}
		at.cashFXMinor = fx
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
	// Every currency with a bucket or a hole, so none silently disappears.
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

	if at.noRate {
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	} else {
		out.InBase = nullable.NewNullableWithValue(at.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	out.CashFxInBase = nullable.NewNullNullable[int64]()
	if at.cashFXSeen && !at.cashFXGap {
		out.CashFxInBase = nullable.NewNullableWithValue(at.cashFXMinor)
	}
	return out
}

// accountCharges are the entries charged or credited to the account directly,
// which no position contains: interest, standalone commissions, and tax not
// attributed to an instrument. Trade commissions and instrument taxes are
// already in positions. Amount and fee are taken together, as reconciliation
// does.
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

// realizedTotals adds up what the account's closed deals have locked in, by
// currency and in the base currency. Nothing is converted or rounded here: the
// terms are the published per-position figures, so the header and the rows
// agree, and the policy lives in one place on the server.
type realizedTotals struct {
	baseCurrency string
	byCurrency   map[string]int64
	// notInOneCurrency names buckets lacking a term (a position realized into
	// another currency); they are null, not short.
	notInOneCurrency map[string]bool
	inBaseMinor      int64
	// undatedPositions counts positions left out of the base sum because a
	// disposal released an undated parcel: permanent, so excluded and counted
	// rather than blanking the account (#158, #195).
	undatedPositions int
	// unknownCostPositions counts positions that sold parcels with no price; their
	// whole proceeds are profit (see UnknownCost).
	unknownCostPositions int
	noRate               bool
}

func newRealizedTotals(baseCurrency string) *realizedTotals {
	return &realizedTotals{
		baseCurrency:     baseCurrency,
		byCurrency:       make(map[string]int64),
		notInOneCurrency: make(map[string]bool),
	}
}

// add folds one position in: nativeMinor in its own currency, baseMinor in the
// base currency or null with the gap. The base sum keeps accumulating past a
// gap; result() drops it. Both sums are overflow-checked (#83), and an overflow
// is an error, not a gap; nothing is half-added.
func (rt *realizedTotals) add(currency string, nativeMinor nullable.Nullable[int64], baseMinor nullable.Nullable[int64], gap baseGap, soldUnknown bool) error {
	native := rt.byCurrency[currency]
	if nativeMinor.IsNull() {
		// No own-currency figure, so neither has the bucket; the base figure is
		// independent.
		rt.notInOneCurrency[currency] = true
	} else {
		var err error
		if native, err = money.Add(rt.byCurrency[currency], nativeMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, currency, nativeMinor.MustGet(), rt.byCurrency[currency])
		}
	}
	inBase, undated := rt.inBaseMinor, 0
	switch gap {
	case gapUndated:
		undated++
	case gapNoRate:
		rt.noRate = true
	default:
		// gapNone: there is a figure to add.
		var err error
		if inBase, err = money.Add(rt.inBaseMinor, baseMinor.MustGet()); err != nil {
			return fmt.Errorf("%w: the account's realized total in %s, adding %d to %d",
				err, rt.baseCurrency, baseMinor.MustGet(), rt.inBaseMinor)
		}
	}
	rt.byCurrency[currency], rt.inBaseMinor = native, inBase
	rt.undatedPositions += undated
	if soldUnknown {
		rt.unknownCostPositions++
	}
	return nil
}

// result is the account's answer as published. It is the exact sum of the
// rounded per-position figures, so the header matches its rows (see
// RealizedTotal.in_base).
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

	// A missing rate withholds the figure; a missing purchase day never resolves,
	// so those positions are left out and counted.
	out.UndatedPositions = rt.undatedPositions
	out.UnknownCostPositions = rt.unknownCostPositions
	if rt.noRate {
		out.InBase = nullable.NewNullNullable[int64]()
		out.InBaseGap = nullable.NewNullableWithValue(apitypes.NoRate)
	} else {
		out.InBase = nullable.NewNullableWithValue(rt.inBaseMinor)
		out.InBaseGap = nullable.NewNullNullable[apitypes.RealizedGap]()
	}
	return out
}
