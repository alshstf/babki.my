package creditcard

import "time"

// The Russian calendar of working days, for a card whose deadlines move off
// days off (Terms.ShiftToWorkday, #452). A decreed year names its
// non-working weekdays — the holidays of the Labour Code, art. 112, with the
// days off moved onto weekdays — and its working weekend days; a year with no
// decree known falls back to the weekends and art. 112's holidays.
var decreed = map[int]struct{ off, on []string }{
	// Постановление Правительства РФ от 24.09.2025 № 1466: the days off of
	// 3 and 4 January moved to 9 January and 31 December.
	2026: {off: []string{
		"2026-01-01", "2026-01-02", "2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09",
		"2026-02-23", "2026-03-09", "2026-05-01", "2026-05-11", "2026-06-12", "2026-11-04", "2026-12-31",
	}},
	// Постановление Правительства РФ от 17.09.2026 № 1187: 2 January moved
	// to 5 November, 3 January to 31 December, Saturday 20 February to
	// Monday 22 February.
	2027: {off: []string{
		"2027-01-01", "2027-01-04", "2027-01-05", "2027-01-06", "2027-01-07", "2027-01-08",
		"2027-02-22", "2027-02-23", "2027-03-08", "2027-05-03", "2027-05-10", "2027-06-14",
		"2027-11-04", "2027-11-05", "2027-12-31",
	}, on: []string{"2027-02-20"}},
}

// holidays are the Labour Code's (art. 112), month and day.
var holidays = [][2]int{{1, 1}, {1, 2}, {1, 3}, {1, 4}, {1, 5}, {1, 6}, {1, 7}, {1, 8}, {2, 23}, {3, 8}, {5, 1}, {5, 9}, {6, 12}, {11, 4}}

var calendar = func() map[time.Time]bool {
	out := map[time.Time]bool{}
	for _, y := range decreed {
		for _, d := range y.off {
			out[must(d)] = true
		}
		for _, d := range y.on {
			out[must(d)] = false
		}
	}
	return out
}()

func must(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// dayOff says whether d is not a working day.
func dayOff(d time.Time) bool {
	if off, ok := calendar[d]; ok {
		return off
	}
	weekend := d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
	if _, ok := decreed[d.Year()]; ok {
		return weekend
	}
	for _, h := range holidays {
		if d.Month() == time.Month(h[0]) && d.Day() == h[1] {
			return true
		}
	}
	return weekend
}

// workday is d, or the first working day after it when the card's
// deadlines move off days off.
func (t Terms) workday(d time.Time) time.Time {
	if !t.ShiftToWorkday {
		return d
	}
	for dayOff(d) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}
