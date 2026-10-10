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
