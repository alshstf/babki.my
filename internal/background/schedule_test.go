package background_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/background"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// retriesTake is how long River's retry policy keeps a job that always fails;
// its ±10% jitter is far from the intervals compared against.
func retriesTake(maxAttempts int) time.Duration {
	policy := &river.DefaultClientRetryPolicy{}
	var total time.Duration
	for failed := 1; failed < maxAttempts; failed++ {
		now := time.Now()
		row := &rivertype.JobRow{Attempt: failed, AttemptedAt: &now, Errors: make([]rivertype.AttemptError, failed-1)}
		total += time.Until(policy.NextRetry(row))
	}
	return total
}

// Every scheduled job is one of a kind at a time, and an always-failing job
// exhausts its attempts (by River's real policy) before the next is due.
func TestEveryScheduledJobIsOneAtATimeAndGivesUpBeforeTheNextIsDue(t *testing.T) {
	entries := background.Schedule()
	if len(entries) == 0 {
		t.Fatal("the schedule is empty")
	}
	for _, e := range entries {
		kind, opts := e.Args.Kind(), jobs.ScheduledOpts(e.Every)
		if opts == nil || len(opts.UniqueOpts.ByState) == 0 {
			t.Errorf("%s is queued with no uniqueness: a source that is down collects a job per tick", kind)
			continue
		}
		states := map[rivertype.JobState]bool{}
		for _, s := range opts.UniqueOpts.ByState {
			states[s] = true
		}
		if !states[rivertype.JobStateRetryable] {
			t.Errorf("%s is not unique across a job waiting to be retried", kind)
		}
		if states[rivertype.JobStateCompleted] {
			t.Errorf("%s is unique across COMPLETED jobs: a finished run would block every later tick until the cleaner removed it", kind)
		}
		if opts.MaxAttempts < 2 {
			t.Errorf("%s gets %d attempts, want at least one retry", kind, opts.MaxAttempts)
		}
		if took := retriesTake(opts.MaxAttempts); took >= e.Every {
			t.Errorf("%s: %d attempts take %s under River's retry policy, which is not inside its %s interval",
				kind, opts.MaxAttempts, took.Round(time.Second), e.Every)
		}
	}
}

// A connection's sync is unique per connection, so the same reasoning binds
// it: its attempts have to fit inside the hour between two dispatches.
func TestASyncGivesUpBeforeTheNextDispatch(t *testing.T) {
	opts := tinvest.SyncInsertOpts()
	if opts.MaxAttempts != jobs.AttemptsWithin(background.TinvestSyncInterval) {
		t.Errorf("a sync gets %d attempts, want %d — what fits inside the %s between two dispatches",
			opts.MaxAttempts, jobs.AttemptsWithin(background.TinvestSyncInterval), background.TinvestSyncInterval)
	}
}

// The uniqueness is enforced by the database: a second job of a kind is
// skipped while the first has not finished, whoever inserts it.
func TestASecondJobOfAScheduledKindIsSkippedWhileTheFirstIsUnfinished(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	client, err := jobs.NewInsertOnlyClient(pool, slog.Default())
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	for _, e := range background.Schedule() {
		opts := jobs.ScheduledOpts(e.Every)
		first, err := client.Insert(ctx, e.Args, opts)
		if err != nil {
			t.Fatalf("%s: first insert: %v", e.Args.Kind(), err)
		}
		if first.UniqueSkippedAsDuplicate {
			t.Fatalf("%s: the first insert into an empty queue was skipped as a duplicate", e.Args.Kind())
		}
		if first.Job.MaxAttempts != opts.MaxAttempts {
			t.Errorf("%s: queued with %d attempts, want %d", e.Args.Kind(), first.Job.MaxAttempts, opts.MaxAttempts)
		}
		second, err := client.Insert(ctx, e.Args, opts)
		if err != nil {
			t.Fatalf("%s: second insert: %v", e.Args.Kind(), err)
		}
		if !second.UniqueSkippedAsDuplicate {
			t.Errorf("%s: a second job was queued beside an unfinished first", e.Args.Kind())
		}
	}
}
