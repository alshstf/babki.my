package tinvest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
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

// A paper the space holds that no import mapped — Coca-Cola entered by hand
// for another broker — gets its history from the listing the price worker
// finds for it by ISIN: still quoted, in the holding's own currency. A paper
// with no ISIN is not searched for at all.
func TestBrokerCandlesFillTheHistoryOfAPaperNoImportMapped(t *testing.T) {
	f := newQuotesFixture(t)
	ko, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Coca-Cola", Ticker: "KO", ISIN: "US1912161007", Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	noISIN := f.instrument(t, "NOISIN", "USD")
	svc := operation.NewService(operation.NewStore(f.pool))
	for _, paper := range []uuid.UUID{ko.ID, noISIN.ID} {
		qty, price := decimal.NewFromInt(2), decimal.NewFromInt(60)
		id := paper
		if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &id, Type: operation.TypeBuy,
			OccurredOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Quantity: &qty, Price: &price,
			AmountMinor: -12_000, Currency: "USD",
		}); err != nil {
			t.Fatalf("buy: %v", err)
		}
	}
	f.broker.answer("InstrumentsService/FindInstrument", 200, `{"instruments":[
		{"uid":"uid-ko","isin":"US1912161007","ticker":"KO","classCode":"SPBXM","instrumentKind":"INSTRUMENT_TYPE_SHARE"}
	]}`)
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(lastPrice("uid-ko", "61", 500000000, "2026-08-07T20:00:00Z", "LAST_PRICE_EXCHANGE")))
	f.broker.answer("InstrumentsService/GetInstrumentBy", 200, `{"instrument":{"uid":"uid-ko","isin":"US1912161007","ticker":"KO","currency":"usd"}}`)
	f.broker.answer(rpcCandles, 200, `{"priceCurrency":"usd","candles":[
		{"close":{"units":"60","nano":0},"time":"2026-07-21T00:00:00Z","isComplete":true}
	]}`)

	store := &historyStore{}
	log := slog.New(f.logs)
	newClient := func(token string) (*Client, error) {
		return NewClient(f.broker.srv.Client(), f.broker.srv.URL, token, log), nil
	}
	w := NewBackfillQuotesWorker(f.store, store, opsFirst{
		ko.ID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), noISIN.ID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
	}, f.sealer, newClient, log).(*backfillQuotesWorker)
	w.now = func() time.Time { return f.now }
	if err := w.Work(f.ctx, &river.Job[BackfillQuotesArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}

	if len(store.stored) != 1 {
		t.Fatalf("stored %+v, want Coca-Cola's one closed day", store.stored)
	}
	if q := store.stored[0]; q.InstrumentID != ko.ID || q.Currency != "USD" || q.Source != HistorySource || q.Price.String() != "60" {
		t.Errorf("stored %+v, want Coca-Cola at 60 $ from the broker's history", q)
	}
	if n := f.broker.callCount("InstrumentsService/FindInstrument"); n != 1 {
		t.Errorf("searched %d times, want once — the paper with no ISIN is not searched for", n)
	}
}

// The listing found for a paper no import mapped is in rubles while the
// holding is kept in dollars: its candles are not this holding's prices, and
// none are asked for.
func TestNoHistoryIsTakenFromAListingInAnotherCurrency(t *testing.T) {
	f := newQuotesFixture(t)
	ko, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Coca-Cola", Ticker: "KO", ISIN: "US1912161007", Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	qty, price := decimal.NewFromInt(2), decimal.NewFromInt(60)
	if _, err := operation.NewService(operation.NewStore(f.pool)).Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &ko.ID, Type: operation.TypeBuy,
		OccurredOn: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), Quantity: &qty, Price: &price,
		AmountMinor: -12_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	f.broker.answer("InstrumentsService/FindInstrument", 200, `{"instruments":[
		{"uid":"uid-ko-rub","isin":"US1912161007","ticker":"KO-RM","classCode":"FQBR","instrumentKind":"INSTRUMENT_TYPE_SHARE"}
	]}`)
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(lastPrice("uid-ko-rub", "5000", 0, "2026-08-07T20:00:00Z", "LAST_PRICE_EXCHANGE")))
	f.broker.answer("InstrumentsService/GetInstrumentBy", 200, `{"instrument":{"uid":"uid-ko-rub","isin":"US1912161007","ticker":"KO-RM","currency":"rub"}}`)

	store := &historyStore{}
	log := slog.New(f.logs)
	newClient := func(token string) (*Client, error) {
		return NewClient(f.broker.srv.Client(), f.broker.srv.URL, token, log), nil
	}
	w := NewBackfillQuotesWorker(f.store, store, opsFirst{ko.ID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)},
		f.sealer, newClient, log).(*backfillQuotesWorker)
	w.now = func() time.Time { return f.now }
	if err := w.Work(f.ctx, &river.Job[BackfillQuotesArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) != 0 || f.broker.callCount(rpcCandles) != 0 {
		t.Errorf("stored %+v after %d candle requests, want nothing asked and nothing stored", store.stored, f.broker.callCount(rpcCandles))
	}
}
