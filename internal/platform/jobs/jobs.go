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
)

// refreshFxInterval and refreshQuotesInterval set how often the fx and
// quotes periodic jobs enqueue. FX rates only change once per business day
// at the source (cbr.ru), so a daily refresh is enough; quotes move
// throughout the trading session, so they refresh more often.
const (
	refreshFxInterval     = 24 * time.Hour
	refreshQuotesInterval = 30 * time.Minute
)

// NewWorkers registers all of the application's workers. mdStore and
// instruments back the marketdata refresh jobs; fxProvider and
// quoteProvider are the external sources those jobs pull from (e.g. cbr and
// moex in production, fakes in tests).
func NewWorkers(
	log *slog.Logger,
	pool *pgxpool.Pool,
	mdStore *marketdata.Store,
	instruments *instrument.Store,
	fxProvider marketdata.FxProvider,
	quoteProvider marketdata.QuoteProvider,
) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &heartbeatWorker{log: log, pool: pool})
	river.AddWorker(workers, marketdata.NewFxWorker(mdStore, fxProvider, log))
	river.AddWorker(workers, marketdata.NewQuotesWorker(mdStore, instruments, quoteProvider, log))
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
		},
	})
}
