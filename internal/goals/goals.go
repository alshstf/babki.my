// Package goals keeps the family's savings goals and sets them against the
// money that stands for them (decision Р-44): a sum by a date, a contribution
// a month, an expected yield a year — each optional — and whole accounts or a
// sum out of one, valued as «Итого» values them. Tables: savings_goals,
// savings_goal_funds.
package goals

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Goal is a savings goal. Target, TargetOn and Monthly are each optional;
// Yield is the family's expected yield a year, in percent.
type Goal struct {
	ID       uuid.UUID
	Name     string
	Target   *int64
	TargetOn *time.Time
	Monthly  *int64
	Yield    decimal.Decimal
	Position int
	Funds    []Fund
}

// Fund is an account standing for a goal: whole, or Minor of it in the base
// currency.
type Fund struct {
	Account uuid.UUID
	Minor   *int64
}

// Status is where a goal stands against its target.
type Status string

const (
	// Done: what is saved reaches the target.
	Done Status = "done"
	// Ahead: by the date the plan comes to more than the target and a twentieth.
	Ahead Status = "ahead"
	// OnTrack: by the date the plan comes to the target.
	OnTrack Status = "on_track"
	// AtRisk: by the date the plan falls short.
	AtRisk Status = "at_risk"
	// NoStatus: no target, or neither a date nor a contribution to judge by.
	NoStatus Status = "none"
)

// Progress is a goal against its target today: Saved what its funds come to;
// with a date, Projected what the plan comes to by it and Needed the
// contribution a month that reaches the target; without a date but with a
// contribution, ReachOn the month it reaches the target.
type Progress struct {
	Saved     int64
	Status    Status
	Projected *int64
	Needed    *int64
	ReachOn   *time.Time
	Months    int
}

// monthsBetween is the whole months from from to to, a month not yet full
// not counted; 0 when to is not after from.
func monthsBetween(from, to time.Time) int {
	n := (to.Year()-from.Year())*12 + int(to.Month()) - int(from.Month())
	if to.Day() < from.Day() {
		n--
	}
	return max(n, 0)
}

var (
	twelve  = decimal.NewFromInt(12)
	hundred = decimal.NewFromInt(100)
)

// future is what saved and a contribution a month come to in n months at a
// yield a year, compounded monthly.
func future(saved, monthly int64, n int, yield decimal.Decimal) decimal.Decimal {
	p, m := decimal.NewFromInt(saved), decimal.NewFromInt(monthly)
	r := yield.Div(hundred).Div(twelve)
	if r.IsZero() {
		return p.Add(m.Mul(decimal.NewFromInt(int64(n))))
	}
	g := r.Add(decimal.NewFromInt(1)).Pow(decimal.NewFromInt(int64(n)))
	return p.Mul(g).Add(m.Mul(g.Sub(decimal.NewFromInt(1))).Div(r))
}

// progress sets a goal with saved against its target as of today.
func progress(g Goal, saved int64, today time.Time) Progress {
	out := Progress{Saved: saved, Status: NoStatus}
	if g.Target == nil {
		return out
	}
	target := *g.Target
	if saved >= target {
		out.Status = Done
		return out
	}
	monthly := int64(0)
	if g.Monthly != nil {
		monthly = *g.Monthly
	}
	switch {
	case g.TargetOn != nil:
		n := monthsBetween(today, *g.TargetOn)
		out.Months = n
		fv := future(saved, monthly, n, g.Yield).Round(0).IntPart()
		out.Projected = &fv
		needed := neededMonthly(saved, target, n, g.Yield)
		out.Needed = &needed
		switch {
		case decimal.NewFromInt(fv).GreaterThanOrEqual(decimal.NewFromInt(target).Mul(decimal.RequireFromString("1.05"))):
			out.Status = Ahead
		case fv >= target:
			out.Status = OnTrack
		default:
			out.Status = AtRisk
		}
	case monthly > 0:
		for n := 1; n <= 600; n++ {
			if future(saved, monthly, n, g.Yield).GreaterThanOrEqual(decimal.NewFromInt(target)) {
				on := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, n, 0)
				out.ReachOn, out.Months = &on, n
				break
			}
		}
	}
	return out
}

// neededMonthly is the contribution a month that brings saved to target in n
// months at the yield; all of what is short when no month is left.
func neededMonthly(saved, target int64, n int, yield decimal.Decimal) int64 {
	short := decimal.NewFromInt(target)
	if n <= 0 {
		return max(target-saved, 0)
	}
	r := yield.Div(hundred).Div(twelve)
	if r.IsZero() {
		return max(short.Sub(decimal.NewFromInt(saved)).Div(decimal.NewFromInt(int64(n))).Ceil().IntPart(), 0)
	}
	g := r.Add(decimal.NewFromInt(1)).Pow(decimal.NewFromInt(int64(n)))
	left := short.Sub(decimal.NewFromInt(saved).Mul(g))
	if !left.IsPositive() {
		return 0
	}
	return left.Mul(r).Div(g.Sub(decimal.NewFromInt(1))).Ceil().IntPart()
}
