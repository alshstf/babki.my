package operation

import (
	"testing"
	"time"
)

// Which of two rows folds after the other follows the one engine order — the
// broker's instant included — whichever accounts the rows are on: a move the
// broker reported a sync later but made at 10:00 does not fold after shares
// that arrived at 15:00, and so does not carry them onward.
func TestFoldsAfterReadsTheBrokersInstant(t *testing.T) {
	day := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	at := func(hour int) *time.Time { v := day.Add(time.Duration(hour) * time.Hour); return &v }
	arrival := Operation{OccurredOn: day, Source: "tinvest", OccurredAt: at(15), CreatedAt: day.Add(20 * time.Hour)}
	move := Operation{OccurredOn: day, Source: "tinvest", OccurredAt: at(10), CreatedAt: day.Add(30 * time.Hour)}
	if foldsAfter(move, arrival) {
		t.Error("the 10:00 move folds after the 15:00 arrival")
	}
	if !foldsAfter(arrival, move) {
		t.Error("the 15:00 arrival does not fold after the 10:00 move")
	}
}
