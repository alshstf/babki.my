package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// The client starts, the heartbeat runs on start and leaves its mark in meta.
func TestHeartbeat(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	client, err := jobs.NewClient(pool, jobs.NewWorkers(slog.Default(), pool), nil, nil, jobs.NewEnqueuer(), slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var v string
		err := pool.QueryRow(ctx,
			`SELECT value FROM meta WHERE key = 'last_heartbeat_at'`).Scan(&v)
		if err == nil && v != "" {
			return // success
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("heartbeat did not run within 15s")
}

// gracefulProbeArgs runs, reports, and then reports how its context ended.
type gracefulProbeArgs struct{}

func (gracefulProbeArgs) Kind() string { return "test.graceful_probe" }

type gracefulProbeWorker struct {
	river.WorkerDefaults[gracefulProbeArgs]
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (w *gracefulProbeWorker) Work(ctx context.Context, _ *river.Job[gracefulProbeArgs]) error {
	close(w.started)
	select {
	case <-ctx.Done():
		close(w.cancelled)
	case <-w.release:
	}
	return nil
}

// Cancelling Start's context, as a signal does, leaves a running job its
// context for SoftStopTimeout. Without the setting River cancels it at once.
// The one-second wait sits far from both the window and an inherited
// cancellation.
func TestSigtermLeavesARunningJobItsGracefulWindow(t *testing.T) {
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probe := &gracefulProbeWorker{
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
	workers := jobs.NewWorkers(slog.Default(), pool)
	river.AddWorker(workers, probe)

	client, err := jobs.NewClient(pool, workers, nil, nil, jobs.NewEnqueuer(), slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := client.Insert(ctx, gracefulProbeArgs{}, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	select {
	case <-probe.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the probe job never started")
	}

	cancel() // the SIGTERM

	select {
	case <-probe.cancelled:
		t.Fatal("the running job's context was cancelled the moment the process was signalled: " +
			"it got no graceful window at all, which is what jobs.SoftStopTimeout is set to prevent")
	case <-time.After(time.Second):
	}

	close(probe.release)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("Stop after the job finished on its own: %v", err)
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

// An Enqueuer refuses until NewClient has attached it, rather than panicking.
func TestAnEnqueuerRefusesBeforeTheQueueRuns(t *testing.T) {
	if _, err := jobs.NewEnqueuer().Insert(context.Background(), gracefulProbeArgs{}, nil); err == nil {
		t.Fatal("an unattached Enqueuer accepted a job")
	}
}

type shownProbeArgs struct{}

func (shownProbeArgs) Kind() string { return "test.shown_probe" }

type shownProbeWorker struct {
	river.WorkerDefaults[shownProbeArgs]
	release chan struct{}
}

func (w *shownProbeWorker) Work(ctx context.Context, _ *river.Job[shownProbeArgs]) error {
	jobs.ProgressFrom(ctx).Stage(ctx, "reading", 3, 10)
	<-w.release
	return nil
}

// The queue itself keeps a shown job's progress row: there while it runs, with
// what the job said, and gone when it ends.
func TestTheQueueKeepsAShownJobsProgressWhileItRuns(t *testing.T) {
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &shownProbeWorker{release: make(chan struct{})}
	workers := jobs.NewWorkers(slog.Default(), pool)
	river.AddWorker(workers, probe)
	client, err := jobs.NewClient(pool, workers, nil, []string{shownProbeArgs{}.Kind()}, jobs.NewEnqueuer(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	}()
	if _, err := client.Insert(ctx, shownProbeArgs{}, nil); err != nil {
		t.Fatal(err)
	}

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("never saw %s", what)
	}
	waitFor("the running job's row", func() bool {
		var stage string
		var done, total int
		err := pool.QueryRow(ctx, `SELECT stage, done, total FROM job_progress WHERE kind = $1`,
			shownProbeArgs{}.Kind()).Scan(&stage, &done, &total)
		return err == nil && stage == "reading" && done == 3 && total == 10
	})
	close(probe.release)
	waitFor("the row go with its job", func() bool {
		var n int
		return pool.QueryRow(ctx, `SELECT count(*) FROM job_progress`).Scan(&n) == nil && n == 0
	})
}
