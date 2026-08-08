// Package jobs is the background job queue built on River (stored in
// Postgres, so enqueueing is transactional with business data). Domain
// modules will register their own workers here in future plans.
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
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
)

// refreshFxInterval and refreshQuotesInterval set how often the fx and
// quotes periodic jobs enqueue. FX rates only change once per business day
// at the source (cbr.ru), so a daily refresh is enough; quotes move
// throughout the trading session, so they refresh more often.
//
// backfillFxInterval paces the history download. A run fetches every
// currency's whole series in one request each, so it needs no continuation
// and a daily tick is enough: it picks up history newly needed by a
// backdated operation, and re-running simply overwrites the same rows.
//
// tinvestSyncInterval is how often the T-Invest importer looks for new
// operations. Hourly is a deliberate middle: the products people compare this
// one with sit either side of it (Intelinvest syncs a couple of times a day,
// Snowball as often as every fifteen minutes), and personal portfolios do not
// change faster than that in any way an hour's delay would misrepresent.
//
// It is affordable because a run costs so little: reading a whole account's
// history is single-digit requests (a thousand operations to the page) against
// a documented limit of 200 a minute for the operations service, and the rebuild
// that follows makes no broker call at all for instruments it has already seen.
// So the cadence is bounded by taste rather than by the broker.
//
// tinvestQuotesInterval paces the broker's own price feed, and it is the SAME
// half hour the exchange feed uses on purpose: they price overlapping sets of
// papers into one table, and two cadences would make which of them a row came
// from depend on the minute the reader happened to look. What the broker adds
// is the papers no exchange feed here covers — foreign shares and a delisted
// fund quoted over the counter — and those move on the same clock as the rest.
//
// It costs one request per hundred instruments per connection, against a
// documented limit for the market-data service far above that.
const (
	refreshFxInterval     = 24 * time.Hour
	refreshQuotesInterval = 30 * time.Minute
	backfillFxInterval    = 24 * time.Hour
	tinvestSyncInterval   = time.Hour
	tinvestQuotesInterval = 30 * time.Minute
)

// SoftStopTimeout is how long a job that is already running gets to finish
// after shutdown begins, before River cancels its context.
//
// WITHOUT IT SET, THERE IS NO SUCH WINDOW AT ALL, and the reason is a piece of
// River's own semantics rather than anything visible at the call site: the
// context handed to Start is the parent of the context every job runs under, so
// cancelling it — which is exactly what SIGTERM does here, the signal context
// being what "all" and "worker" pass to Start — cancels each running job on the
// spot, indistinguishably from StopAndCancel. Setting any positive value
// detaches the work context from the start context and turns that same
// cancellation into a soft stop: producers stop fetching immediately, jobs
// already in flight are left alone, and only when this elapses are they
// cancelled. River states both halves in Config.SoftStopTimeout's own doc.
//
// It matters most for the longest-running jobs — the fx history download and a
// broker sync — which do many external requests in one run and, killed mid-run,
// leave the work to be redone from the start on the next tick.
//
// TEN SECONDS, AND IT MUST STAY BELOW cmd/babki's stopJobClientTimeout, which
// bounds the graceful Stop that follows. If this were the longer of the two,
// the outer bound would expire first on a job that was still inside its
// window, and the process would report a graceful stop that "did not complete
// in time" and escalate to StopAndCancel — cancelling the very jobs this value
// exists to protect, on a schedule that would look like a race in the logs.
// cmd/babki has a test that keeps the two ordered.
const SoftStopTimeout = 10 * time.Second

// TinvestDeps is everything the T-Invest import workers need, grouped rather
// than added to NewWorkers' positional list — which is long enough already that
// two arguments of the same type could be swapped and still compile.
//
// NewClient and NewRebuilder are factories and not instances, each for its own
// reason: a broker client is per token, which is only known once a job has read
// a connection; and a Rebuilder is per run, because the Resolver it carries
// caches broker passports in a plain map and is not safe for concurrent use
// (see tinvest.Rebuilder's own doc).
type TinvestDeps struct {
	Store        *tinvest.Store
	Box          *secretbox.Box
	NewClient    func(token string) (*tinvest.Client, error)
	NewRebuilder func() *tinvest.Rebuilder
	Reconciler   *tinvest.Reconciler
}

// Enqueuer is the queue as a worker that queues other jobs sees it.
//
// It exists because the T-Invest dispatcher and the River client genuinely need
// each other: the dispatcher is a worker, workers are registered before a client
// can be built, and the dispatcher's whole job is to insert through that client.
// The indirection is filled in by NewClient below, at the one place a client
// comes into existence, so no caller can forget to do it.
//
// Insert before that point is an error and never a nil dereference: a
// dispatcher that somehow ran against an unattached Enqueuer must say so and be
// retried, not crash the process.
//
// The field is written once, in NewClient, and read from worker goroutines. It
// needs no lock: those goroutines do not exist until the client is started, and
// starting it is what the caller does after NewClient has returned — so the
// write happens before any goroutine that reads it is created.
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

// NewWorkers registers all of the application's workers. mdStore,
// instruments, operations, accounts and spaces back the marketdata jobs;
// fxProvider and quoteProvider are the external sources those jobs pull from
// (e.g. cbr and moex in production, fakes in tests). fxProvider is an
// FxHistoryProvider rather than a plain FxProvider because the history
// download needs a source that can deliver a whole date range at once.
// tinvest and enqueuer back the T-Invest import jobs; enqueuer must be the same
// one handed to NewClient, which is what fills it in.
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
	enqueuer *Enqueuer,
) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &heartbeatWorker{log: log, pool: pool})
	river.AddWorker(workers, marketdata.NewFxWorker(mdStore, fxProvider, log))
	river.AddWorker(workers, marketdata.NewQuotesWorker(mdStore, instruments, quoteProvider, log))
	river.AddWorker(workers, marketdata.NewBackfillFxWorker(
		mdStore, operations, accounts, spaces, fxProvider, log))
	river.AddWorker(workers, tinvest.NewDispatchWorker(tinvestDeps.Store, enqueuer, log))
	river.AddWorker(workers, tinvest.NewSyncWorker(tinvestDeps.Store, tinvestDeps.Box,
		tinvestDeps.NewClient, tinvestDeps.NewRebuilder, tinvestDeps.Reconciler, log))
	river.AddWorker(workers, tinvest.NewQuotesWorker(tinvestDeps.Store, mdStore,
		tinvestDeps.Box, tinvestDeps.NewClient, log, nil))
	return workers
}

// NewClient creates a River client with the given workers and periodic jobs,
// and attaches it to enqueuer — the indirection the dispatch worker registered
// above inserts through. The attaching happens HERE, and not at the call site,
// because this is the moment the client first exists and there is then nothing
// left for a caller to forget.
//
// THE ATTACHING IS COVERED, and it has to be: without it every dispatch of the
// import answers "the job queue is not running yet", is retried, answers the
// same, and no connection is ever synced — while the process looks perfectly
// healthy. TestStartingTheQueueQueuesASyncForAnActiveConnection is what would
// notice; deleting the line below turns it red, as does deleting the periodic
// job that fires the dispatcher.
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

// NewInsertOnlyClient builds a River client that can only ENQUEUE jobs, for the
// "api" role: a process that serves requests and works nothing.
//
// NO QUEUES AND NO WORKERS, which is River's own documented shape for this ("an
// insert-only client can be initialized by omitting Queues, and not calling
// Start for the client"). It must therefore never be started, and there is
// nothing to stop: with no queue configured it runs no goroutines of its own.
//
// Omitting Workers costs one check River would otherwise make — that a kind
// being inserted has a worker registered for it — and it is omitted anyway,
// because in this process the answer would always be no: it registers none. A
// bundle listing kinds this binary cannot run would be a claim about the worker
// process rather than about this one.
func NewInsertOnlyClient(pool *pgxpool.Pool, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: log})
}

func newClient(pool *pgxpool.Pool, workers *river.Workers, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:          log,
		Workers:         workers,
		SoftStopTimeout: SoftStopTimeout,
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
			river.NewPeriodicJob(
				river.PeriodicInterval(tinvestSyncInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return tinvest.SyncDispatchArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(tinvestQuotesInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return tinvest.RefreshQuotesArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
}
