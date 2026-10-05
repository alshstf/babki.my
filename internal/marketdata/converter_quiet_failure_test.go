package marketdata

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// An uncovered query in RatesOn's second pass is a bug in converter.go, and
// handlers ignore the error, so it is logged at ERROR (#97). It is unreachable
// through RatesOn, so resolveQueries is called with an empty prefetch.

const uncoveredQueryMessage = "fx rate prefetch did not cover the resolution"

// The uncovered query is logged at ERROR, not WARN or DEBUG.
func TestResolveQueriesLogsAnUncoveredQueryAsAnError(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	queries := []RateQuery{{From: "USD", To: "EUR", On: on}}
	capture := CaptureLogs(t)

	// An empty prefetch: the first row asked for was never recorded.
	got, err := resolveQueries(ctx, prefetchedRows{}, queries)
	if err == nil {
		t.Fatalf("resolveQueries over an empty prefetch: err = nil, want the uncovered-query error")
	}
	if got.Len() != 0 {
		t.Fatalf("resolveQueries over an empty prefetch returned %d entries, want the zero Rates", got.Len())
	}
	AssertOneRecordAt(t, capture, uncoveredQueryMessage, slog.LevelError, "USD")
}

// A covered batch, even one with a genuine gap, logs nothing.
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
