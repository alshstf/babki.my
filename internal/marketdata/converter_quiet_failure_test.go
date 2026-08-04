package marketdata

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// This file covers #97: the OTHER way a batch can die, and the one nothing was
// watching.
//
// resolveQueries returns errNotPrefetched when the resolution asks the prefetch
// for a triple the enumeration never recorded. RatesOn hands that back beside
// the zero Rates; the three HTTP handlers drop it on the floor by design (an
// error page is a worse outcome than a slow correct one); the memo therefore
// stays cold and EVERY figure on the page falls back to a lookup of its own.
// Symptom for symptom that is #70's dead batch — right, slow, and silent.
//
// The cause is not. A dead batch is the database having a bad day; this is the
// two halves of RatesOn disagreeing about what resolution consults, which is a
// bug in converter.go itself. Nothing will arrive to fix it and the next request
// degrades exactly as this one did, so the level is ERROR rather than the dead
// batch's WARN. It is the same distinction money.ErrOverflow draws against a
// missing rate: data that is absent will come back, code that is wrong will not.
//
// The test lives INSIDE the package because the failure cannot be reached from
// outside it. Both passes of RatesOn run the same resolution over the same
// enumeration, and the first pass finds nothing ever, so it walks every branch
// the second can take — the second consulting an unrecorded key is unreachable
// by construction, which is precisely why it is an assertion rather than a
// handled case. Reaching it means calling resolveQueries with a prefetch that
// was never filled, which is what this does.

// uncoveredQueryMessage is the line an operator greps for. Named here rather
// than matched loosely, so a silent rename is a red test with a list of what WAS
// logged rather than a test that quietly stops watching anything.
const uncoveredQueryMessage = "fx rate prefetch did not cover the resolution"

// TestResolveQueriesLogsAnUncoveredQueryAsAnError pins the level, not the
// existence of a line. WARN would file a bug in this file under the same heading
// as a database having a bad day, and DEBUG would not be printed at all at the
// level this application runs at — which is the silence the whole issue is
// about.
func TestResolveQueriesLogsAnUncoveredQueryAsAnError(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	queries := []RateQuery{{From: "USD", To: "EUR", On: on}}
	capture := CaptureLogs(t)

	// An empty prefetch: nothing was asked for, so the first row resolution
	// reaches for is one nobody recorded.
	got, err := resolveQueries(ctx, prefetchedRows{}, queries)
	if err == nil {
		t.Fatalf("resolveQueries over an empty prefetch: err = nil, want the uncovered-query error")
	}
	if got.Len() != 0 {
		t.Fatalf("resolveQueries over an empty prefetch returned %d entries, want the zero Rates", got.Len())
	}
	// The cause names the pair, because an operator holding only "something was
	// not prefetched" has a bug report with no way in.
	AssertOneRecordAt(t, capture, uncoveredQueryMessage, slog.LevelError, "USD")
}

// TestResolveQueriesSaysNothingWhenThePrefetchCovers is the other half of the
// claim, and the one that keeps the line worth reading: an ordinary batch —
// including one where a pair genuinely has no rate — writes nothing at all. A
// line that appeared on healthy calls would be noise inside a week, and #97 is
// about a line being noticed.
func TestResolveQueriesSaysNothingWhenThePrefetchCovers(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	queries := []RateQuery{
		{From: "USD", To: "EUR", On: on},
		{From: "USD", To: "USD", On: on},
	}
	candidates := &recordingRows{}
	for _, q := range queries {
		_, _, _ = rateVia(ctx, candidates, q.From, q.To, q.On)
	}
	capture := CaptureLogs(t)

	if _, err := resolveQueries(ctx, prefetchedRows{asked: candidates.seen}, queries); err != nil {
		t.Fatalf("resolveQueries over a complete prefetch: err = %v, want nil", err)
	}
	AssertNoRecord(t, capture, uncoveredQueryMessage)
}
