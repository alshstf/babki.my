package jobs_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

func sourceOf(t *testing.T, list []jobs.Source, kind string) jobs.Source {
	t.Helper()
	for _, s := range list {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no source %q in %+v", kind, list)
	return jobs.Source{}
}

// Every attempt is recorded under its kind: a success as its time, a failure
// as its time and its text, cut short; the heartbeat, which says nothing about
// outside data, is not recorded; and the job's own error passes through
// unchanged whatever happens to the record. Sources answers the kinds asked
// for, in their order.
func TestEachAttemptOfAJobIsRecorded(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	hook := jobs.NewOutcomeHook(pool, slog.Default())
	quotes := &rivertype.JobRow{Kind: "marketdata.refresh_quotes"}
	kinds := []jobs.SourceKind{{Kind: quotes.Kind, Every: 30 * time.Minute}, {Kind: "never.ran", Every: time.Hour}}

	if err := hook.WorkEnd(ctx, quotes, nil); err != nil {
		t.Fatalf("success: %v", err)
	}
	failure := errors.New("moex: unexpected status 503 " + strings.Repeat("x", 400))
	if err := hook.WorkEnd(ctx, quotes, failure); !errors.Is(err, failure) {
		t.Fatalf("failure passed on as %v, want the job's own error", err)
	}
	if err := hook.WorkEnd(ctx, &rivertype.JobRow{Kind: jobs.HeartbeatArgs{}.Kind()}, nil); err != nil {
		t.Fatal(err)
	}

	list, err := jobs.Sources(ctx, pool, kinds)
	if err != nil {
		t.Fatal(err)
	}
	s := sourceOf(t, list, quotes.Kind)
	if s.LastSuccessAt == nil || s.LastFailureAt == nil {
		t.Fatalf("source = %+v, want both a success and a failure", s)
	}
	if !strings.HasPrefix(s.LastError, "moex: unexpected status 503") || len([]rune(s.LastError)) != 301 {
		t.Errorf("last error = %q (%d runes), want the text cut to 300 and an ellipsis", s.LastError, len([]rune(s.LastError)))
	}
	var heartbeats int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_outcomes WHERE kind = 'heartbeat'`).Scan(&heartbeats); err != nil {
		t.Fatal(err)
	}
	if heartbeats != 0 {
		t.Error("the heartbeat was recorded as a source")
	}
	if len(list) != 2 || list[1].Kind != "never.ran" || list[1].Every != time.Hour || list[1].LastSuccessAt != nil {
		t.Errorf("sources = %+v, want the two kinds asked for, in their order, a kind with no record included", list)
	}
}

// A source is stale once it has not succeeded for three of its intervals, or
// when it has only ever failed; one that has not run yet is not.
func TestASourceIsStaleAfterThreeMissedIntervals(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for name, c := range map[string]struct {
		s    jobs.Source
		want bool
	}{
		"fresh":         {jobs.Source{Every: time.Hour, LastSuccessAt: at(3*time.Hour - time.Minute)}, false},
		"three missed":  {jobs.Source{Every: time.Hour, LastSuccessAt: at(3*time.Hour + time.Minute)}, true},
		"only failed":   {jobs.Source{Every: time.Hour, LastFailureAt: at(time.Minute)}, true},
		"not run yet":   {jobs.Source{Every: time.Hour}, false},
		"failed lately": {jobs.Source{Every: time.Hour, LastSuccessAt: at(time.Hour), LastFailureAt: at(time.Minute)}, false},
	} {
		if got := c.s.Stale(now); got != c.want {
			t.Errorf("%s: stale = %v, want %v", name, got, c.want)
		}
	}
}
