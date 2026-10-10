// Package twr works out a time-weighted return (#405): how the holdings
// themselves did, whatever money came in or went out and when — beside the
// money-weighted rate (xirr), which does count when the money came.
package twr

import (
	"math"
	"time"
)

// Point is a portfolio's worth at the end of a day and the money that
// crossed its edge that day, signed from the investor's side: put in is
// negative, taken out positive (as a return's flows are).
type Point struct {
	Day   time.Time
	Worth int64
	Flow  int64
}

// Rate chains the growth of each stretch between consecutive points: the
// worth at its end, the day's money taken back out of it, over the worth at
// its start. The money is taken to have come at the end of its day, earning
// nothing yet. A stretch from nothing — before the first deposit — adds
// nothing. ok is false when no stretch had a worth to grow from.
func Rate(points []Point) (float64, bool) {
	growth, counted := 1.0, false
	for i := 1; i < len(points); i++ {
		start := float64(points[i-1].Worth)
		if start <= 0 {
			continue
		}
		end := float64(points[i].Worth) + float64(points[i].Flow)
		growth *= end / start
		counted = true
	}
	if !counted || growth <= 0 || math.IsNaN(growth) || math.IsInf(growth, 0) {
		return 0, false
	}
	return growth - 1, true
}

// Annual turns a period's rate into a year's, compounding.
func Annual(period float64, from, to time.Time) float64 {
	days := to.Sub(from).Hours() / 24
	if days <= 0 {
		return period
	}
	return math.Pow(1+period, 365/days) - 1
}
