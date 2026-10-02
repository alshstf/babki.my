package xirr

import (
	"math"
	"testing"
	"time"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

func TestRateMatchesKnownAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		flows []Flow
		want  float64
	}{
		// 1 000 grows to 1 100 in exactly a year: 10%.
		"one year": {[]Flow{{day("2025-01-01"), -1000}, {day("2026-01-01"), 1100}}, 0.10},
		// The spreadsheet's own documentation example (XIRR = 37.336%).
		"documentation": {[]Flow{
			{day("2008-01-01"), -10000},
			{day("2008-03-01"), 2750},
			{day("2008-10-30"), 4250},
			{day("2009-02-15"), 3250},
			{day("2009-04-01"), 2750},
		}, 0.373362535},
		// Half the money a year late earns the same 10% on what it was in for.
		"top-up": {[]Flow{
			{day("2025-01-01"), -1000}, {day("2026-01-01"), -1000}, {day("2027-01-01"), 1100*1.1 + 1100},
		}, 0.10},
		// A loss: 1 000 down to 800 in a year.
		"loss": {[]Flow{{day("2025-01-01"), -1000}, {day("2026-01-01"), 800}}, -0.20},
	} {
		got, ok := Rate(tc.flows)
		if !ok || math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("%s = %v, %v; want %v", name, got, ok, tc.want)
		}
	}
	for name, flows := range map[string][]Flow{
		"only in":  {{day("2025-01-01"), -1000}, {day("2026-01-01"), -10}},
		"only out": {{day("2025-01-01"), 1000}, {day("2026-01-01"), 10}},
		"one flow": {{day("2025-01-01"), -1000}},
	} {
		if got, ok := Rate(flows); ok {
			t.Errorf("%s gave %v, want no rate", name, got)
		}
	}
}
