// Package compare compares the banks' deposits and savings accounts on a
// sum over a horizon (decision Р-57 Б): what each brings by its terms, under
// the key rate's scenario and the conditions the family says it meets, what
// the person keeps after the tax on interest and what of the sum deposit
// insurance covers.
package compare

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/interest"
)

// overshoot is how far past the horizon a deposit's last term may run and
// still be opened: a year's deposit is 366 or 367 days at many banks.
const overshoot = 31

// Earning is what a variant brings on the sum from the day it is opened: the
// term chosen (none for a savings account), the way of payment, the interest
// paid — by day, for the tax of each year — and the days it is for.
type Earning struct {
	Term       int
	Payout     deposit.Payout
	Payments   []interest.Payment
	Income     int64
	Days       int
	Prolonged  int
	TermsTried int
}

// errNoRate: some day of the horizon has no rate by the variant's terms.
var errNoRate = errors.New("compare: no rate for a day")

// earn is what the variant earns on amount from start, when it is opened, to
// end, both in: a savings account day by day, its interest added each month;
// a deposit at the term of its grid that brings the most a year — prolonged
// at the terms of the day while the horizon lasts, its last term running at
// most a month past it. keyOn and met are the scenario's key rate and the
// conditions as reckoned.
func earn(p deposit.Programme, v deposit.Variant, amount int64, start, end time.Time, keyOn deposit.KeyOn, met deposit.Conditions) (Earning, error) {
	if p.Kind == deposit.KindSavings {
		h := deposit.Holding{Variant: v.ID, OpenedOn: start}
		paid, err := run(p, v, h, amount, start, end, interest.Pays{Monthly: true, Capitalize: true, Ends: end}, keyOn, met)
		if err != nil {
			return Earning{}, err
		}
		return Earning{Payout: deposit.PayoutCapitalize, Payments: paid, Income: sum(paid), Days: days(start, end)}, nil
	}
	way := payout(v)
	var best Earning
	found := false
	for _, term := range terms(v, amount, start, end) {
		e, err := prolonged(p, v, way, term, amount, start, end, keyOn, met)
		if errors.Is(err, errNoRate) || errors.Is(err, deposit.ErrNoCell) {
			continue
		}
		if err != nil {
			return Earning{}, err
		}
		if !found || better(e, best) {
			best, found = e, true
		}
	}
	if !found {
		return Earning{}, errNoRate
	}
	return best, nil
}

// prolonged is a deposit of term opened on start and prolonged at the terms
// of each day it ends, while the horizon lasts.
func prolonged(p deposit.Programme, v deposit.Variant, way deposit.Payout, term int, amount int64, start, end time.Time,
	keyOn deposit.KeyOn, met deposit.Conditions,
) (Earning, error) {
	out := Earning{Term: term, Payout: way}
	principal, at := amount, start
	limit := end.AddDate(0, 0, overshoot)
	for at.Before(end) {
		ends := at.AddDate(0, 0, term)
		if v.TermUnit == deposit.TermMonths {
			ends = at.AddDate(0, term, 0)
		}
		if ends.After(limit) {
			break
		}
		h := deposit.Holding{Variant: v.ID, OpenedOn: at, Term: term, Amount: principal}
		pays := interest.Pays{Ends: ends, Monthly: way != deposit.PayoutEnd, Capitalize: way == deposit.PayoutCapitalize}
		paid, err := run(p, v, h, principal, at, ends, pays, keyOn, met)
		if err != nil {
			return Earning{}, err
		}
		out.Payments = append(out.Payments, paid...)
		got := sum(paid)
		out.Income += got
		if way != deposit.PayoutMonthly {
			// Paid at the end or added monthly: it stays on the deposit and
			// is prolonged with it; paid monthly onto a card, it does not.
			principal += got
		}
		out.Days = days(start, ends)
		if at != start {
			out.Prolonged++
		}
		at = ends
	}
	if out.Days == 0 {
		return Earning{}, errNoRate
	}
	return out, nil
}

// run is the interest of one holding from opening to the stretch's end, its
// rate struck once a month and at the schedule's boundaries — what the rate
// moves with — rather than every day.
func run(p deposit.Programme, v deposit.Variant, h deposit.Holding, amount int64, opened, end time.Time, pays interest.Pays,
	keyOn deposit.KeyOn, met deposit.Conditions,
) ([]interest.Payment, error) {
	bounds := boundaries(v, opened)
	type key struct {
		month   string
		stage   int
		balance int64
	}
	memo := map[key]decimal.Decimal{}
	rate := func(day time.Time, balance int64) (decimal.Decimal, bool, error) {
		k := key{day.Format("2006-01"), stage(bounds, day), balance}
		if r, ok := memo[k]; ok {
			return r, true, nil
		}
		q, err := p.Quote(h, day, balance, keyOn, met)
		if err != nil {
			if errors.Is(err, deposit.ErrNoCell) || errors.Is(err, deposit.ErrNoEdition) || errors.Is(err, deposit.ErrNoKeyRate) || errors.Is(err, deposit.ErrOpened) {
				return decimal.Zero, false, nil
			}
			return decimal.Zero, false, err
		}
		memo[k] = q.Percent
		return q.Percent, true, nil
	}
	paid, unrated, err := interest.Earned([]interest.Point{{On: opened, Minor: amount}}, rate, pays, opened.AddDate(0, 0, 1), end)
	if err != nil {
		return nil, err
	}
	if unrated != nil {
		return nil, fmt.Errorf("%w: %s", errNoRate, unrated.Format(time.DateOnly))
	}
	return paid, nil
}

// boundaries are the days the variant's schedule moves from one period to
// the next, for a holding opened on opened.
func boundaries(v deposit.Variant, opened time.Time) []time.Time {
	var out []time.Time
	at := opened
	for _, s := range v.Schedule {
		next := at.AddDate(0, 0, s.Days)
		if s.Months > 0 {
			next = time.Date(at.Year(), at.Month()+time.Month(s.Months), 1, 0, 0, 0, 0, time.UTC)
		}
		out = append(out, next)
		at = next
	}
	return out
}

func stage(bounds []time.Time, day time.Time) int {
	n := 0
	for _, b := range bounds {
		if !day.Before(b) {
			n++
		}
	}
	return n
}

// terms are the deposit's terms to try on amount: of each range of its grid
// the bank has a rate for on the sum, the longest that fits the horizon —
// the range's end, else the horizon itself when within the range.
func terms(v deposit.Variant, amount int64, start, end time.Time) []int {
	span := days(start, end)
	if v.TermUnit == deposit.TermMonths {
		span = months(start, end)
	}
	var out []int
	for _, c := range v.Grid {
		if c.AmountFrom > amount || c.From <= 0 {
			continue
		}
		t := c.To
		if t > span {
			t = span
		}
		if t < c.From {
			// Shorter than the range: only its first term, if it runs at
			// most a month past the horizon.
			t = c.From
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// payout is the way the deposit pays that brings the most: added to it,
// else at the end, else monthly onto a card.
func payout(v deposit.Variant) deposit.Payout {
	for _, w := range []deposit.Payout{deposit.PayoutCapitalize, deposit.PayoutEnd, deposit.PayoutMonthly} {
		if slices.Contains(v.Payouts, w) {
			return w
		}
	}
	return deposit.PayoutMonthly
}

// better: more a day, else the shorter term.
func better(a, b Earning) bool {
	x := decimal.NewFromInt(a.Income).Mul(decimal.NewFromInt(int64(b.Days)))
	y := decimal.NewFromInt(b.Income).Mul(decimal.NewFromInt(int64(a.Days)))
	if c := x.Cmp(y); c != 0 {
		return c > 0
	}
	return a.Term < b.Term
}

func sum(ps []interest.Payment) int64 {
	var out int64
	for _, p := range ps {
		out += p.Minor
	}
	return out
}

func days(start, end time.Time) int { return int(end.Sub(start).Hours() / 24) }

func months(start, end time.Time) int {
	m := (end.Year()-start.Year())*12 + int(end.Month()-start.Month())
	if end.Day() < start.Day() {
		m--
	}
	return max(m, 0)
}
