package tinvest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/marketdata"
)

const rpcCandles = "MarketDataService/GetCandles"

type historyStore struct {
	recordingQuotes
	coverage map[string]map[uuid.UUID]time.Time // source → instrument → last day
}

func (h *historyStore) HistoryCoverage(_ context.Context, _ []uuid.UUID, source string) (map[uuid.UUID]time.Time, error) {
	if h.coverage[source] == nil {
		return map[uuid.UUID]time.Time{}, nil
	}
	return h.coverage[source], nil
}

type opsFirst map[uuid.UUID]time.Time

func (o opsFirst) FirstDaysByInstrument(context.Context) (map[uuid.UUID]time.Time, error) {
	return o, nil
}

// The broker's daily candles fill in the history of the listings a connection
// holds that the exchange does not price — Apple on the СПБ, in dollars — from
// a month before the first operation; a paper the exchange already covers is
// left to it, a day not yet closed is not a price, and candles in a currency
// other than the listing's are not stored.
func TestBrokerCandlesFillTheHistoryTheExchangeLacks(t *testing.T) {
	f := newQuotesFixture(t)
	apple := f.instrument(t, "AAPL", "USD")
	sber := f.instrument(t, "SBER", "RUB")
	tesla := f.instrument(t, "TSLA", "USD")
	f.mapTo(t, "uid-aapl", apple.ID, "USD")
	f.mapTo(t, "uid-sber", sber.ID, "RUB")
	f.mapTo(t, "uid-tsla", tesla.ID, "EUR")
	f.broker.answer(rpcCandles, 200, `{"priceCurrency":"usd","candles":[
		{"close":{"units":"338","nano":980000000},"time":"2026-07-21T00:00:00Z","isComplete":true},
		{"close":{"units":"339","nano":750000000},"time":"2026-07-22T00:00:00Z","isComplete":true},
		{"close":{"units":"340","nano":0},"time":"2026-08-08T00:00:00Z","isComplete":false}
	]}`)

	store := &historyStore{coverage: map[string]map[uuid.UUID]time.Time{
		marketdata.QuoteHistorySource: {sber.ID: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)},
	}}
	log := slog.New(f.logs)
	newClient := func(token string) (*Client, error) {
		return NewClient(f.broker.srv.Client(), f.broker.srv.URL, token, log), nil
	}
	w := NewBackfillQuotesWorker(f.store, store, opsFirst{
		apple.ID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		sber.ID:  time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		tesla.ID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
	}, f.sealer, newClient, log).(*backfillQuotesWorker)
	w.now = func() time.Time { return f.now }
	if err := w.Work(f.ctx, &river.Job[BackfillQuotesArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}

	if len(store.stored) != 2 {
		t.Fatalf("stored %+v, want Apple's two closed days", store.stored)
	}
	for _, q := range store.stored {
		if q.InstrumentID != apple.ID || q.Currency != "USD" || q.Source != HistorySource {
			t.Errorf("stored %+v, want Apple in dollars from the broker's history", q)
		}
	}
	// Apple once, Tesla once (refused for its currency); Sberbank not at all.
	if n := f.broker.callCount(rpcCandles); n != 2 {
		t.Errorf("asked for candles %d times, want 2", n)
	}
}
