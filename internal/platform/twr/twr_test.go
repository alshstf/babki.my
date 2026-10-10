package twr

import (
	"math"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

// 100 grows to 110 (+10 %); 1 000 more comes in that day; the 1 110 falls to
// 999 (−10 %). Time-weighted: 1.1 × 0.9 − 1 = −1 %, whatever the 1 000 did —
// though by money the family lost on most of it.
func TestTheHoldingsGrowthIsChainedPastTheMoney(t *testing.T) {
	r, ok := Rate([]Point{
		{Day: d("2026-01-01"), Worth: 100_00},
		{Day: d("2026-06-01"), Worth: 1_110_00, Flow: -1_000_00},
		{Day: d("2026-12-31"), Worth: 999_00},
	})
	if !ok || math.Abs(r-(-0.01)) > 1e-9 {
		t.Errorf("rate = %v, %v; want −1 %%", r, ok)
	}
}

// A stretch from nothing adds nothing: the first deposit starts the clock.
func TestNothingToGrowFromIsSkipped(t *testing.T) {
	r, ok := Rate([]Point{
		{Day: d("2026-01-01"), Worth: 0},
		{Day: d("2026-03-01"), Worth: 500_00, Flow: -500_00},
		{Day: d("2026-12-31"), Worth: 550_00},
	})
	if !ok || math.Abs(r-0.10) > 1e-9 {
		t.Errorf("rate = %v, %v; want 10 %%", r, ok)
	}
	if _, ok := Rate([]Point{{Day: d("2026-01-01")}, {Day: d("2026-12-31")}}); ok {
		t.Error("nothing at all has a rate")
	}
}

func TestAnHalfYearAnnualises(t *testing.T) {
	if got := Annual(0.10, d("2026-01-01"), d("2026-07-02")); math.Abs(got-(math.Pow(1.1, 365.0/182)-1)) > 1e-9 {
		t.Errorf("annual = %v", got)
	}
}

// The index of the holdings rises to 1.2, falls to 0.9, recovers to 1.1: the
// deepest fall is a quarter, from the second point to the third, whatever
// money came in on the way.
func TestTheDeepestFallIsFoundOnTheHoldingsOwnIndex(t *testing.T) {
	points := []Point{
		{Day: d("2026-01-31"), Worth: 100_00},
		{Day: d("2026-02-28"), Worth: 1_120_00, Flow: -1_000_00}, // 1.2, with 1 000 put in
		{Day: d("2026-03-31"), Worth: 840_00},                    // 0.9
		{Day: d("2026-04-30"), Worth: 1_026_67},                  // 1.1
	}
	index := Index(points)
	if math.Abs(index[1]-1.2) > 1e-9 || math.Abs(index[2]-0.9) > 1e-9 {
		t.Fatalf("index = %v", index)
	}
	depth, peak, bottom, ok := Drawdown(index)
	if !ok || math.Abs(depth-0.25) > 1e-9 || peak != 1 || bottom != 2 {
		t.Errorf("drawdown = %v from %d to %d (%v)", depth, peak, bottom, ok)
	}
	if _, _, _, ok := Drawdown([]float64{1, 1.1, 1.2}); ok {
		t.Error("an index that never fell has a drawdown")
	}
}

// Monthly changes of +2 %, −1 %, +3 %, 0 %: their spread a year.
func TestVolatilityIsTheMonthlySpreadPerYear(t *testing.T) {
	index := []float64{1, 1.02, 1.02 * 0.99, 1.02 * 0.99 * 1.03, 1.02 * 0.99 * 1.03, 1.05}
	ends := []bool{true, true, true, true, true, false}
	got, ok := Volatility(index, ends)
	changes := []float64{0.02, -0.01, 0.03, 0}
	mean := (0.02 - 0.01 + 0.03) / 4
	var sum float64
	for _, c := range changes {
		sum += (c - mean) * (c - mean)
	}
	want := math.Sqrt(sum/3) * math.Sqrt(12)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Errorf("volatility = %v (%v), want %v", got, ok, want)
	}
	if _, ok := Volatility(index[:3], ends[:3]); ok {
		t.Error("two months gave a volatility")
	}
}
