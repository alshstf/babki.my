// Package budget is the family's budget (decision Р-25, А): a limit a month
// on the spending categories that need one, the others only counted; a
// category with a «копилка» carries what it left unspent into the next month
// (a trip, presents). The spending is the money report's (internal/cashflow):
// in the base currency at each row's day, a category with its subcategories.
// The module keeps the limits only.
package budget

import (
	"time"

	"github.com/google/uuid"
)

// Limit is a category's limit a month from the month From (its first day)
// until a later limit of the category. Amount 0 without Rollover takes the
// limit off from From.
type Limit struct {
	CategoryID uuid.UUID
	From       time.Time
	Amount     int64
	Rollover   bool
}

// off says whether the limit takes the category's limit off.
func (l Limit) off() bool { return l.Amount == 0 && !l.Rollover }

// Line is a limited category's month: its limit, what its копилка carried in
// from the months before, what was spent, and what is left — below zero when
// overspent. Since is the month the limit in force was set from.
type Line struct {
	CategoryID uuid.UUID
	Limit      int64
	Rollover   bool
	Since      time.Time
	Carried    int64
	Spent      int64
	Left       int64
}

// Month is the budget of a month: the limited categories' lines in the
// categories' order; Planned their limits with what was carried, Spent and
// Left their sums — a subcategory limited under a limited parent counted in
// the parent's only; Unlimited the month's other spending.
type Month struct {
	Month        time.Time
	BaseCurrency string
	Lines        []Line
	Planned      int64
	Spent        int64
	Left         int64
	Unlimited    int64
	// MissingRates are the currencies of rows the report left out for want
	// of a rate on their day.
	MissingRates []string
}

// firstOfMonth is the first day of d's month.
func firstOfMonth(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// inForce is the category's limit in force in month m: the latest one from m
// or before; ok is false with none or one taking the limit off.
func inForce(limits []Limit, m time.Time) (Limit, bool) {
	var best Limit
	found := false
	for _, l := range limits {
		if !l.From.After(m) && (!found || l.From.After(best.From)) {
			best, found = l, true
		}
	}
	return best, found && !best.off()
}

// line works out a category's month m from its limits and what it spent each
// month from the first of months on (spent[i] is months[i]'s). The копилка
// fills month by month while the limit in force has one: what is left over
// goes on, an overspend does not.
func line(categoryID uuid.UUID, limits []Limit, months []time.Time, spent []int64, m time.Time) (Line, bool) {
	l, ok := inForce(limits, m)
	if !ok {
		return Line{}, false
	}
	out := Line{CategoryID: categoryID, Limit: l.Amount, Rollover: l.Rollover, Since: l.From}
	var pot int64
	for i, month := range months {
		if month.After(m) {
			break
		}
		cur, ok := inForce(limits, month)
		if !ok || !cur.Rollover {
			pot = 0
			if month.Equal(m) {
				out.Spent = spent[i]
			}
			continue
		}
		if month.Equal(m) {
			out.Carried, out.Spent = pot, spent[i]
			break
		}
		pot = max(pot+cur.Amount-spent[i], 0)
	}
	out.Left = out.Carried + out.Limit - out.Spent
	return out, true
}
