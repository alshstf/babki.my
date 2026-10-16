package deposit

import (
	"errors"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/dates"
)

// Holding is an account of a programme: the variant chosen, the day it was
// opened (a deposit's last prolongation), its term in the variant's unit and
// the sum it was opened with.
type Holding struct {
	Variant  string
	OpenedOn time.Time
	Term     int
	Amount   int64
}

// Conditions tell what the family met in a month: for a bonus (by its id)
// the index of the highest level met — 0 for a bonus of one; for a period's
// requirement (by its name) ok alone counts. The marks of the manual mode or
// the program's own reckoning of the automatic one (Р-53).
type Conditions func(id string, month time.Time) (level int, ok bool)

// KeyOn is the key rate in force on a day.
type KeyOn func(day time.Time) (decimal.Decimal, bool, error)

// Part is a part of the balance, From to To (minor units), and its rate.
type Part struct {
	From, To int64
	Percent  decimal.Decimal
}

// Quote is the rate a year on a day: Percent on the whole balance, weighed
// by its Parts; the edition and the period of the schedule it came from and
// the bonuses that counted.
type Quote struct {
	Percent decimal.Decimal
	Parts   []Part
	Edition string
	Period  string
	Bonuses []string
}

var (
	ErrNoEdition = errors.New("deposit: the catalog has no terms of the programme for the day")
	ErrNoVariant = errors.New("deposit: no such variant in the edition")
	ErrNoCell    = errors.New("deposit: the edition has no rate for the term and sum")
	ErrNoKeyRate = errors.New("deposit: the key rate of the day is not known")
)

// Quote is the rate a year the holding earns on day with balance on the
// account. A deposit keeps the edition of the day it was opened, a savings
// account takes the edition of each day.
func (p Programme) Quote(h Holding, day time.Time, balance int64, keyOn KeyOn, met Conditions) (Quote, error) {
	if met == nil {
		met = func(string, time.Time) (int, bool) { return 0, false }
	}
	if keyOn == nil {
		keyOn = func(time.Time) (decimal.Decimal, bool, error) { return decimal.Zero, false, nil }
	}
	on := day
	if p.Kind == KindDeposit {
		on = h.OpenedOn
	}
	e, ok := p.editionOn(on)
	if !ok {
		return Quote{}, ErrNoEdition
	}
	i := slices.IndexFunc(e.Variants, func(v Variant) bool { return v.ID == h.Variant })
	if i < 0 {
		return Quote{}, ErrNoVariant
	}
	v := e.Variants[i]
	key := func() (decimal.Decimal, error) {
		k, ok, err := keyOn(keyDay(day, v.Key))
		if err != nil {
			return decimal.Zero, err
		}
		if !ok {
			return decimal.Zero, ErrNoKeyRate
		}
		return k, nil
	}
	period, inPeriod := v.periodOn(h.OpenedOn, day, met)
	balance = max(balance, 0)
	out := Quote{Edition: e.From}
	if inPeriod {
		out.Period = period.Name
	}
	counted := map[string]bool{}
	var weighed decimal.Decimal
	bounds := v.bounds(period, inPeriod, balance)
	for j := range bounds {
		from, to := bounds[j], balance
		if j+1 < len(bounds) {
			to = bounds[j+1]
		}
		whole := h.Amount
		if p.Kind == KindSavings {
			whole = balance
		}
		rate, err := v.rateOf(v.Grid, v.Split, h.Term, whole, from, p.Kind, key)
		if err != nil {
			return Quote{}, err
		}
		bonuses := true
		if inPeriod && (period.Cap == 0 || from < period.Cap) {
			bonuses = period.Bonuses
			if len(period.Grid) > 0 {
				if rate, err = v.rateOf(period.Grid, v.Split, h.Term, whole, from, p.Kind, key); err != nil {
					return Quote{}, err
				}
			} else {
				rate = rate.Add(decimal.RequireFromString(period.Plus))
			}
		}
		if bonuses {
			plus, ids := v.bonusOn(day, from, met)
			rate = rate.Add(plus)
			for _, id := range ids {
				counted[id] = true
			}
		}
		if v.Cap != nil && from >= v.Cap.Amount {
			if v.Cap.Percent != "" {
				rate = decimal.RequireFromString(v.Cap.Percent)
			} else {
				rate = rate.Mul(decimal.RequireFromString(v.Cap.Share))
			}
		}
		rate = decimal.Max(rate, decimal.Zero)
		out.Parts = append(out.Parts, Part{From: from, To: to, Percent: rate})
		weighed = weighed.Add(rate.Mul(decimal.NewFromInt(to - from)))
	}
	if balance > 0 {
		out.Percent = weighed.Div(decimal.NewFromInt(balance)).Round(6)
	} else {
		out.Percent = out.Parts[0].Percent
	}
	for _, b := range v.Bonuses {
		if counted[b.ID] {
			out.Bonuses = append(out.Bonuses, b.ID)
		}
	}
	return out, nil
}

// editionOn is the edition in force on day: the last from it or before.
func (p Programme) editionOn(day time.Time) (Edition, bool) {
	at := day.Format(time.DateOnly)
	for i := len(p.Editions) - 1; i >= 0; i-- {
		if p.Editions[i].From <= at {
			return p.Editions[i], true
		}
	}
	return Edition{}, false
}

// periodOn is the period of the schedule day falls in, the periods running
// one after another from opening; a period whose requirement was not met at
// opening is passed over as if it were the variant's own rate.
func (v Variant) periodOn(opened, day time.Time, met Conditions) (Period, bool) {
	start := opened
	for _, s := range v.Schedule {
		end := start.AddDate(0, 0, s.Days)
		if s.Months > 0 {
			end = time.Date(start.Year(), start.Month()+time.Month(s.Months), 1, 0, 0, 0, 0, time.UTC)
		}
		if day.Before(end) {
			if s.Requires != "" {
				if _, ok := met(s.Requires, monthOf(opened)); !ok {
					return Period{}, false
				}
			}
			return s, true
		}
		start = end
	}
	return Period{}, false
}

// bounds are where the rate of a part of the balance may change: the steps
// of a split grid and the caps of the period, the bonuses and the variant.
func (v Variant) bounds(period Period, inPeriod bool, balance int64) []int64 {
	out := []int64{0}
	add := func(at int64) {
		if at > 0 && at < balance && !slices.Contains(out, at) {
			out = append(out, at)
		}
	}
	if v.Split {
		for _, c := range v.Grid {
			add(c.AmountFrom)
		}
		for _, c := range period.Grid {
			add(c.AmountFrom)
		}
	}
	if inPeriod {
		add(period.Cap)
	}
	for _, b := range v.Bonuses {
		add(b.Cap)
	}
	if v.Cap != nil {
		add(v.Cap.Amount)
	}
	slices.Sort(out)
	return out
}

// rateOf is the grid's rate for the term and sum: the step the whole sum
// reaches, or — split — the step of the part from from.
func (v Variant) rateOf(grid []Cell, split bool, term int, whole, from int64, kind Kind, key func() (decimal.Decimal, error)) (decimal.Decimal, error) {
	amount := whole
	if split {
		amount = from
	}
	var cell *Cell
	for i := range grid {
		c := &grid[i]
		if kind == KindDeposit && (term < c.From || term > c.To) {
			continue
		}
		if c.AmountFrom <= amount && (cell == nil || c.AmountFrom >= cell.AmountFrom) {
			cell = c
		}
	}
	if cell == nil {
		return decimal.Zero, ErrNoCell
	}
	return cell.at(key)
}

// at is the rate's percent: fixed, or the key rate times KeyTimes plus
// KeyPlus, not below Floor.
func (r Rate) at(key func() (decimal.Decimal, error)) (decimal.Decimal, error) {
	if r.Percent != "" {
		return decimal.RequireFromString(r.Percent), nil
	}
	k, err := key()
	if err != nil {
		return decimal.Zero, err
	}
	times, plus := decimal.NewFromInt(1), decimal.Zero
	if r.KeyTimes != "" {
		times = decimal.RequireFromString(r.KeyTimes)
	}
	if r.KeyPlus != "" {
		plus = decimal.RequireFromString(r.KeyPlus)
	}
	out := k.Mul(times).Add(plus)
	if r.Floor != "" {
		out = decimal.Max(out, decimal.RequireFromString(r.Floor))
	}
	return out, nil
}

// bonusOn is what the bonuses add on day to the part of the balance from
// from: the greatest of each group, those whose cap the part is under.
func (v Variant) bonusOn(day time.Time, from int64, met Conditions) (decimal.Decimal, []string) {
	best := map[string]decimal.Decimal{}
	bestID := map[string]string{}
	for _, b := range v.Bonuses {
		if b.Cap > 0 && from >= b.Cap {
			continue
		}
		month := monthOf(day)
		if b.NextMonth {
			month = month.AddDate(0, -1, 0)
		}
		level, ok := met(b.ID, month)
		if !ok {
			continue
		}
		plus := b.Plus
		if len(b.Levels) > 0 {
			plus = b.Levels[min(max(level, 0), len(b.Levels)-1)].Plus
		}
		group := b.Group
		if group == "" {
			group = "#" + b.ID
		}
		if d := decimal.RequireFromString(plus); d.GreaterThan(best[group]) || bestID[group] == "" {
			best[group], bestID[group] = d, b.ID
		}
	}
	sum := decimal.Zero
	var ids []string
	for g, d := range best {
		sum = sum.Add(d)
		ids = append(ids, bestID[g])
	}
	return sum, ids
}

// keyDay is the day whose key rate counts on day: the same, or — with a lag
// — before the changes that have not reached the deposit yet.
func keyDay(day time.Time, lag *KeyLag) time.Time {
	switch {
	case lag == nil:
		return day
	case lag.NextWorkday:
		// A change counts from the working day after it: on day, those up
		// to the eve of the last working day.
		last := day
		for dates.DayOff(last) {
			last = last.AddDate(0, 0, -1)
		}
		return last.AddDate(0, 0, -1)
	}
	start := monthOf(day)
	prevEnd := start.AddDate(0, 0, -1)
	switch {
	case !day.Before(nthWorkday(start, lag.LateWorkday)):
		return prevEnd
	case !day.Before(nthWorkday(start, lag.Workday)):
		return time.Date(prevEnd.Year(), prevEnd.Month(), lag.Cutoff, 0, 0, 0, 0, time.UTC)
	}
	return monthOf(prevEnd).AddDate(0, 0, -1)
}

// nthWorkday is the n-th working day of the month starting on start.
func nthWorkday(start time.Time, n int) time.Time {
	d := dates.Workday(start)
	for i := 1; i < n; i++ {
		d = dates.Workday(d.AddDate(0, 0, 1))
	}
	return d
}

func monthOf(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
}
