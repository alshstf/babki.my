package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// retriesTake is how long River's own retry policy keeps a job that fails
// every time, from its first failure to its last attempt. The policy jitters
// each wait by up to a tenth either way, so the figure differs from run to run;
// the intervals are far enough from it that the comparison below does not.
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

// Every job of the schedule is queued one of a kind at a time, and a job that
// fails every time has used up its attempts before the next one is due. The
// second half is checked against River's own retry policy rather than against
// the formula this package assumes of it: with the two rules together, a job
// parked in backoff past its interval would silence the schedule for that kind.
func TestEveryScheduledJobIsOneAtATimeAndGivesUpBeforeTheNextIsDue(t *testing.T) {
	entries := jobs.Schedule()
	if len(entries) == 0 {
		t.Fatal("the schedule is empty")
	}
	for _, e := range entries {
		kind := e.Args.Kind()
		if e.Opts == nil || len(e.Opts.UniqueOpts.ByState) == 0 {
			t.Errorf("%s is queued with no uniqueness: a source that is down collects a job per tick", kind)
			continue
		}
		states := map[rivertype.JobState]bool{}
		for _, s := range e.Opts.UniqueOpts.ByState {
			states[s] = true
		}
		if !states[rivertype.JobStateRetryable] {
			t.Errorf("%s is not unique across a job waiting to be retried", kind)
		}
		if states[rivertype.JobStateCompleted] {
			t.Errorf("%s is unique across COMPLETED jobs: a finished run would block every later tick until the cleaner removed it", kind)
		}
		if e.Opts.MaxAttempts < 2 {
			t.Errorf("%s gets %d attempts, want at least one retry", kind, e.Opts.MaxAttempts)
		}
		if took := retriesTake(e.Opts.MaxAttempts); took >= e.Every {
			t.Errorf("%s: %d attempts take %s under River's retry policy, which is not inside its %s interval",
				kind, e.Opts.MaxAttempts, took.Round(time.Second), e.Every)
		}
	}
}

func TestAttemptsWithin(t *testing.T) {
	for interval, want := range map[time.Duration]int{
		time.Minute:      3,
		30 * time.Minute: 6,
		time.Hour:        7,
		24 * time.Hour:   13,
	} {
		if got := jobs.AttemptsWithin(interval); got != want {
			t.Errorf("AttemptsWithin(%s) = %d, want %d", interval, got, want)
		}
	}
}

// A connection's sync is unique per connection, so the same reasoning binds
// it: its attempts have to fit inside the hour between two dispatches.
func TestASyncGivesUpBeforeTheNextDispatch(t *testing.T) {
	opts := tinvest.SyncInsertOpts()
	if opts.MaxAttempts != jobs.AttemptsWithin(jobs.TinvestSyncInterval) {
		t.Errorf("a sync gets %d attempts, want %d — what fits inside the %s between two dispatches",
			opts.MaxAttempts, jobs.AttemptsWithin(jobs.TinvestSyncInterval), jobs.TinvestSyncInterval)
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
	for _, e := range jobs.Schedule() {
		first, err := client.Insert(ctx, e.Args, e.Opts)
		if err != nil {
			t.Fatalf("%s: first insert: %v", e.Args.Kind(), err)
		}
		if first.UniqueSkippedAsDuplicate {
			t.Fatalf("%s: the first insert into an empty queue was skipped as a duplicate", e.Args.Kind())
		}
		if first.Job.MaxAttempts != e.Opts.MaxAttempts {
			t.Errorf("%s: queued with %d attempts, want %d", e.Args.Kind(), first.Job.MaxAttempts, e.Opts.MaxAttempts)
		}
		second, err := client.Insert(ctx, e.Args, e.Opts)
		if err != nil {
			t.Fatalf("%s: second insert: %v", e.Args.Kind(), err)
		}
		if !second.UniqueSkippedAsDuplicate {
			t.Errorf("%s: a second job was queued beside an unfinished first", e.Args.Kind())
		}
	}
}
