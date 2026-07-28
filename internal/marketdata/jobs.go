package marketdata

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
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
