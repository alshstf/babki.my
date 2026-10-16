package creditcard

import (
	"time"

	"babki.my/babki/internal/platform/dates"
)

// workday is d, or the first working day after it when the card's
// deadlines move off days off.
func (t Terms) workday(d time.Time) time.Time {
	if !t.ShiftToWorkday {
		return d
	}
	return dates.Workday(d)
}
