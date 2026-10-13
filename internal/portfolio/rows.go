package portfolio

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// YearRows is an account's year line by line, as a tax return lists it
// (decision Р-45): each coupon, dividend and interest with the rate of its
// day and the tax withheld on it; each disposal with the lots it gave up,
// every sum at the rate of its own day (НК РФ ст. 210 п. 5). Taxes the
// journal shows on no payment of the year are apart.
type YearRows struct {
	Payouts []Payout
	Sales   []Sale
	// OtherTaxes are tax rows set against no payment: on another day than
	// the paper's payment, or on no paper.
	OtherTaxes []Payout
}

// Payout is one payment of the year, before the tax: Minor of Currency on
// On, Withheld the tax rows of the same paper, day and currency took. Rate
// is the day's rate to the base; Known is false when there is none, and the
// sums in the base are then zero.
type Payout struct {
	On             time.Time
	Type           Type
	Instrument     *uuid.UUID
	Currency       string
	Minor          int64
	Withheld       int64
	Rate           decimal.Decimal
	Known          bool
	InBase         int64
	WithheldInBase int64
}

// Sale is one disposal: Proceeds and Fee in Currency at the rate of RateOn —
// the settlement day when known — and the lots given up, each at the rate of
// its purchase. Result is the gain in the base; Long, of it, what lots held
// longer than the years asked gained. Quantity is zero for an amortization.
// Known is false when a rate or a lot's day is missing; the sums in the base
// then claim nothing.
type Sale struct {
	On, RateOn     time.Time
	Instrument     uuid.UUID
	Quantity       decimal.Decimal
	Currency       string
	Proceeds, Fee  int64
	Rate           decimal.Decimal
	Known          bool
	ProceedsInBase int64
	FeeInBase      int64
	Lots           []SoldLot
	Result, Long   int64
}

// SoldLot is a lot's piece a disposal gave up: Cost of Currency, the
// position's, bought on AcquiredOn, at the rate of RateOn.
type SoldLot struct {
	AcquiredOn *time.Time
	RateOn     *time.Time
	Quantity   decimal.Decimal
	Currency   string
	Cost       int64
	Rate       decimal.Decimal
	Known      bool
	CostInBase int64
}

// YearRows reads the account's journal for year, line by line in currency.
func (s *Service) YearRows(ctx context.Context, spaceID, accountID uuid.UUID, year int, currency string, longYears int) (YearRows, error) {
	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return YearRows{}, err
	}
	positions, err := Compute(ops)
	if err != nil {
		return YearRows{}, journalDoesNotCompute{err}
	}
	memo := marketdata.NewRateMemo(s.conv)
	rate := func(from string, on time.Time) (decimal.Decimal, bool, error) {
		if from == currency {
			return decimal.NewFromInt(1), true, nil
		}
		r := memo.Rate(ctx, from, currency, on)
		if errors.Is(r.Err, marketdata.ErrNoRate) {
			return decimal.Zero, false, nil
		}
		return r.Rate, r.Err == nil, r.Err
	}
	in := func(minor int64, r decimal.Decimal) int64 {
		return decimal.NewFromInt(minor).Mul(r).Round(0).IntPart()
	}
	var out YearRows
	for _, p := range positions {
		for _, e := range p.Realizations {
			if e.OccurredOn.Year() != year {
				continue
			}
			sale, err := soldRows(e, p.InstrumentID, p.Currency, longYears, rate, in)
			if err != nil {
				return YearRows{}, err
			}
			out.Sales = append(out.Sales, sale)
		}
	}
	slices.SortStableFunc(out.Sales, func(a, b Sale) int { return a.On.Compare(b.On) })

	type key struct {
		paper    uuid.UUID
		on       time.Time
		currency string
	}
	paid := map[key]int{}
	var taxes []Operation
	for _, o := range ops {
		if o.OccurredOn.Year() != year {
			continue
		}
		switch o.Type {
		case TypeCoupon, TypeDividend, TypeInterest:
			if o.InstrumentID != nil {
				paid[key{*o.InstrumentID, o.OccurredOn, o.Currency}] = len(out.Payouts)
			}
			out.Payouts = append(out.Payouts, Payout{On: o.OccurredOn, Type: o.Type, Instrument: o.InstrumentID, Currency: o.Currency, Minor: o.AmountMinor})
		case TypeTax:
			taxes = append(taxes, o)
		}
	}
	for _, o := range taxes {
		// A tax row takes money: withheld is what it took.
		took := -o.AmountMinor
		if o.InstrumentID != nil {
			if i, ok := paid[key{*o.InstrumentID, o.OccurredOn, o.Currency}]; ok {
				out.Payouts[i].Withheld += took
				continue
			}
		}
		out.OtherTaxes = append(out.OtherTaxes, Payout{On: o.OccurredOn, Type: o.Type, Instrument: o.InstrumentID, Currency: o.Currency, Withheld: took})
	}
	for _, list := range [][]Payout{out.Payouts, out.OtherTaxes} {
		for i := range list {
			p := &list[i]
			r, ok, err := rate(p.Currency, p.On)
			if err != nil {
				return YearRows{}, err
			}
			if ok {
				p.Rate, p.Known, p.InBase, p.WithheldInBase = r, true, in(p.Minor, r), in(p.Withheld, r)
			}
		}
	}
	return out, nil
}

// soldRows is one disposal's lines: the proceeds and fee at the rate of the
// settlement day, each lot's piece at its own.
func soldRows(e Realization, paper uuid.UUID, positionCurrency string, longYears int,
	rate func(string, time.Time) (decimal.Decimal, bool, error), in func(int64, decimal.Decimal) int64,
) (Sale, error) {
	s := Sale{On: e.OccurredOn, RateOn: e.OccurredOn, Instrument: paper, Currency: e.Currency, Proceeds: e.ProceedsMinor, Fee: e.FeeMinor}
	if e.RateOn != nil {
		s.RateOn = *e.RateOn
	}
	r, ok, err := rate(e.Currency, s.RateOn)
	if err != nil {
		return Sale{}, err
	}
	s.Known = ok
	if ok {
		s.Rate, s.ProceedsInBase, s.FeeInBase = r, in(e.ProceedsMinor, r), in(e.FeeMinor, r)
	}
	s.Result = s.ProceedsInBase - s.FeeInBase
	since := e.OccurredOn.AddDate(-longYears, 0, 0)
	// What the proceeds less the fee bring on the long-held pieces, shared by
	// quantity as the estimate shares them.
	whole := decimal.Zero
	for _, l := range e.Released {
		whole = whole.Add(l.Quantity)
	}
	for _, l := range e.Released {
		lot := SoldLot{AcquiredOn: l.AcquiredOn, RateOn: l.RateOn, Quantity: l.Quantity, Currency: positionCurrency, Cost: l.CostMinor}
		s.Quantity = s.Quantity.Add(l.Quantity)
		paidOn := l.AcquiredOn
		if l.RateOn != nil {
			paidOn = l.RateOn
		}
		switch {
		case paidOn == nil && l.CostMinor != 0:
			// No day for its rate: the result cannot be told.
			s.Known = false
		case paidOn == nil:
			lot.Known = true
		default:
			r, ok, err := rate(positionCurrency, *paidOn)
			if err != nil {
				return Sale{}, err
			}
			if ok {
				lot.Rate, lot.Known, lot.CostInBase = r, true, in(l.CostMinor, r)
			} else {
				s.Known = false
			}
		}
		s.Result -= lot.CostInBase
		if longYears > 0 && l.AcquiredOn != nil && l.AcquiredOn.Before(since) && whole.IsPositive() {
			share := decimal.NewFromInt(s.ProceedsInBase - s.FeeInBase).Mul(l.Quantity).Div(whole).Round(0).IntPart()
			s.Long += share - lot.CostInBase
		}
		s.Lots = append(s.Lots, lot)
	}
	if !s.Known {
		s.Result, s.Long = 0, 0
	}
	return s, nil
}
