// Package background wires every module's background jobs onto the queue in
// internal/platform/jobs: which workers run, on what schedule, and which of
// them fetch data from outside (served as GET /api/v1/data-sources).
package background

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/jobs"
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
	// Dividends are declared weeks ahead and paid quarterly at most; reading a
	// calendar once a day is plenty.
	tinvestDividendsInterval = 24 * time.Hour

	// Splits are announced days ahead and take effect on a date.
	corporateActionsInterval = 24 * time.Hour

	// A fund's NAV is published days late; a home-exchange close once a day.
	referencePricesInterval = 24 * time.Hour
)

// ReferenceSources are the feeds beyond the exchange and the broker: the full
// valuation's reference prices (decision Р-11) and the dividend calendar of
// papers no broker's calendar covers (Р-14). A nil one is not fetched.
type ReferenceSources struct {
	NAV       marketdata.NAVProvider
	Foreign   marketdata.ForeignQuoteProvider
	Dividends marketdata.DividendFeed
}

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
	references ReferenceSources,
	tinvestDeps TinvestDeps,
	caStore *corporateaction.Store,
	caMaterializer *corporateaction.Materializer,
	enqueuer *jobs.Enqueuer,
) *river.Workers {
	workers := jobs.NewWorkers(log, pool)
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
	river.AddWorker(workers, marketdata.NewReferencePricesWorker(mdStore, operations, instruments,
		references.NAV, references.Foreign, log))
	river.AddWorker(workers, marketdata.NewDividendCalendarWorker(mdStore, operations, instruments,
		references.Dividends, log))
	return workers
}

// NewClient builds the River client with the workers and Schedule, and
// attaches it to enqueuer.
func NewClient(pool *pgxpool.Pool, workers *river.Workers, enqueuer *jobs.Enqueuer, log *slog.Logger) (
	*river.Client[pgx.Tx], error,
) {
	return jobs.NewClient(pool, workers, Schedule(), enqueuer, log)
}

// Schedule is every periodic job of the modules; each also runs once at start.
func Schedule() []jobs.Periodic {
	return []jobs.Periodic{
		{Every: refreshFxInterval, Args: marketdata.RefreshFxArgs{}},
		{Every: refreshQuotesInterval, Args: marketdata.RefreshQuotesArgs{}},
		{Every: backfillFxInterval, Args: marketdata.BackfillFxArgs{}},
		{Every: backfillFxInterval, Args: marketdata.BackfillGoldArgs{}},
		{Every: backfillFxInterval, Args: marketdata.BackfillQuotesArgs{}},
		{Every: tinvestSyncInterval, Args: tinvest.SyncDispatchArgs{}},
		{Every: tinvestQuotesInterval, Args: tinvest.RefreshQuotesArgs{}},
		{Every: backfillFxInterval, Args: tinvest.BackfillQuotesArgs{}},
		{Every: tinvestDividendsInterval, Args: tinvest.RefreshDividendsArgs{}},
		{Every: corporateActionsInterval, Args: corporateaction.RefreshMoexSplitsArgs{}},
		{Every: corporateActionsInterval, Args: corporateaction.MaterializeAllArgs{}},
		{Every: referencePricesInterval, Args: marketdata.RefreshReferencePricesArgs{}},
		{Every: tinvestDividendsInterval, Args: marketdata.RefreshDividendCalendarArgs{}},
	}
}

// sources are the jobs that fetch data from outside, in the order a reader
// would look for them, with how often each runs.
var sources = []jobs.SourceKind{
	{Kind: marketdata.RefreshQuotesArgs{}.Kind(), Every: refreshQuotesInterval},
	{Kind: marketdata.BackfillQuotesArgs{}.Kind(), Every: backfillFxInterval},
	{Kind: marketdata.RefreshFxArgs{}.Kind(), Every: refreshFxInterval},
	{Kind: marketdata.BackfillFxArgs{}.Kind(), Every: backfillFxInterval},
	{Kind: tinvest.SyncArgs{}.Kind(), Every: tinvestSyncInterval},
	{Kind: tinvest.RefreshQuotesArgs{}.Kind(), Every: tinvestQuotesInterval},
	{Kind: tinvest.BackfillQuotesArgs{}.Kind(), Every: backfillFxInterval},
	{Kind: tinvest.RefreshDividendsArgs{}.Kind(), Every: tinvestDividendsInterval},
	{Kind: corporateaction.RefreshMoexSplitsArgs{}.Kind(), Every: corporateActionsInterval},
	{Kind: marketdata.RefreshReferencePricesArgs{}.Kind(), Every: referencePricesInterval},
	{Kind: marketdata.RefreshDividendCalendarArgs{}.Kind(), Every: tinvestDividendsInterval},
}

// Sources reads how each source's jobs last ended.
func Sources(ctx context.Context, pool *pgxpool.Pool) ([]jobs.Source, error) {
	return jobs.Sources(ctx, pool, sources)
}
