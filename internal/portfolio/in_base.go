package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// baseGap names why a base-currency total could not be struck: a missing rate
// closes on its own, a missing purchase date never does.
type baseGap uint8

const (
	// gapNone: there is a figure.
	gapNone baseGap = iota
	// gapNoRate: every date is known but a rate is missing; the backfill will
	// close it.
	gapNoRate
	// gapUndated: a parcel's purchase date is unknown (see Lot.AcquiredOn) and
	// never will be.
	gapUndated
)

// inBaseGap names which term stopped a position's in_base object
// (Position.in_base_gap). Unlike baseGap, which sums many positions, it names
// terms, so the caption fits the figure: «нет курса» is false of an undated
// lot.
type inBaseGap uint8

const (
	// inBaseStruck: there is an object.
	inBaseStruck inBaseGap = iota
	// inBaseSameCurrency: the position is in the base currency, nothing to
	// convert. Distinct from inBaseStruck only for readability.
	inBaseSameCurrency
	// inBaseUndatedLot: a held lot's purchase date is unknown; the one gap that
	// never closes.
	inBaseUndatedLot
	// inBaseNoRateLotDate: no rate on or before some lot's purchase day.
	inBaseNoRateLotDate
	// inBaseNoRateIncomeDate: no rate for some income operation's day.
	inBaseNoRateIncomeDate
	// inBaseNoRateToday: no rate today for the valuation's currency against the
	// base one.
	inBaseNoRateToday
)

// apiInBaseGap maps a gap onto the contract; ok is false for the two non-gaps.
// One cause is published: positionInBase stops at the first term it cannot
// value, checking the undated lot first, so a row never promises a figure that
// is not coming.
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

// realizedInBase is the position's realized result in currency to, or null
// with the gap that stopped it. The position's in_base and the account total
// both use it, so they agree.
//
// Every term of every disposal is converted as a decimal and the total is
// rounded once, like cost and income. (A future per-disposal breakdown would
// round each line, as tax per trade does, and could differ from this by a
// unit.)
//
// A missing rate or purchase date nulls only this figure. A non-nil error is a
// real failure, never shown as null.
func (h *Handler) realizedInBase(ctx context.Context, p *Position, to string, rates *marketdata.RateMemo) (nullable.Nullable[int64], baseGap, error) {
	if minor, inOneCurrency := p.RealizedPnL(); p.Currency == to && inOneCurrency {
		// Nothing to convert, but only when the position is in the base currency and
		// every disposal settled in it; otherwise the figure is struck from the terms
		// below.
		return nullable.NewNullableWithValue(minor), gapNone, nil
	}
	terms, dated := realizedTerms(p.Realizations, p.Currency)
	if !dated {
		return nullable.NewNullNullable[int64](), gapUndated, nil
	}
	minor, ok, err := h.sumInBase(ctx, terms, to, rates)
	if err != nil {
		return nullable.Nullable[int64]{}, gapNone, err
	}
	if !ok {
		return nullable.NewNullNullable[int64](), gapNoRate, nil
	}
	return nullable.NewNullableWithValue(minor), gapNone, nil
}

// incomeByInstrument groups instrument-attributed income operations, so income
// is converted payment by payment at each one's own date and currency. The
// types match the engine's notion of income (dividend, coupon, tax);
// TestPositionInBaseIncomeUsesEachOperationsOwnRate holds them together.
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

// positionInBase expresses a position's figures in baseCurrency, each at the
// rate that answers its own question:
//   - cost_minor: the held lots, each at the rate of its purchase (or
//     settlement) day — not today's rate, which would cancel the currency's own
//     move out of the profit;
//   - income_minor: each payment at its own day's rate, out of the currency it
//     arrived in — the whole income, unlike the position's own figure;
//   - market_value_minor: today's rate, from the currency the valuation is
//     really in (a bond's face currency when it was converted);
//   - unrealized_pnl_minor: valuation minus basis, so it includes the currency's
//     move and may differ in sign from the native profit (owner's decision);
//   - realized_pnl_minor: passed in from realizedInBase.
//
// fees_minor is not carried.
//
// Each figure is converted once from its own currency: chaining the
// valuation's conversion through the position's currency lost a cent before
// the second rate (#39).
//
// The valuation is carried only when it reached the position's currency.
// Every stored rate is quoted in RUB, so a valuation that could not reach it
// cannot reach the base currency either; the rule is still written out for the
// day a non-RUB-quoted rate exists.
//
// A position in the base currency gets no object (and income in another
// currency then appears nowhere — a contract gap needing a per-currency income
// field). Otherwise the whole object is null, with the term that stopped it,
// when a held lot is undated or any rate the object needs is missing; today's
// rate is needed only when there is a valuation. Figures belonging to one
// value only — the valuation, the realized result — null only themselves. A
// non-nil error is a real failure; the gap beside it means nothing.
//
// now is the request's single "today", shared with toAPI.
func (h *Handler) positionInBase(ctx context.Context, p *Position, apiPos apitypes.Position, income []Operation, baseCurrency string, realizedMinor nullable.Nullable[int64], now time.Time, rates *marketdata.RateMemo) (*apitypes.PositionInBase, inBaseGap, error) {
	// Nothing to convert unless a disposal settled in a third currency: then the
	// native realized figure does not exist and this object must carry the base
	// one, its other figures being identity conversions.
	if _, inOneCurrency := p.RealizedPnL(); p.Currency == baseCurrency && inOneCurrency {
		return nil, inBaseSameCurrency, nil
	}

	lots, dated := lotTerms(p.Lots, p.Currency)
	if !dated {
		// An undated held lot: the basis cannot be valued, so the whole object goes —
		// a partial basis would look ordinary and drag the profit. Checked before any
		// rate, so the permanent cause is the one reported.
		return nil, inBaseUndatedLot, nil
	}
	costMinor, ok, err := h.sumInBase(ctx, lots, baseCurrency, rates)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateLotDate, nil
	}

	// Each payment out of its own currency (see incomeTerms).
	incomeMinor, ok, err := h.sumInBase(ctx, incomeTerms(income), baseCurrency, rates)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		return nil, inBaseNoRateIncomeDate, nil
	}

	// A realized result that cannot be struck nulls only itself.
	out := &apitypes.PositionInBase{
		CostMinor:        costMinor,
		IncomeMinor:      incomeMinor,
		RealizedPnlMinor: realizedMinor,
		Currency:         baseCurrency,
	}
	// Both terms already converted at their own dates, so this exists even where
	// the native figure cannot.
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

	// Any market_value_gap withholds the valuation: it never reached the
	// position's currency, so it cannot reach the base one (see the doc). The
	// decision is toAPI's, so the caption and the withheld figure agree.
	marketValueMinor := nullableValue(apiPos.MarketValueMinor)
	valuationCurrency := p.Currency
	if nullableValue(apiPos.MarketValueGap) != nil {
		marketValueMinor = nil
	}
	// Convert the original valuation from its source currency in one step.
	// marketValueMinor != nil is insurance should toAPI ever set the source on an
	// unconverted path.
	if src, srcCurrency := nullableValue(apiPos.MarketValueSourceMinor), nullableValue(apiPos.MarketValueSourceCurrency); marketValueMinor != nil && src != nil && srcCurrency != nil {
		marketValueMinor, valuationCurrency = src, *srcCurrency
	}
	if marketValueMinor == nil {
		// No valuation to convert. rate_on goes too: it dates the valuation and
		// nothing else. Today's rate is not asked for at all here, so a position
		// without a quote still publishes its cost and income.
		out.MarketValueMinor = nullable.NewNullNullable[int64]()
		out.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
		out.RateOn = nullable.NewNullNullable[string]()
		// The object stands; a withheld valuation is explained on the position itself
		// (market_value_gap).
		return out, inBaseStruck, nil
	}

	// Today's rate values the valuation and supplies rate_on. The pair is the
	// valuation's currency against the base one, so a missing face-currency rate
	// would also null cost and income — unreachable while every rate is quoted in
	// RUB, since the valuation reached the position's currency.
	today := rates.Rate(ctx, valuationCurrency, baseCurrency, now)
	if today.Err != nil {
		if errors.Is(today.Err, marketdata.ErrNoRate) {
			return nil, inBaseNoRateToday, nil
		}
		return nil, inBaseStruck, today.Err
	}
	valuation, err := applyRate(today, *marketValueMinor)
	if err != nil {
		// Too large to state: fail the request rather than join the null, which means
		// a rate will come.
		return nil, inBaseStruck, err
	}
	out.MarketValueMinor = nullable.NewNullableWithValue(valuation)
	// Overflow-checked: two int64s of opposite sign can differ by more than an
	// int64.
	unrealized, err := money.Sub(valuation, costMinor)
	if err != nil {
		// Named, since money.Sub names no figure.
		return nil, inBaseStruck, fmt.Errorf("%w: a base valuation of %d less a basis of %d in %s",
			err, valuation, costMinor, baseCurrency)
	}
	out.UnrealizedPnlMinor = nullable.NewNullableWithValue(unrealized)
	total, err := totalToAPI(out.SettledMinor, out.UnrealizedPnlMinor, p.InstrumentID)
	if err != nil {
		return nil, inBaseStruck, err
	}
	out.TotalMinor = total
	// An identity conversion has a rate of 1 on the zero date; publish null rather
	// than "0001-01-01".
	if today.RateDate.IsZero() {
		out.RateOn = nullable.NewNullNullable[string]()
	} else {
		out.RateOn = nullable.NewNullableWithValue(today.RateDate.Format("2006-01-02"))
	}
	return out, inBaseStruck, nil
}
