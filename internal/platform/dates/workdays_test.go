package dates

import (
	"testing"
	"time"
)

// The calendar: the decreed days off and working Saturdays of 2026 and
// 2027, the Labour Code's holidays and weekends for a year with no decree.
func TestTheWorkingDaysFollowTheDecrees(t *testing.T) {
	for dayText, off := range map[string]bool{
		"2026-01-09": true,  // 3 January moved here
		"2026-01-12": false, // the first working day of 2026
		"2026-03-09": true,  // 8 March was a Sunday
		"2026-11-04": true,
		"2026-12-31": true,  // 4 January moved here
		"2027-02-20": false, // a working Saturday
		"2027-02-22": true,  // its day off
		"2027-11-05": true,  // 2 January moved here
		"2028-01-03": true,  // no decree: the Labour Code's holiday
		"2028-02-24": false,
		"2028-02-26": true, // a Saturday
	} {
		if got := DayOff(day(dayText)); got != off {
			t.Errorf("%s: day off = %v, want %v", dayText, got, off)
		}
	}
}

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}
