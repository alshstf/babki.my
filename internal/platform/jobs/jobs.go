// Package jobs is the background job queue built on River (stored in
// Postgres, so enqueueing is transactional with business data). Domain
// modules will register their own workers here in future plans.
package jobs

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

// refreshFxInterval and refreshQuotesInterval set how often the fx and
// quotes periodic jobs enqueue. FX rates only change once per business day
// at the source (cbr.ru), so a daily refresh is enough; quotes move
// throughout the trading session, so they refresh more often.
//
// backfillFxInterval only sets the pace of *starting* a backfill: once
// running, each chunk enqueues the next one itself, so the daily tick is a
// safety net (and the trigger for history newly needed by a backdated
// operation), not the pace of the backfill.
const (
	refreshFxInterval     = 24 * time.Hour
	refreshQuotesInterval = 30 * time.Minute
	backfillFxInterval    = 24 * time.Hour
)

// NewWorkers registers all of the application's workers. mdStore,
// instruments and operations back the marketdata jobs; fxProvider and
// quoteProvider are the external sources those jobs pull from (e.g. cbr and
// moex in production, fakes in tests).
func NewWorkers(
	log *slog.Logger,
	pool *pgxpool.Pool,
	mdStore *marketdata.Store,
	instruments *instrument.Store,
	operations *operation.Store,
	fxProvider marketdata.FxProvider,
	quoteProvider marketdata.QuoteProvider,
) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &heartbeatWorker{log: log, pool: pool})
	river.AddWorker(workers, marketdata.NewFxWorker(mdStore, fxProvider, log))
	river.AddWorker(workers, marketdata.NewQuotesWorker(mdStore, instruments, quoteProvider, log))
	river.AddWorker(workers, marketdata.NewBackfillFxWorker(mdStore, operations, fxProvider, log))
	return workers
}

// NewClient creates a River client with the given workers and periodic jobs.
func NewClient(pool *pgxpool.Pool, workers *river.Workers, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(time.Minute),
				func() (river.JobArgs, *river.InsertOpts) {
					return HeartbeatArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(refreshFxInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return marketdata.RefreshFxArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(refreshQuotesInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return marketdata.RefreshQuotesArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(backfillFxInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return marketdata.BackfillFxArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
}
