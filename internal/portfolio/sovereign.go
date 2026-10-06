package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// roubles is the currency НК РФ ст. 214.1 п. 13 restates into.
const roubles = "RUB"

// restateAtSaleRate counts a bond of Russia's external loans as a Russian tax
// resident's tax does (НК РФ ст. 214.1 п. 13, decision Р-21): each parcel's
// cost in the bond's face currency, in roubles at the rate of the day the bond
// was sold or repaid — today's for what is still held, so the result does not
// jump on the day of the sale. It changes p in place and reports whether it did.
//
//   - Held in roubles (bought for roubles on the exchange, as these trade now):
//     each cost becomes cost × rate(day of sale) ÷ rate(day paid), the norm's
//     second paragraph; the position's own figures are then the tax's.
//   - Held in the face currency with roubles as the base: the costs stand and
//     their rouble terms are dated by the sale, the norm's first paragraph.
//
// Anything else is left alone. A missing rate leaves the whole position alone
// rather than mixing the two rules in one figure.
func (s *Service) restateAtSaleRate(ctx context.Context, p *Position, face, base string, now time.Time,
	rates *marketdata.RateMemo,
) (bool, error) {
	switch {
	case face == "" || face == roubles:
		return false, nil
	case p.Currency == face && base == roubles:
		for i := range p.Lots {
			p.Lots[i].RateOn = &now
		}
		for i := range p.Realizations {
			saleDay := p.Realizations[i].rateDay()
			for j := range p.Realizations[i].Released {
				p.Realizations[i].Released[j].RateOn = &saleDay
			}
		}
		return true, nil
	case p.Currency != roubles:
		return false, nil
	}

	restate := func(cost int64, paid *time.Time, sold time.Time) (int64, bool, error) {
		if paid == nil || cost == 0 {
			return cost, cost == 0, nil
		}
		then := rates.Rate(ctx, face, roubles, *paid)
		later := rates.Rate(ctx, face, roubles, sold)
		for _, rl := range []marketdata.RateResult{then, later} {
			if errors.Is(rl.Err, marketdata.ErrNoRate) {
				return 0, false, nil
			}
			if rl.Err != nil {
				return 0, false, rl.Err
			}
		}
		minor, err := money.Minor(decimal.NewFromInt(cost).Mul(later.Rate).Div(then.Rate))
		if err != nil {
			return 0, false, fmt.Errorf("%w: a cost of %d restated from %s to %s", err, cost, paid.Format(time.DateOnly), sold.Format(time.DateOnly))
		}
		return minor, true, nil
	}
	lots := make([]int64, len(p.Lots))
	for i, l := range p.Lots {
		minor, ok, err := restate(l.CostMinor, lotRateDay(l.AcquiredOn, l.RateOn), now)
		if err != nil || !ok {
			return false, err
		}
		lots[i] = minor
	}
	released := make([][]int64, len(p.Realizations))
	for i, r := range p.Realizations {
		released[i] = make([]int64, len(r.Released))
		for j, pc := range r.Released {
			minor, ok, err := restate(pc.CostMinor, lotRateDay(pc.AcquiredOn, pc.RateOn), r.rateDay())
			if err != nil || !ok {
				return false, err
			}
			released[i][j] = minor
		}
	}

	var held int64
	for i := range p.Lots {
		p.Lots[i].CostMinor = lots[i]
		held += lots[i]
	}
	p.CostMinor = held
	for i := range p.Realizations {
		for j := range p.Realizations[i].Released {
			p.Realizations[i].Released[j].CostMinor = released[i][j]
		}
	}
	return true, p.finishRealized()
}

// lotRateDay is the day whose rate prices a parcel's cost, or nil for one with
// no purchase day.
func lotRateDay(acquired, rateOn *time.Time) *time.Time {
	if rateOn != nil {
		return rateOn
	}
	return acquired
}

// rateDay is the day the disposal's income came in: its settlement day when
// known, else its own.
func (r Realization) rateDay() time.Time {
	if r.RateOn != nil {
		return *r.RateOn
	}
	return r.OccurredOn
}
