package marketdata_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

// fakeFxProvider is a network-free stand-in for marketdata.FxProvider.
type fakeFxProvider struct {
	rates []marketdata.FxRate
	err   error
}

func (p fakeFxProvider) RatesOn(context.Context, time.Time) ([]marketdata.FxRate, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.rates, nil
}

func (p fakeFxProvider) Name() string { return "fake-fx" }

// fakeQuoteProvider is a network-free stand-in for marketdata.QuoteProvider.
type fakeQuoteProvider struct {
	quotes []marketdata.TickerQuote
	err    error
	// calls records the ticker slices this provider was invoked with, so
	// tests can assert it was (or wasn't) called at all.
	calls *[][]string
}

func (p fakeQuoteProvider) QuotesFor(_ context.Context, tickers []string, _ time.Time) ([]marketdata.TickerQuote, error) {
	if p.calls != nil {
		*p.calls = append(*p.calls, tickers)
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.quotes, nil
}

func (p fakeQuoteProvider) Name() string { return "fake-quotes" }

func newJobsFixture(t *testing.T) (*marketdata.Store, *instrument.Store, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return marketdata.NewStore(pool), instrument.NewStore(pool), ctx
}

func TestFxWorker_UpsertsRatesFromProvider(t *testing.T) {
	store, _, ctx := newJobsFixture(t)

	provider := fakeFxProvider{rates: []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-25"), Rate: dec("90.5"), Source: "fake-fx"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-25"), Rate: dec("98.0"), Source: "fake-fx"},
	}}
	worker := marketdata.NewFxWorker(store, provider, slog.Default())

	err := worker.Work(ctx, &river.Job[marketdata.RefreshFxArgs]{Args: marketdata.RefreshFxArgs{}})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	got, err := store.FxRateOn(ctx, "USD", "RUB", date("2026-07-25"))
	if err != nil {
		t.Fatalf("FxRateOn: %v", err)
	}
	if !got.Rate.Equal(dec("90.5")) {
		t.Fatalf("rate = %s, want 90.5", got.Rate)
	}

	latest, err := store.LatestFxRates(ctx)
	if err != nil {
		t.Fatalf("LatestFxRates: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("LatestFxRates len = %d, want 2: %+v", len(latest), latest)
	}
}

func TestFxWorker_ProviderErrorReturnsFromWork(t *testing.T) {
	store, _, ctx := newJobsFixture(t)

	wantErr := errors.New("cbr unreachable")
	worker := marketdata.NewFxWorker(store, fakeFxProvider{err: wantErr}, slog.Default())

	err := worker.Work(ctx, &river.Job[marketdata.RefreshFxArgs]{Args: marketdata.RefreshFxArgs{}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v", err, wantErr)
	}
}

func TestQuotesWorker_UpsertsMatchedTickersAndSkipsMissingPrices(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}
	gazp, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Газпром", Ticker: "GAZP", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create gazp: %v", err)
	}
	// yndx is tradable (has a ticker) but the provider won't report a price
	// for it — must be skipped, not treated as an error.
	yndx, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Яндекс", Ticker: "YNDX", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create yndx: %v", err)
	}
	// non-tradable: no ticker, must not even be requested from the provider.
	if _, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Без тикера", Currency: "RUB",
	}); err != nil {
		t.Fatalf("create tickerless: %v", err)
	}

	var calls [][]string
	provider := fakeQuoteProvider{
		calls: &calls,
		quotes: []marketdata.TickerQuote{
			{Ticker: "SBER", Price: dec("305.5"), Currency: "RUB", On: date("2026-07-25")},
			{Ticker: "GAZP", Price: dec("150.0"), Currency: "RUB", On: date("2026-07-25")},
		},
	}
	worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())

	err = worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("provider called %d times, want 1", len(calls))
	}
	requested := map[string]bool{}
	for _, t := range calls[0] {
		requested[t] = true
	}
	if len(calls[0]) != 3 || !requested["SBER"] || !requested["GAZP"] || !requested["YNDX"] {
		t.Fatalf("requested tickers = %v, want exactly SBER, GAZP, YNDX (tickerless instrument excluded)", calls[0])
	}

	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID, gazp.ID, yndx.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("LatestQuotes len = %d, want 2 (yndx has no price): %+v", len(latest), latest)
	}
	if _, ok := latest[yndx.ID]; ok {
		t.Fatalf("yndx should have no quote: %+v", latest[yndx.ID])
	}
	if q, ok := latest[sber.ID]; !ok || !q.Price.Equal(dec("305.5")) {
		t.Fatalf("sber quote = %+v", q)
	}
}

func TestQuotesWorker_NoTradableInstrumentsSkipsProviderCall(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	var calls [][]string
	provider := fakeQuoteProvider{calls: &calls, err: errors.New("must not be called")}
	worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())

	err := worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("provider called %d times, want 0 when there are no tradable instruments", len(calls))
	}
}

func TestQuotesWorker_ProviderErrorReturnsFromWork(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	if _, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	}); err != nil {
		t.Fatalf("create sber: %v", err)
	}

	wantErr := errors.New("moex unreachable")
	worker := marketdata.NewQuotesWorker(store, instStore, fakeQuoteProvider{err: wantErr}, slog.Default())

	err := worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v", err, wantErr)
	}
}
