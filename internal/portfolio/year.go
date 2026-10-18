package portfolio

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// YearFacts is what an account's journal says of one calendar year, as an
// estimate of the year's tax needs it (decision Р-41), in currency at the
// rates of each figure's own day (НК РФ ст. 210 п. 5): the disposals' result
// — proceeds and fee at the disposal's day, each released piece at its
// purchase's — and the part of it on pieces held longer than the long-holding
// years; the coupons, dividends and interest paid; the taxes withheld. A
// figure that cannot be valued — a piece with no day, a missing rate — is
// left out and counted in Unvalued, so the caller can say the estimate is
// short. WithheldJanuary is the part of Withheld taken in the year's
// January, WithheldAfter the taxes withheld in the January after it: a
// Russian broker withholds a year's tax at its end, often in January
// (decision Р-60 Б).
type YearFacts struct {
	Sales, SalesLong                         int64
	Coupons, Dividends, Interest             int64
	Withheld, WithheldJanuary, WithheldAfter int64
	Unvalued                                 int
}

// YearFacts reads the account's journal for year.
func (s *Service) YearFacts(ctx context.Context, spaceID, accountID uuid.UUID, year int, currency string, longYears int) (YearFacts, error) {
	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return YearFacts{}, err
	}
	positions, err := Compute(ops)
	if err != nil {
		return YearFacts{}, journalDoesNotCompute{err}
	}
	rates := marketdata.NewRateMemo(s.conv)
	in := func(t time.Time) bool { return t.Year() == year }
	var out YearFacts
	value := func(minor int64, from string, on time.Time) (decimal.Decimal, bool, error) {
		if from == currency {
			return decimal.NewFromInt(minor), true, nil
		}
		r := rates.Rate(ctx, from, currency, on)
		if errors.Is(r.Err, marketdata.ErrNoRate) {
			return decimal.Zero, false, nil
		}
		if r.Err != nil {
			return decimal.Zero, false, r.Err
		}
		return decimal.NewFromInt(minor).Mul(r.Rate), true, nil
	}
	for _, p := range positions {
		for _, e := range p.Realizations {
			if !in(e.OccurredOn) {
				continue
			}
			sales, long, ok, err := disposal(e, p.Currency, longYears, value)
			if err != nil {
				return YearFacts{}, err
			}
			if !ok {
				out.Unvalued++
				continue
			}
			out.Sales += sales
			out.SalesLong += long
		}
	}
	for _, o := range ops {
		after := o.Type == TypeTax && o.OccurredOn.Year() == year+1 && o.OccurredOn.Month() == time.January
		if !in(o.OccurredOn) && !after {
			continue
		}
		var into *int64
		switch o.Type {
		case TypeCoupon:
			into = &out.Coupons
		case TypeDividend:
			into = &out.Dividends
		case TypeInterest:
			into = &out.Interest
		case TypeTax:
			into = &out.Withheld
		default:
			continue
		}
		v, ok, err := value(o.AmountMinor, o.Currency, o.OccurredOn)
		if err != nil {
			return YearFacts{}, err
		}
		if !ok {
			out.Unvalued++
			continue
		}
		minor := v.Round(0).IntPart()
		if o.Type == TypeTax {
			// A tax row takes money: withheld is what it took.
			minor = -minor
			switch {
			case after:
				into = &out.WithheldAfter
			case o.OccurredOn.Month() == time.January:
				out.WithheldJanuary += minor
			}
		}
		*into += minor
	}
	return out, nil
}

// disposal is one disposal's result in the currency and the part of it on
// pieces held longer than longYears; ok is false when a piece cannot be
// valued. The proceeds and the fee are shared out over the pieces by
// quantity — by cost for an amortization, whose pieces carry none.
func disposal(e Realization, positionCurrency string, longYears int,
	value func(int64, string, time.Time) (decimal.Decimal, bool, error),
) (total, long int64, ok bool, err error) {
	on := e.OccurredOn
	if e.RateOn != nil {
		on = *e.RateOn
	}
	net, ok, err := value(e.ProceedsMinor-e.FeeMinor, e.Currency, on)
	if err != nil || !ok {
		return 0, 0, false, err
	}
	if len(e.Released) == 0 {
		// An amortization after the basis is spent: all of it is gain.
		return net.Round(0).IntPart(), 0, true, nil
	}
	weight := func(r ReleasedLot) decimal.Decimal { return r.Quantity }
	whole := decimal.Zero
	for _, r := range e.Released {
		whole = whole.Add(r.Quantity)
	}
	if whole.IsZero() {
		weight = func(r ReleasedLot) decimal.Decimal { return decimal.NewFromInt(r.CostMinor) }
		for _, r := range e.Released {
			whole = whole.Add(decimal.NewFromInt(r.CostMinor))
		}
	}
	sum, longSum := decimal.Zero, decimal.Zero
	since := e.OccurredOn.AddDate(-longYears, 0, 0)
	for _, r := range e.Released {
		share := net
		if whole.IsPositive() {
			share = net.Mul(weight(r)).Div(whole)
		}
		cost := decimal.Zero
		switch {
		case r.AcquiredOn == nil && r.CostMinor != 0:
			// No day for its rate: the result cannot be told.
			return 0, 0, false, nil
		case r.AcquiredOn != nil:
			paid := *r.AcquiredOn
			if r.RateOn != nil {
				paid = *r.RateOn
			}
			c, ok, err := value(r.CostMinor, positionCurrency, paid)
			if err != nil || !ok {
				return 0, 0, false, err
			}
			cost = c
		}
		gain := share.Sub(cost)
		sum = sum.Add(gain)
		if longYears > 0 && r.AcquiredOn != nil && r.AcquiredOn.Before(since) {
			longSum = longSum.Add(gain)
		}
	}
	return sum.Round(0).IntPart(), longSum.Round(0).IntPart(), true, nil
}
