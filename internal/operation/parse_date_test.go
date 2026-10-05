package operation

import (
	"testing"
	"time"
)

// parseDate returns midnight UTC. The engine compares acquisition and
// operation dates as instants, which orders lots correctly only because every
// date is midnight UTC; the DATE column guarantees that for stored rows, but
// Service.Create folds the parsed request before it is stored. A time of day or a
// zone here would make the acquisition-date tie-break depend on wall-clock time.
func TestParseDateProducesUTCMidnight(t *testing.T) {
	got, err := parseDate("2026-07-25")
	if err != nil {
		t.Fatalf("parseDate: %v", err)
	}
	if got.Location() != time.UTC {
		t.Errorf("location = %v, want UTC — a date carrying any other zone would silently break the acquisition-date tie-break in portfolio.acquiredBefore",
			got.Location())
	}
	if h, m, s := got.Clock(); h != 0 || m != 0 || s != 0 || got.Nanosecond() != 0 {
		t.Errorf("time-of-day = %02d:%02d:%02d.%09d, want midnight — a non-zero time-of-day would make portfolio.acquiredBefore compare wall-clock time instead of calendar day",
			h, m, s, got.Nanosecond())
	}
}
