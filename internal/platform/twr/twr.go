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

// Index is the holdings' own growth at each point, the first at 1: each
// stretch's growth (as Rate takes it) chained. A stretch from nothing keeps
// the index where it was.
func Index(points []Point) []float64 {
	out := make([]float64, len(points))
	if len(points) == 0 {
		return out
	}
	out[0] = 1
	for i := 1; i < len(points); i++ {
		out[i] = out[i-1]
		start := float64(points[i-1].Worth)
		if start <= 0 {
			continue
		}
		g := (float64(points[i].Worth) + float64(points[i].Flow)) / start
		if g > 0 && !math.IsInf(g, 0) && !math.IsNaN(g) {
			out[i] = out[i-1] * g
		}
	}
	return out
}

// Drawdown is the deepest fall of the index from a peak before it: the share
// lost (0.12 for 12 %), and the points of the peak and of the bottom. ok is
// false when the index never fell.
func Drawdown(index []float64) (depth float64, peak, bottom int, ok bool) {
	top := 0
	for i := range index {
		if index[i] > index[top] {
			top = i
		}
		if index[top] <= 0 {
			continue
		}
		if fall := 1 - index[i]/index[top]; fall > depth {
			depth, peak, bottom, ok = fall, top, i, true
		}
	}
	return depth, peak, bottom, ok
}

// Volatility is the yearly spread of the index's monthly changes: the
// standard deviation of month-on-month growth over the points that close a
// month, times √12. ok is false with fewer than three months.
func Volatility(index []float64, monthEnds []bool) (float64, bool) {
	var changes []float64
	last := -1
	for i := range index {
		if !monthEnds[i] {
			continue
		}
		if last >= 0 && index[last] > 0 {
			changes = append(changes, index[i]/index[last]-1)
		}
		last = i
	}
	if len(changes) < 3 {
		return 0, false
	}
	mean := 0.0
	for _, c := range changes {
		mean += c
	}
	mean /= float64(len(changes))
	sum := 0.0
	for _, c := range changes {
		sum += (c - mean) * (c - mean)
	}
	return math.Sqrt(sum/float64(len(changes)-1)) * math.Sqrt(12), true
}

// Performance is what one valued series says beyond the money-weighted rate:
// the time-weighted rate over the period, the deepest fall of the holdings'
// own index with its peak and bottom days, and the yearly volatility of its
// monthly changes. Each Has… is false when it cannot be told.
type Performance struct {
	Period        float64
	HasRate       bool
	Drawdown      float64
	Peak, Bottom  time.Time
	HasDrawdown   bool
	Volatility    float64
	HasVolatility bool
}

// Days are the days a period's performance is valued on: its start, the last
// day of every month within it, each day money crossed the edge, and its end;
// MonthEnds marks the start and the months' last days.
func Days(from, to time.Time, flowDays []time.Time) (days []time.Time, monthEnds []bool) {
	marked := map[time.Time]bool{from: true}
	all := map[time.Time]bool{from: true, to: true}
	for m := time.Date(from.Year(), from.Month()+1, 0, 0, 0, 0, 0, time.UTC); m.Before(to); m = time.Date(m.Year(), m.Month()+2, 0, 0, 0, 0, 0, time.UTC) {
		if m.After(from) {
			all[m], marked[m] = true, true
		}
	}
	for _, d := range flowDays {
		if d.After(from) && !d.After(to) {
			all[d] = true
		}
	}
	for d := range all {
		days = append(days, d)
	}
	sortDays(days)
	monthEnds = make([]bool, len(days))
	for i, d := range days {
		monthEnds[i] = marked[d]
	}
	return days, monthEnds
}

func sortDays(days []time.Time) {
	for i := 1; i < len(days); i++ {
		for j := i; j > 0 && days[j].Before(days[j-1]); j-- {
			days[j], days[j-1] = days[j-1], days[j]
		}
	}
}

// Measure works the performance out of points valued on Days.
func Measure(points []Point, monthEnds []bool) Performance {
	var p Performance
	p.Period, p.HasRate = Rate(points)
	index := Index(points)
	if depth, peak, bottom, ok := Drawdown(index); ok {
		p.Drawdown, p.Peak, p.Bottom, p.HasDrawdown = depth, points[peak].Day, points[bottom].Day, true
	}
	p.Volatility, p.HasVolatility = Volatility(index, monthEnds)
	return p
}
