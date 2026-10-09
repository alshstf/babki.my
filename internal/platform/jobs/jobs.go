// Package jobs runs the background job queue (River on Postgres): the client,
// the periodic schedule, the heartbeat and the record of how each job ended.
// Which jobs exist is the caller's to say (internal/background).
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// SoftStopTimeout is how long running jobs get to finish after shutdown
// begins. Without it River cancels them the moment the start context is
// cancelled. It must stay below cmd/babki's stopJobClientTimeout (a test
// holds the order), or the outer stop would cancel jobs still in their window.
const SoftStopTimeout = 10 * time.Second

// Enqueuer lets a worker insert jobs through the client it is registered
// with: workers exist before the client, and NewClient fills this in. The
// field is written before any worker goroutine starts, so it needs no lock.
type Enqueuer struct{ client *river.Client[pgx.Tx] }

func NewEnqueuer() *Enqueuer { return &Enqueuer{} }

func (e *Enqueuer) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (
	*rivertype.JobInsertResult, error,
) {
	if e.client == nil {
		return nil, errors.New("jobs: the job queue is not running yet, nothing can be enqueued")
	}
	return e.client.Insert(ctx, args, opts)
}

// Periodic is a job queued on a clock; it also runs once at start.
type Periodic struct {
	Every time.Duration
	Args  river.JobArgs
}

// heartbeat is the platform's own periodic job.
var heartbeat = Periodic{Every: time.Minute, Args: HeartbeatArgs{}}

// NewWorkers returns the workers the queue itself needs (the heartbeat); the
// caller adds its own.
func NewWorkers(log *slog.Logger, pool *pgxpool.Pool) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &heartbeatWorker{log: log, pool: pool})
	return workers
}

// NewClient builds the River client with the workers and the schedule (the
// heartbeat is added to it), and attaches it to enqueuer. The jobs of the
// shown kinds keep a progress row for the screen while they run (Progress).
func NewClient(pool *pgxpool.Pool, workers *river.Workers, schedule []Periodic, shown []string, enqueuer *Enqueuer,
	log *slog.Logger,
) (*river.Client[pgx.Tx], error) {
	all := append([]Periodic{heartbeat}, schedule...)
	periodic := make([]*river.PeriodicJob, 0, len(all))
	for _, job := range all {
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(job.Every),
			func() (river.JobArgs, *river.InsertOpts) { return job.Args, ScheduledOpts(job.Every) },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:          log,
		Hooks:           []rivertype.Hook{&outcomeHook{pool: pool, log: log}},
		Middleware:      []rivertype.Middleware{newProgressMiddleware(pool, shown, log)},
		Workers:         workers,
		SoftStopTimeout: SoftStopTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		PeriodicJobs: periodic,
	})
	if err != nil {
		return nil, err
	}
	enqueuer.client = client
	return client, nil
}

// NewInsertOnlyClient builds a client that only enqueues, for the api role. It
// has no queues or workers and must not be started.
func NewInsertOnlyClient(pool *pgxpool.Pool, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: log})
}

// unfinishedStates are the states of a job still in flight, including one
// waiting to retry.
var unfinishedStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// ScheduledOpts allows one job of a kind in flight at a time, and bounds its
// attempts to fit inside the interval: otherwise River's growing backoff would
// park a failing job, holding the slot, past several ticks.
func ScheduledOpts(every time.Duration) *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: AttemptsWithin(every),
		UniqueOpts:  river.UniqueOpts{ByState: unfinishedStates},
	}
}

// AttemptsWithin is how many attempts fit in interval under River's default
// backoff of attempt⁴ seconds.
func AttemptsWithin(interval time.Duration) int {
	attempts, waited := 1, time.Duration(0)
	for {
		next := time.Duration(attempts*attempts*attempts*attempts) * time.Second
		if waited+next >= interval {
			return attempts
		}
		waited += next
		attempts++
	}
}
