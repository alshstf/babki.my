package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
)

// RefreshFxArgs triggers a refresh of today's FX rates from FxProvider. Kind
// is namespaced with "marketdata." so job kinds registered by different
// domain modules never collide in the shared River queue.
type RefreshFxArgs struct{}

func (RefreshFxArgs) Kind() string { return "marketdata.refresh_fx" }

// RefreshQuotesArgs triggers a refresh of quotes, via QuoteProvider, for
// every tradable instrument in the catalog.
type RefreshQuotesArgs struct{}

func (RefreshQuotesArgs) Kind() string { return "marketdata.refresh_quotes" }

// BackfillFxArgs triggers one chunk of historical FX backfill: rates are
// fetched downwards from the current coverage boundary towards the oldest
// operation in the journal, so every operation can be converted at the rate
// of its own date. A job that stops on the chunk cap enqueues its own
// successor, so a cold start catches up in one stretch instead of one chunk
// per day.
type BackfillFxArgs struct{}

func (BackfillFxArgs) Kind() string { return "marketdata.backfill_fx" }

const (
	// backfillChunkDays caps how many dates a single backfill job asks the
	// provider about. It bounds both the job's runtime and the burst we put
	// on the external source.
	backfillChunkDays = 180
	// backfillPause spaces out consecutive requests to the external source.
	backfillPause = 250 * time.Millisecond
	// backfillChunkTimeout overrides River's one-minute default job timeout:
	// a full chunk spends 45 seconds on pauses alone, before any network
	// time is counted.
	backfillChunkTimeout = 15 * time.Minute
)

// backfillFloor is the earliest date worth fetching rates for. An operation
// dated before it is far more likely to be a mistyped date than real
// history, and chasing it would mean thousands of pointless requests.
var backfillFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// operationDater is the subset of operation.Store the backfill worker needs
// — the same locally declared interface pattern as instrumentLister below,
// which additionally keeps marketdata free of an import of the operation
// package. *operation.Store satisfies it structurally.
type operationDater interface {
	EarliestOccurredOn(ctx context.Context) (time.Time, error)
}

// instrumentLister is the subset of instrument.Store the quotes worker
// needs. Declared locally rather than depending on *instrument.Store
// directly — the same pattern journalStore uses in portfolio/http.go — so
// this package only commits to the one method it actually calls.
// *instrument.Store satisfies it structurally; callers pass it in with no
// conversion needed.
type instrumentLister interface {
	ListTradable(ctx context.Context) ([]instrument.Instrument, error)
}

// fxWorker refreshes today's FX rates via FxProvider and stores them.
type fxWorker struct {
	river.WorkerDefaults[RefreshFxArgs]
	store    *Store
	provider FxProvider
	log      *slog.Logger
}

// NewFxWorker builds the River worker that refreshes daily FX rates via
// provider and upserts them into store. Register it with river.AddWorker.
func NewFxWorker(store *Store, provider FxProvider, log *slog.Logger) river.Worker[RefreshFxArgs] {
	return &fxWorker{store: store, provider: provider, log: log}
}

// Work fetches today's rates and upserts them. A provider error is returned
// (not swallowed) so River retries the job per its backoff policy; it is
// also logged here since River's own failure log lacks provider context.
func (w *fxWorker) Work(ctx context.Context, _ *river.Job[RefreshFxArgs]) error {
	on := time.Now().UTC()
	rates, err := w.provider.RatesOn(ctx, on)
	if err != nil {
		w.log.Error("marketdata: fetch fx rates failed", "provider", w.provider.Name(), "err", err)
		return err
	}
	if err := w.store.UpsertFxRates(ctx, rates); err != nil {
		w.log.Error("marketdata: store fx rates failed", "provider", w.provider.Name(), "err", err)
		return err
	}
	w.log.Info("marketdata: refreshed fx rates", "provider", w.provider.Name(), "count", len(rates))
	return nil
}

// quotesWorker refreshes quotes for every tradable instrument via
// QuoteProvider and stores them.
type quotesWorker struct {
	river.WorkerDefaults[RefreshQuotesArgs]
	store       *Store
	instruments instrumentLister
	provider    QuoteProvider
	log         *slog.Logger
}

// NewQuotesWorker builds the River worker that refreshes quotes, via
// provider, for every tradable instrument (share/bond/etf with a ticker)
// and upserts them into store. Register it with river.AddWorker.
func NewQuotesWorker(store *Store, instruments instrumentLister, provider QuoteProvider, log *slog.Logger) river.Worker[RefreshQuotesArgs] {
	return &quotesWorker{store: store, instruments: instruments, provider: provider, log: log}
}

// Work looks up the tradable instrument catalog, maps ticker -> InstrumentID
// (the provider knows nothing about our catalog, so this mapping is the
// worker's job), fetches prices, and upserts the ones the provider actually
// reported. Instruments the provider has no price for are skipped with a
// debug log — that's an expected, routine condition (e.g. newly listed or
// suspended instruments), not an error. A provider error is returned (not
// swallowed) so River retries the job.
func (w *quotesWorker) Work(ctx context.Context, _ *river.Job[RefreshQuotesArgs]) error {
	insts, err := w.instruments.ListTradable(ctx)
	if err != nil {
		w.log.Error("marketdata: list tradable instruments failed", "err", err)
		return err
	}
	if len(insts) == 0 {
		w.log.Debug("marketdata: no tradable instruments, skipping quotes refresh")
		return nil
	}

	byTicker := make(map[string]uuid.UUID, len(insts))
	tickers := make([]string, 0, len(insts))
	for _, inst := range insts {
		byTicker[inst.Ticker] = inst.ID
		tickers = append(tickers, inst.Ticker)
	}

	on := time.Now().UTC()
	tickerQuotes, err := w.provider.QuotesFor(ctx, tickers, on)
	if err != nil {
		w.log.Error("marketdata: fetch quotes failed", "provider", w.provider.Name(), "err", err)
		return err
	}

	seen := make(map[string]bool, len(tickerQuotes))
	quotes := make([]Quote, 0, len(tickerQuotes))
	for _, tq := range tickerQuotes {
		id, ok := byTicker[tq.Ticker]
		if !ok {
			// Provider reported a ticker we didn't ask about; ignore rather
			// than fail the whole batch over it.
			continue
		}
		seen[tq.Ticker] = true
		quotes = append(quotes, Quote{
			InstrumentID: id,
			On:           tq.On,
			Price:        tq.Price,
			Currency:     tq.Currency,
			Source:       w.provider.Name(),
		})
	}
	for _, t := range tickers {
		if !seen[t] {
			w.log.Debug("marketdata: no price for ticker, skipping", "ticker", t)
		}
	}

	if err := w.store.UpsertQuotes(ctx, quotes); err != nil {
		w.log.Error("marketdata: store quotes failed", "err", err)
		return err
	}
	w.log.Info("marketdata: refreshed quotes",
		"provider", w.provider.Name(), "requested", len(tickers), "matched", len(quotes))
	return nil
}

// backfillFxWorker fills in historical FX rates, one chunk per job, walking
// the calendar downwards from the oldest rate we already have towards the
// oldest operation in the journal.
type backfillFxWorker struct {
	river.WorkerDefaults[BackfillFxArgs]
	store    *Store
	ops      operationDater
	provider FxProvider
	log      *slog.Logger
	pause    time.Duration
	// now stands in for time.Now so tests can pin "today" instead of racing
	// the wall clock; see coverageCursor.
	now func() time.Time
}

// NewBackfillFxWorker builds the River worker that backfills historical FX
// rates via provider and upserts them into store; ops supplies the oldest
// date the journal needs rates for. Register it with river.AddWorker.
func NewBackfillFxWorker(store *Store, ops operationDater, provider FxProvider, log *slog.Logger) river.Worker[BackfillFxArgs] {
	return &backfillFxWorker{store: store, ops: ops, provider: provider, log: log, pause: backfillPause, now: time.Now}
}

// Timeout raises River's one-minute default, which a full chunk exceeds on
// its pauses alone.
func (w *backfillFxWorker) Timeout(*river.Job[BackfillFxArgs]) time.Duration {
	return backfillChunkTimeout
}

// Work fetches one chunk of history and, if the walk isn't finished, queues
// the next chunk. Provider and store errors are returned so River retries;
// a failure to queue the successor is not, see enqueueNext.
func (w *backfillFxWorker) Work(ctx context.Context, _ *river.Job[BackfillFxArgs]) error {
	floor, wanted, err := w.demandFloor(ctx)
	if err != nil || !wanted {
		return err
	}
	cursor, err := w.coverageCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.Before(floor) {
		w.log.Debug("marketdata: fx history already reaches the journal, nothing to backfill",
			"provider", w.provider.Name(), "floor", floor.Format(time.DateOnly))
		return nil
	}
	// startBoundary is the coverage boundary this chunk needs to push
	// earlier. cursor is always "one day below that boundary" at this point
	// (coverageCursor either returns haveFrom-1, or today when there is no
	// coverage yet — in which case startBoundary works out to today+1, which
	// any date this run could possibly fetch is trivially earlier than), so
	// adding the day back reconstructs it without a second store read.
	startBoundary := cursor.AddDate(0, 0, 1)
	cursor, err = w.fetchChunk(ctx, cursor, floor)
	if err != nil {
		return err
	}
	if cursor.Before(floor) {
		w.log.Info("marketdata: fx backfill reached the floor",
			"provider", w.provider.Name(), "floor", floor.Format(time.DateOnly))
		return nil
	}
	advanced, err := w.coverageAdvanced(ctx, startBoundary)
	if err != nil {
		return err
	}
	if !advanced {
		// A provider that answers every request with the same On date (never
		// later than requested — cbr.ru can't actually do this) would
		// otherwise make every future run re-request the same dates forever.
		// That can't happen against the real source, but the cost of trusting
		// it blindly is an unbounded stream of requests to an external
		// service, so it gets a guard and a loud log line instead.
		w.log.Warn("marketdata: fx backfill chunk made no coverage progress, not enqueuing a follow-up (provider stuck?)",
			"provider", w.provider.Name(), "boundary", startBoundary.Format(time.DateOnly))
		return nil
	}
	w.enqueueNext(ctx, cursor, floor)
	return nil
}

// coverageAdvanced reports whether this chunk pushed the stored coverage
// boundary for w.provider earlier than startBoundary. pgx.ErrNoRows (nothing
// was actually stored despite the chunk running, e.g. the provider answered
// every request with an empty rate list) counts as "not advanced" rather
// than an error, since there is nothing wrong with the job itself.
func (w *backfillFxWorker) coverageAdvanced(ctx context.Context, startBoundary time.Time) (bool, error) {
	newBoundary, err := w.store.EarliestFxDate(ctx, w.provider.Name())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		w.log.Error("marketdata: read fx coverage boundary failed",
			"provider", w.provider.Name(), "err", err)
		return false, err
	}
	return utcDay(newBoundary).Before(startBoundary), nil
}

// demandFloor is the oldest date the journal needs rates for: the earliest
// operation, clamped to backfillFloor. wanted is false when there are no
// operations at all — then there is nothing to backfill for and the provider
// is not called even once.
func (w *backfillFxWorker) demandFloor(ctx context.Context) (time.Time, bool, error) {
	earliest, err := w.ops.EarliestOccurredOn(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log.Debug("marketdata: no operations yet, skipping fx backfill")
		return time.Time{}, false, nil
	}
	if err != nil {
		w.log.Error("marketdata: read earliest operation date failed", "err", err)
		return time.Time{}, false, err
	}
	floor := utcDay(earliest)
	if floor.Before(backfillFloor) {
		w.log.Warn("marketdata: earliest operation predates the fx backfill floor, clamping (most likely a mistyped date)",
			"provider", w.provider.Name(),
			"earliest_operation", floor.Format(time.DateOnly),
			"floor", backfillFloor.Format(time.DateOnly),
			"days_dropped", daysBetween(floor, backfillFloor))
		floor = backfillFloor
	}
	return floor, true, nil
}

// coverageCursor is the date this chunk starts at: one day below the oldest
// rate this provider has already delivered, or today when there is none.
func (w *backfillFxWorker) coverageCursor(ctx context.Context) (time.Time, error) {
	haveFrom, err := w.store.EarliestFxDate(ctx, w.provider.Name())
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return utcDay(w.now().UTC()), nil
	case err != nil:
		w.log.Error("marketdata: read fx coverage boundary failed",
			"provider", w.provider.Name(), "err", err)
		return time.Time{}, err
	}
	return utcDay(haveFrom).AddDate(0, 0, -1), nil
}

// fetchChunk walks the calendar down from cursor towards floor, fetching and
// storing rates for at most backfillChunkDays dates, and returns the cursor
// it stopped at.
//
// Walking *down* is what keeps the coverage boundary honest: the boundary is
// MIN(on_date), which means "nothing older exists" only while coverage grows
// downwards without gaps. Filling upwards from the oldest operation would
// drop MIN(on_date) after the very first chunk, hiding the hole left in the
// middle and making the next run believe the work is done.
//
// Weekends are skipped by *requested* date; holidays are not, because they
// can't be told apart up front. The source answers a non-working day with
// the previous working day's rates, and re-upserting those is harmless.
func (w *backfillFxWorker) fetchChunk(ctx context.Context, cursor, floor time.Time) (time.Time, error) {
	fetched := 0
	for fetched < backfillChunkDays && !cursor.Before(floor) {
		if isWeekend(cursor) {
			cursor = cursor.AddDate(0, 0, -1)
			continue
		}
		if fetched > 0 {
			if err := wait(ctx, w.pause); err != nil {
				return cursor, err
			}
		}
		rates, err := w.provider.RatesOn(ctx, cursor)
		if err != nil {
			w.log.Error("marketdata: backfill fetch fx rates failed",
				"provider", w.provider.Name(), "on", cursor.Format(time.DateOnly), "err", err)
			return cursor, err
		}
		if err := w.store.UpsertFxRates(ctx, rates); err != nil {
			w.log.Error("marketdata: backfill store fx rates failed",
				"provider", w.provider.Name(), "on", cursor.Format(time.DateOnly), "err", err)
			return cursor, err
		}
		fetched++
		// The cursor moves by requested date, never by the date carried in
		// the response: the source dates a non-working day's answer with an
		// earlier publication date, and following that would make the walk
		// stall or skip dates.
		cursor = cursor.AddDate(0, 0, -1)
	}
	w.log.Info("marketdata: fx backfill chunk done",
		"provider", w.provider.Name(), "fetched", fetched,
		"next_from", cursor.Format(time.DateOnly))
	return cursor, nil
}

// enqueueNext queues the follow-up chunk. The River client is taken from the
// job context because the client is built *from* the workers, so it cannot
// be injected into this worker's constructor.
//
// Neither a missing client nor a failed insert fails the job: the chunk
// itself succeeded, and returning an error would make River re-fetch a
// couple of hundred dates that are already stored. The daily periodic job
// picks the remainder up either way.
func (w *backfillFxWorker) enqueueNext(ctx context.Context, cursor, floor time.Time) {
	remaining := businessDays(floor, cursor)
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		w.log.Warn("marketdata: no river client in context, next fx backfill chunk not enqueued",
			"remaining_dates", remaining, "err", err)
		return
	}
	if _, err := client.Insert(ctx, BackfillFxArgs{}, nil); err != nil {
		w.log.Error("marketdata: enqueue next fx backfill chunk failed",
			"remaining_dates", remaining, "err", err)
		return
	}
	w.log.Info("marketdata: enqueued the next fx backfill chunk",
		"remaining_dates", remaining, "next_from", cursor.Format(time.DateOnly))
}

// wait pauses for d, or returns ctx's error as soon as the context is done.
// A plain time.Sleep would keep a shutdown waiting for the rest of a chunk.
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// utcDay strips the time of day, so dates coming from the clock compare
// exactly with the midnight-UTC dates coming out of Postgres DATE columns.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func isWeekend(d time.Time) bool {
	return d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
}

// daysBetween counts calendar days from -> to. Both are midnight UTC, which
// has no DST jumps, so plain division is exact.
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}

// businessDays counts the Mon-Fri days in [from, to] — the dates a backfill
// walk over that range would actually request.
func businessDays(from, to time.Time) int {
	n := 0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if !isWeekend(d) {
			n++
		}
	}
	return n
}
