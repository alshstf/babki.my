// Package jobs runs the background job queue (River on Postgres) and the
// schedule of periodic jobs.
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

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
)

// How often each periodic job runs. Rates change once a business day; quotes
// move during the session. The T-Invest sync is hourly — a run costs a few
// requests against a limit of 200 a minute — and the broker's quotes share
// the exchange feed's half hour because both write one table.
const (
	refreshFxInterval     = 24 * time.Hour
	refreshQuotesInterval = 30 * time.Minute
	backfillFxInterval    = 24 * time.Hour
	tinvestSyncInterval   = time.Hour
	tinvestQuotesInterval = 30 * time.Minute
	// Dividends are declared weeks ahead and paid quarterly at most; reading
	// the broker's calendar once a day is plenty.
	tinvestDividendsInterval = 24 * time.Hour

	// Splits are announced days ahead and take effect on a date.
	corporateActionsInterval = 24 * time.Hour
)

// SoftStopTimeout is how long running jobs get to finish after shutdown
// begins. Without it River cancels them the moment the start context is
// cancelled. It must stay below cmd/babki's stopJobClientTimeout (a test
// holds the order), or the outer stop would cancel jobs still in their window.
const SoftStopTimeout = 10 * time.Second

// TinvestDeps is what the T-Invest workers need. Clients are made per token
// and Rebuilders per run (a Resolver's cache is not safe for concurrent use),
// hence the factories.
type TinvestDeps struct {
	Store        *tinvest.Store
	Box          *secretbox.Box
	NewClient    func(token string) (*tinvest.Client, error)
	NewRebuilder func() *tinvest.Rebuilder
	Reconciler   *tinvest.Reconciler
}

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

// NewWorkers registers every worker. enqueuer must be the one later passed to
// NewClient.
func NewWorkers(
	log *slog.Logger,
	pool *pgxpool.Pool,
	mdStore *marketdata.Store,
	instruments *instrument.Store,
	operations *operation.Store,
	accounts *account.Store,
	spaces *family.Store,
	fxProvider marketdata.FxHistoryProvider,
	quoteProvider marketdata.QuoteProvider,
	tinvestDeps TinvestDeps,
	caStore *corporateaction.Store,
	caMaterializer *corporateaction.Materializer,
	enqueuer *Enqueuer,
) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &heartbeatWorker{log: log, pool: pool})
	river.AddWorker(workers, marketdata.NewFxWorker(mdStore, fxProvider, log))
	river.AddWorker(workers, marketdata.NewQuotesWorker(mdStore, instruments, quoteProvider, log))
	river.AddWorker(workers, marketdata.NewBackfillFxWorker(
		mdStore, operations, accounts, spaces, fxProvider, log))
	// Gold has a source of its own because the central bank publishes none for
	// it (see marketdata.backfillGoldWorker). Registered only when the quote
	// provider can answer for it — the interface is the exchange client's, and
	// a deployment wired to something else simply has no gold rates rather than
	// a job that fails for ever.
	if gold, ok := quoteProvider.(marketdata.GoldRateProvider); ok {
		river.AddWorker(workers, marketdata.NewBackfillGoldWorker(mdStore, operations, gold, log))
	}
	// Past closing prices come from the exchange too, by the same rule.
	if history, ok := quoteProvider.(marketdata.HistoryProvider); ok {
		river.AddWorker(workers, marketdata.NewBackfillQuotesWorker(mdStore, operations, instruments, history, log))
	}
	river.AddWorker(workers, tinvest.NewDispatchWorker(tinvestDeps.Store, enqueuer, log))
	river.AddWorker(workers, tinvest.NewSyncWorker(tinvestDeps.Store, tinvestDeps.Box,
		tinvestDeps.NewClient, tinvestDeps.NewRebuilder, caMaterializer, tinvestDeps.Reconciler, log))
	river.AddWorker(workers, tinvest.NewQuotesWorker(tinvestDeps.Store, mdStore,
		tinvestDeps.Box, tinvestDeps.NewClient, log, nil))
	river.AddWorker(workers, tinvest.NewBackfillQuotesWorker(tinvestDeps.Store, mdStore, operations,
		tinvestDeps.Box, tinvestDeps.NewClient, log))
	river.AddWorker(workers, tinvest.NewDividendsWorker(tinvestDeps.Store, mdStore,
		tinvestDeps.Box, tinvestDeps.NewClient, log, nil))
	// The corporate-actions registry. The refresh worker is registered only
	// when the quote provider can also answer about splits — the same rule the
	// gold worker follows above and for the same reason: the interface is the
	// exchange client's, and a deployment wired to something else has no
	// automatic splits rather than a job that fails for ever. The sweep is
	// registered unconditionally, because it reads only what is already stored
	// and is what carries a HAND-ENTERED event into the journals.
	if splits, ok := quoteProvider.(corporateaction.SplitsProvider); ok {
		river.AddWorker(workers, corporateaction.NewRefreshMoexSplitsWorker(
			caStore, caMaterializer, splits, log))
	}
	river.AddWorker(workers, corporateaction.NewMaterializeAllWorker(caMaterializer, log))
	river.AddWorker(workers, corporateaction.NewMaterializeISINWorker(caMaterializer, log))
	return workers
}

// NewClient builds the River client with the workers and the schedule, and
// attaches it to enqueuer.
func NewClient(pool *pgxpool.Pool, workers *river.Workers, enqueuer *Enqueuer, log *slog.Logger) (
	*river.Client[pgx.Tx], error,
) {
	client, err := newClient(pool, workers, log)
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

// scheduledJob is a job queued on a clock.
type scheduledJob struct {
	every time.Duration
	args  river.JobArgs
}

// schedule is every periodic job; each also runs once at start.
func schedule() []scheduledJob {
	return []scheduledJob{
		{time.Minute, HeartbeatArgs{}},
		{refreshFxInterval, marketdata.RefreshFxArgs{}},
		{refreshQuotesInterval, marketdata.RefreshQuotesArgs{}},
		{backfillFxInterval, marketdata.BackfillFxArgs{}},
		{backfillFxInterval, marketdata.BackfillGoldArgs{}},
		{backfillFxInterval, marketdata.BackfillQuotesArgs{}},
		{tinvestSyncInterval, tinvest.SyncDispatchArgs{}},
		{tinvestQuotesInterval, tinvest.RefreshQuotesArgs{}},
		{backfillFxInterval, tinvest.BackfillQuotesArgs{}},
		{tinvestDividendsInterval, tinvest.RefreshDividendsArgs{}},
		{corporateActionsInterval, corporateaction.RefreshMoexSplitsArgs{}},
		{corporateActionsInterval, corporateaction.MaterializeAllArgs{}},
	}
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

// scheduledOpts allows one job of a kind in flight at a time, and bounds its
// attempts to fit inside the interval: otherwise River's growing backoff would
// park a failing job, holding the slot, past several ticks.
func scheduledOpts(every time.Duration) *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: attemptsWithin(every),
		UniqueOpts:  river.UniqueOpts{ByState: unfinishedStates},
	}
}

// attemptsWithin is how many attempts fit in interval under River's default
// backoff of attempt⁴ seconds.
func attemptsWithin(interval time.Duration) int {
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

func newClient(pool *pgxpool.Pool, workers *river.Workers, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	periodic := make([]*river.PeriodicJob, 0, len(schedule()))
	for _, job := range schedule() {
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(job.every),
			func() (river.JobArgs, *river.InsertOpts) { return job.args, scheduledOpts(job.every) },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:          log,
		Hooks:           []rivertype.Hook{&outcomeHook{pool: pool, log: log}},
		Workers:         workers,
		SoftStopTimeout: SoftStopTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		PeriodicJobs: periodic,
	})
}
