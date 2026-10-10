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
	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/notify"
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
	// pushRemindersInterval is how often the reminders due are pushed: each
	// goes once, in the day, so an hour is soon enough.
	pushRemindersInterval = time.Hour

	// Splits are announced days ahead and take effect on a date.
	corporateActionsInterval = 24 * time.Hour

	// A fund's NAV is published days late; a home-exchange close once a day.
	referencePricesInterval = 24 * time.Hour
)

// ReferenceSources are the feeds beyond the exchange and the broker: the full
// valuation's reference prices (decision Р-11), the dividend calendar of
// papers no broker's calendar covers (Р-14), foreign papers' splits and
// cryptocurrencies' prices (Р-20). A nil one is not fetched.
type ReferenceSources struct {
	NAV       []marketdata.NAVProvider
	Foreign   marketdata.ForeignQuoteProvider
	Dividends marketdata.DividendFeed
	Splits    corporateaction.ForeignSplitsProvider
	Crypto    marketdata.CryptoQuoteProvider
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
	river.AddWorker(workers, corporateaction.NewRefreshForeignSplitsWorker(
		caStore, caMaterializer, operations, instruments, references.Splits, log))
	river.AddWorker(workers, corporateaction.NewRecordKnownConversionsWorker(caStore, caMaterializer, instruments, log))
	river.AddWorker(workers, corporateaction.NewMaterializeAllWorker(caMaterializer, log))
	river.AddWorker(workers, corporateaction.NewMaterializeISINWorker(caMaterializer, log))
	river.AddWorker(workers, marketdata.NewReferencePricesWorker(mdStore, operations, instruments,
		references.NAV, references.Foreign, log))
	river.AddWorker(workers, marketdata.NewDividendCalendarWorker(mdStore, operations, instruments,
		references.Dividends, log))
	river.AddWorker(workers, marketdata.NewCryptoPricesWorker(mdStore, operations, instruments, references.Crypto, log))
	// Bonds' schedules come from the exchange, by the rule the gold and
	// history workers follow.
	if schedules, ok := quoteProvider.(marketdata.BondScheduleFeed); ok {
		river.AddWorker(workers, marketdata.NewBondScheduleWorker(mdStore, operations, instruments, schedules, log))
	}
	// Benchmark indices (#401): the exchange's MCFTR and RGBITR, the S&P 500
	// with dividends from the foreign feed — each where its feed is wired.
	indexFeeds := map[string]marketdata.IndexFeed{}
	if ix, ok := quoteProvider.(marketdata.IndexFeed); ok {
		indexFeeds["moex"] = ix
	}
	if references.Foreign != nil {
		indexFeeds["yahoo"] = marketdata.ClosesAsIndex(references.Foreign)
	}
	river.AddWorker(workers, marketdata.NewIndexesWorker(mdStore, operations, indexFeeds, log))
	// Push reminders (decision Р-27): the keys come from the encryption key,
	// so without one the worker is registered but sends nothing.
	var sender notify.Sender
	if tinvestDeps.Box != nil {
		if keys, err := notify.KeysFrom(tinvestDeps.Box); err == nil {
			sender = notify.NewWebPush(keys)
		} else {
			log.Error("push keys", "error", err)
		}
	}
	report := cashflow.NewService(operations, accounts, category.NewStore(pool), spaces, marketdata.NewConverter(mdStore))
	river.AddWorker(workers, notify.NewRemindersWorker(notify.NewStore(pool),
		creditcard.NewService(pool, accounts, operations, category.NewStore(pool)),
		budget.NewService(pool, report, category.NewStore(pool)), category.NewStore(pool), sender, log))
	return workers
}

// NewClient builds the River client with the workers and Schedule, and
// attaches it to enqueuer.
func NewClient(pool *pgxpool.Pool, workers *river.Workers, enqueuer *jobs.Enqueuer, log *slog.Logger) (
	*river.Client[pgx.Tx], error,
) {
	return jobs.NewClient(pool, workers, Schedule(), Shown(), enqueuer, log)
}

// Shown are the jobs the screen shows while they run (jobs.Progress): the broker
// import, and what catches up the rates, prices and registry its figures need.
func Shown() []string {
	return []string{
		tinvest.SyncArgs{}.Kind(),
		marketdata.BackfillFxArgs{}.Kind(),
		marketdata.BackfillGoldArgs{}.Kind(),
		marketdata.BackfillQuotesArgs{}.Kind(),
		tinvest.BackfillQuotesArgs{}.Kind(),
		corporateaction.MaterializeAllArgs{}.Kind(),
	}
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
		{Every: corporateActionsInterval, Args: corporateaction.RefreshForeignSplitsArgs{}},
		{Every: corporateActionsInterval, Args: corporateaction.RecordKnownConversionsArgs{}},
		{Every: corporateActionsInterval, Args: corporateaction.MaterializeAllArgs{}},
		{Every: referencePricesInterval, Args: marketdata.RefreshReferencePricesArgs{}},
		{Every: tinvestDividendsInterval, Args: marketdata.RefreshDividendCalendarArgs{}},
		{Every: referencePricesInterval, Args: marketdata.RefreshCryptoPricesArgs{}},
		{Every: tinvestDividendsInterval, Args: marketdata.RefreshBondSchedulesArgs{}},
		{Every: pushRemindersInterval, Args: notify.SendRemindersArgs{}},
		{Every: tinvestDividendsInterval, Args: marketdata.RefreshIndexesArgs{}},
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
	{Kind: corporateaction.RefreshForeignSplitsArgs{}.Kind(), Every: corporateActionsInterval},
	{Kind: marketdata.RefreshReferencePricesArgs{}.Kind(), Every: referencePricesInterval},
	{Kind: marketdata.RefreshDividendCalendarArgs{}.Kind(), Every: tinvestDividendsInterval},
	{Kind: marketdata.RefreshCryptoPricesArgs{}.Kind(), Every: referencePricesInterval},
	{Kind: marketdata.RefreshBondSchedulesArgs{}.Kind(), Every: tinvestDividendsInterval},
	{Kind: marketdata.RefreshIndexesArgs{}.Kind(), Every: tinvestDividendsInterval},
}

// Sources reads how each source's jobs last ended.
func Sources(ctx context.Context, pool *pgxpool.Pool) ([]jobs.Source, error) {
	return jobs.Sources(ctx, pool, sources)
}
