// Package interest reckons what a savings account or a deposit earns by its
// rate (deposit-rates plan, step 6): day by day on the day's balance at the
// day's rate, paid at each month's end or at the term's end. The money
// forecast takes a savings account's monthly interest from it, the year's
// tax what the banks pay to the year's end — and since the year's start on
// the accounts kept by their balance.
package interest

import (
	"time"

	"github.com/shopspring/decimal"
)

// Point is the balance from On on, in the account's minor units.
type Point struct {
	On    time.Time
	Minor int64
}

// RateOn is an account's rate, a percent a year, on day with balance on it;
// ok is false without one.
type RateOn func(day time.Time, balance int64) (pct decimal.Decimal, ok bool, err error)

// Pays is how the bank pays the interest: Monthly on each month's last day,
// else all of it on Ends; Capitalize adds what is paid to the balance, to
// earn in turn. Ends is a deposit's last day — it earns nothing after —
// and zero for a savings account, which has no term.
type Pays struct {
	Monthly    bool
	Capitalize bool
	Ends       time.Time
}

// Payment is interest paid on On.
type Payment struct {
	On    time.Time
	Minor int64
}

var hundred = decimal.NewFromInt(100)

// Earned is what the balance earns from `from` to `to`, both days in, as the
// payments falling in them. Each day earns its balance — the last point on or
// before it, with what was capitalized since — times its rate over the
// year's days (365 or 366); the sum is rounded once, on the day it is paid,
// and what the last days earned toward a payment after `to` is not in.
// Unrated is the first day with money and no rate: the reckoning stops
// before it, the payments made by then kept.
func Earned(points []Point, rate RateOn, pays Pays, from, to time.Time) (out []Payment, unrated *time.Time, err error) {
	if !pays.Ends.IsZero() && pays.Ends.Before(to) {
		to = pays.Ends
	}
	accrued := decimal.Zero
	var base, added int64
	next := 0
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		for next < len(points) && !points[next].On.After(day) {
			// A new mark of the balance holds what was added before it.
			base, added = points[next].Minor, 0
			next++
		}
		if balance := base + added; balance > 0 {
			pct, ok, err := rate(day, balance)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				d := day
				return out, &d, nil
			}
			days := decimal.NewFromInt(int64(time.Date(day.Year(), time.December, 31, 0, 0, 0, 0, time.UTC).YearDay()))
			accrued = accrued.Add(decimal.NewFromInt(balance).Mul(pct).Div(hundred).Div(days))
		}
		monthEnd := day.AddDate(0, 0, 1).Day() == 1
		if pays.Monthly && monthEnd || day.Equal(pays.Ends) {
			if paid := accrued.Round(0).IntPart(); paid > 0 {
				out = append(out, Payment{On: day, Minor: paid})
				if pays.Capitalize {
					added += paid
				}
			}
			accrued = decimal.Zero
		}
	}
	return out, nil, nil
}
