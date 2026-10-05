// Package dates holds the date bounds shared by records entered by hand.
package dates

import "time"

// LatestRecordable is the newest date a hand-entered record may carry: one day
// past today in UTC. The extra day lets someone east of UTC record what they
// did this evening while the server's date is still yesterday.
func LatestRecordable() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
}

// EarliestRecordable is the oldest date a recorded event may carry. It guards
// against a mistyped year (1026 for 2026), which would otherwise sort to the
// front of the FIFO queue.
func EarliestRecordable() time.Time {
	return time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
}
