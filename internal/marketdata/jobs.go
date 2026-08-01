package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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

// BackfillFxArgs triggers a download of the whole FX rate history the
// journal needs: for every currency actually in use, one request covering the
// entire range from the oldest operation to today.
type BackfillFxArgs struct{}

func (BackfillFxArgs) Kind() string { return "marketdata.backfill_fx" }

// quoteCurrency is the currency every stored rate is quoted in ("USD -> RUB"
// and so on). It is never downloaded: the rouble against itself is not a
// rate, and the source does not quote it.
const quoteCurrency = "RUB"

// backfillTimeout overrides River's one-minute default job timeout. A run
// makes one request per currency in use — under a dozen in practice, and
// bounded by the fifty-odd currencies the source quotes at all — but each
// answer is a whole multi-year series, megabytes of XML to transfer and
// parse. At the 15s per-request timeout cmd/babki gives the cbr client, this
// leaves room for far more currencies than any instance will have.
const backfillTimeout = 15 * time.Minute

// backfillFloor is the earliest date worth fetching rates for. An operation
// dated before it is far more likely to be a mistyped date than real
// history, and honouring it would mean asking for decades of series nothing
// will ever be converted at.
var backfillFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// operationCurrencies is the subset of operation.Store the backfill worker
// needs — the same locally declared interface pattern as instrumentLister
// below, which additionally keeps marketdata free of an import of the
// operation package. *operation.Store satisfies it structurally.
type operationCurrencies interface {
	EarliestOccurredOn(ctx context.Context) (time.Time, error)
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

// accountCurrencies is the subset of account.Store the backfill worker needs;
// *account.Store satisfies it structurally.
type accountCurrencies interface {
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

// spaceCurrencies is the subset of family.Store the backfill worker needs;
// *family.Store satisfies it structurally.
type spaceCurrencies interface {
	DistinctBaseCurrencies(ctx context.Context) ([]string, error)
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

// backfillFxWorker downloads the FX rate history the journal needs: the
// whole range at once, one request per currency in use.
type backfillFxWorker struct {
	river.WorkerDefaults[BackfillFxArgs]
	store    *Store
	ops      operationCurrencies
	accounts accountCurrencies
	spaces   spaceCurrencies
	provider FxHistoryProvider
	log      *slog.Logger
	// now stands in for time.Now so tests can pin "today" — the upper end of
	// every requested range — instead of racing the wall clock.
	now func() time.Time
}

// NewBackfillFxWorker builds the River worker that downloads historical FX
// rates via provider and upserts them into store. ops supplies the oldest
// date the journal needs rates for; ops, accounts and spaces together supply
// the currencies those rates are needed in. Register it with river.AddWorker.
func NewBackfillFxWorker(
	store *Store,
	ops operationCurrencies,
	accounts accountCurrencies,
	spaces spaceCurrencies,
	provider FxHistoryProvider,
	log *slog.Logger,
) river.Worker[BackfillFxArgs] {
	return &backfillFxWorker{
		store: store, ops: ops, accounts: accounts, spaces: spaces,
		provider: provider, log: log, now: time.Now,
	}
}

// Timeout raises River's one-minute default; see backfillTimeout.
func (w *backfillFxWorker) Timeout(*river.Job[BackfillFxArgs]) time.Duration {
	return backfillTimeout
}

// Work downloads every currency's whole series and stores it. There is no
// bookkeeping of what is already covered and no follow-up job: a run always
// asks for the full range, which costs one request per currency and heals
// any hole an outage may have left. Re-running simply overwrites the same
// rows.
//
// A provider or store error fails the job so River retries it; whatever was
// stored for the currencies handled before the failure stays in the database.
func (w *backfillFxWorker) Work(ctx context.Context, _ *river.Job[BackfillFxArgs]) error {
	from, wanted, err := w.rangeStart(ctx)
	if err != nil || !wanted {
		return err
	}
	codes, err := w.wantedCurrencies(ctx)
	if err != nil {
		return err
	}
	if len(codes) == 0 {
		w.log.Debug("marketdata: nothing but the quote currency is in use, skipping fx backfill",
			"quote", quoteCurrency)
		return nil
	}

	to := utcDay(w.now())
	if from.After(to) {
		// Only reachable from an operation dated in the future — a typo, most
		// likely. Left alone it would make every run ask for a backwards
		// range, which the source rejects, so the job would fail forever
		// instead of keeping the rest of the history fresh.
		w.log.Warn("marketdata: earliest operation is in the future, fetching today only",
			"earliest_operation", from.Format(time.DateOnly), "today", to.Format(time.DateOnly))
		from = to
	}

	ids, err := w.provider.CurrencyIDs(ctx)
	if err != nil {
		w.log.Error("marketdata: fetch currency ids failed", "provider", w.provider.Name(), "err", err)
		return err
	}

	for _, code := range codes {
		id, ok := ids[code]
		if !ok {
			// The source doesn't quote this currency, so there is no series to
			// ask for. Operations in it stay unconverted, which is the honest
			// outcome — but it must be visible, not silent.
			w.log.Warn("marketdata: source does not quote currency, skipping it (its amounts stay unconverted)",
				"provider", w.provider.Name(), "currency", code)
			continue
		}
		rates, err := w.provider.RatesRange(ctx, code, id, from, to)
		if err != nil {
			w.log.Error("marketdata: fetch fx history failed",
				"provider", w.provider.Name(), "currency", code,
				"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "err", err)
			return err
		}
		if err := w.store.UpsertFxRates(ctx, rates); err != nil {
			w.log.Error("marketdata: store fx history failed",
				"provider", w.provider.Name(), "currency", code, "err", err)
			return err
		}
		w.log.Info("marketdata: downloaded fx history",
			"provider", w.provider.Name(), "currency", code,
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "rates", len(rates))
	}
	return nil
}

// rangeStart is the oldest date the journal needs rates for: the earliest
// operation, clamped to backfillFloor. wanted is false when there are no
// operations at all — then no amount needs converting at any past date, and
// the source is not contacted even once.
func (w *backfillFxWorker) rangeStart(ctx context.Context) (time.Time, bool, error) {
	earliest, err := w.ops.EarliestOccurredOn(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log.Debug("marketdata: no operations yet, skipping fx backfill")
		return time.Time{}, false, nil
	}
	if err != nil {
		w.log.Error("marketdata: read earliest operation date failed", "err", err)
		return time.Time{}, false, err
	}
	from := utcDay(earliest)
	if from.Before(backfillFloor) {
		w.log.Warn("marketdata: earliest operation predates the fx backfill floor, clamping (most likely a mistyped date)",
			"provider", w.provider.Name(),
			"earliest_operation", from.Format(time.DateOnly),
			"floor", backfillFloor.Format(time.DateOnly),
			"days_dropped", daysBetween(from, backfillFloor))
		from = backfillFloor
	}
	return from, true, nil
}

// wantedCurrencies is the sorted set of currencies rates are needed for:
// everything accounts are denominated in, everything operations are recorded
// in, and every space's base currency — minus quoteCurrency, which rates are
// quoted against rather than fetched for.
func (w *backfillFxWorker) wantedCurrencies(ctx context.Context) ([]string, error) {
	accountCodes, err := w.accounts.DistinctCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read account currencies failed", "err", err)
		return nil, err
	}
	operationCodes, err := w.ops.DistinctCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read operation currencies failed", "err", err)
		return nil, err
	}
	baseCodes, err := w.spaces.DistinctBaseCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read space base currencies failed", "err", err)
		return nil, err
	}

	total := len(accountCodes) + len(operationCodes) + len(baseCodes)
	seen := make(map[string]bool, total)
	out := make([]string, 0, total)
	for _, codes := range [][]string{accountCodes, operationCodes, baseCodes} {
		for _, code := range codes {
			if code == quoteCurrency || seen[code] {
				continue
			}
			seen[code] = true
			out = append(out, code)
		}
	}
	// Sorted so a run's requests, and its log, come out in a stable order
	// whichever list a currency first appeared in.
	slices.Sort(out)
	return out, nil
}

// utcDay strips the time of day, so dates coming from the clock compare
// exactly with the midnight-UTC dates coming out of Postgres DATE columns.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// daysBetween counts calendar days from -> to. Both are midnight UTC, which
// has no DST jumps, so plain division is exact.
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}
