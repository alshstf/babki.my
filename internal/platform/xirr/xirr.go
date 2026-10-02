// Package xirr finds the annual rate of return of a series of dated cash
// flows — the rate at which their present values sum to nought, the way a
// spreadsheet's XIRR does. Flows are signed from the investor's side: money
// put in is negative, money taken out (and what is left at the end) positive.
package xirr

import (
	"math"
	"time"
)

// Flow is an amount on a day.
type Flow struct {
	Day    time.Time
	Amount float64
}

// Rate is the annual rate r for which Σ amount / (1+r)^(days/365) = 0, days
// counted from the first flow. ok is false when there is no such rate: all
// flows of one sign, or none that the search can find between −99.99% and
// 1 000 000%.
//
// The rate is found by bisection on the net present value, which falls as the
// rate rises for any series that puts money in before taking it out — the only
// kind this program asks about. It is slower than Newton's method and never
// wanders off.
func Rate(flows []Flow) (float64, bool) {
	if len(flows) < 2 {
		return 0, false
	}
	start := flows[0].Day
	for _, f := range flows {
		if f.Day.Before(start) {
			start = f.Day
		}
	}
	in, out := false, false
	for _, f := range flows {
		in = in || f.Amount < 0
		out = out || f.Amount > 0
	}
	if !in || !out {
		return 0, false
	}
	npv := func(r float64) float64 {
		sum := 0.0
		for _, f := range flows {
			years := f.Day.Sub(start).Hours() / 24 / 365
			sum += f.Amount / math.Pow(1+r, years)
		}
		return sum
	}
	lo, hi := -0.9999, 10000.0
	flo, fhi := npv(lo), npv(hi)
	if math.IsNaN(flo) || math.IsNaN(fhi) || flo*fhi > 0 {
		return 0, false
	}
	for range 200 {
		mid := (lo + hi) / 2
		fm := npv(mid)
		if fm == 0 || hi-lo < 1e-10 {
			return mid, true
		}
		if (fm > 0) == (flo > 0) {
			lo, flo = mid, fm
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, true
}
