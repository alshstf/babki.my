package operation

import (
	"testing"
	"time"
)

// TestParseDateProducesUTCMidnight pins the convention
// portfolio.acquiredBefore quietly depends on without checking it:
// portfolio.Lot.AcquiredOn and portfolio.Operation.OccurredOn are compared as
// time.Time INSTANTS (a.Before(b)), which only orders lots correctly because
// every value that reaches the engine is, in fact, midnight UTC — never a
// moment with a time-of-day or a zone. Nothing in the type system enforces
// that; it is true only because of where these values come from.
//
// parseDate is one of the two places that convention is established (the
// other is the DATE column itself, which structurally cannot hold a
// time-of-day or a zone — Postgres drops both on write, so a round trip
// through the table always comes back midnight UTC regardless of what went
// in). parseDate is the one place the type system does NOT enforce it: a
// client-submitted string becomes a time.Time here, in memory, before it
// ever reaches a column. And it does so before persistence matters for
// ordering: Service.Create folds the not-yet-stored operation straight into
// portfolio.Compute (see checkJournalOps) to validate it, so a stray
// time-of-day or zone from this function would already be live at the one
// place acquiredBefore does its comparing, DATE column or not.
//
// If parseDate ever changed to something that CAN carry either — say
// time.ParseInLocation with a real *time.Location, or a layout that accepts
// a time — the acquisition-date tie-break engine.go documents would start
// silently depending on wall-clock time instead of calendar day, on
// whichever lots happened to be inserted around the boundary. Nothing would
// fail; a number destined for a tax return would just occasionally be wrong.
// This test is what would fail instead.
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
