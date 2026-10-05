package marketdata_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/logtest"
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
	// calls records each request's tickers.
	calls *[][]string
}

func (p fakeQuoteProvider) QuotesFor(_ context.Context, tickers []string) ([]marketdata.TickerQuote, error) {
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

// A non-positive rate is dropped and the rest are stored (#28): one bad row
// used to fail the whole batch, forever. Both halves are asserted.
func TestFxWorker_NonPositiveRateIsDroppedAndTheRestIsStored(t *testing.T) {
	store, _, ctx := newJobsFixture(t)
	on := date("2026-07-25")
	logs := &logtest.Capture{}
	log := logs.Logger()

	// Zero and negative: both refused by the CHECK, both possible from a source.
	provider := fakeFxProvider{rates: []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90.5"), Source: "fake-fx"},
		{Base: "XXX", Quote: "RUB", On: on, Rate: dec("0"), Source: "fake-fx"},
		{Base: "YYY", Quote: "RUB", On: on, Rate: dec("-1.5"), Source: "fake-fx"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: dec("98.0"), Source: "fake-fx"},
	}}
	worker := marketdata.NewFxWorker(store, provider, log)

	if err := worker.Work(ctx, &river.Job[marketdata.RefreshFxArgs]{Args: marketdata.RefreshFxArgs{}}); err != nil {
		t.Fatalf("Work: %v, want one unusable rate not to cost the whole day's set", err)
	}

	for _, base := range []string{"USD", "EUR"} {
		if _, err := store.FxRateOn(ctx, base, "RUB", on); err != nil {
			t.Fatalf("FxRateOn(%s): %v, want the sound rates stored regardless of the unusable ones", base, err)
		}
	}
	for _, base := range []string{"XXX", "YYY"} {
		if got, err := store.FxRateOn(ctx, base, "RUB", on); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("FxRateOn(%s) = %+v, err = %v, want pgx.ErrNoRows: a rate the table refuses must not be stored", base, got, err)
		}
	}

	// Each drop is a Warn line naming the pair and value.
	dropped := logs.Lines(droppedRateMsg)
	if len(dropped) != 2 {
		t.Fatalf("%d dropped-rate lines, want 2 (one per dropped rate):\n%s", len(dropped), logtest.Describe(logs.Records()))
	}
	for _, line := range dropped {
		if line.Level != slog.LevelWarn {
			t.Fatalf("dropped-rate line at %s, want WARN:\n%s", line.Level, logtest.Describe(logs.Records()))
		}
	}
	if bases := []string{logtest.Attr(dropped[0], "base"), logtest.Attr(dropped[1], "base")}; !slices.Equal(bases, []string{"XXX", "YYY"}) {
		t.Fatalf("dropped-rate lines name %v, want [XXX YYY]:\n%s", bases, logtest.Describe(logs.Records()))
	}
	if got := logtest.Attr(dropped[0], "rate"); got != "0" {
		t.Fatalf("dropped-rate line for XXX says rate=%q, want the value that was refused:\n%s", got, logtest.Describe(logs.Records()))
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
	// yndx is tradable but unpriced: skipped, not an error.
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

// A quote is stored under the day the provider named, not the clock's (#90);
// the date cannot come from any clock.
func TestQuotesWorker_StoresTheDayTheProviderNamed(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}

	session := date("2026-07-24")
	provider := fakeQuoteProvider{quotes: []marketdata.TickerQuote{
		{Ticker: "SBER", Price: dec("276.52"), Currency: "RUB", On: session},
	}}
	worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())
	if err := worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	q, ok := latest[sber.ID]
	if !ok {
		t.Fatalf("LatestQuotes has no quote for sber: %+v", latest)
	}
	if !q.On.Equal(session) {
		t.Errorf("stored quote is dated %s, want %s — the day the exchange named, not the day of the refresh",
			q.On.Format(time.DateOnly), session.Format(time.DateOnly))
	}

	// The row is at that date: the day before has no quote.
	if _, err := store.QuoteOn(ctx, sber.ID, session.AddDate(0, 0, -1)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("QuoteOn(the day before the session) err = %v, want pgx.ErrNoRows: "+
			"a quote must not exist on a day before the session it belongs to", err)
	}
}

// Repeat refreshes rewrite one row per session; a new row appears with a new
// session. LatestQuotes follows the newest; QuoteOn still finds the older.
func TestQuotesWorker_RowsFollowTheExchangesSessionsNotTheRefreshes(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store, instStore := marketdata.NewStore(pool), instrument.NewStore(pool)

	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}

	rows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE instrument_id = $1`, sber.ID).Scan(&n); err != nil {
			t.Fatalf("count quotes: %v", err)
		}
		return n
	}
	refresh := func(price string, on time.Time) {
		t.Helper()
		provider := fakeQuoteProvider{quotes: []marketdata.TickerQuote{
			{Ticker: "SBER", Price: dec(price), Currency: "RUB", On: on},
		}}
		worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())
		if err := worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}}); err != nil {
			t.Fatalf("Work: %v", err)
		}
	}

	friday, monday := date("2026-07-24"), date("2026-07-27")

	refresh("276.52", friday)
	refresh("276.52", friday)
	if n := rows(); n != 1 {
		t.Fatalf("%d rows after two refreshes of the same session, want 1", n)
	}

	refresh("280.85", monday)
	if n := rows(); n != 2 {
		t.Fatalf("%d rows after a refresh naming a later session, want 2", n)
	}
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q := latest[sber.ID]; !q.On.Equal(monday) || !q.Price.Equal(dec("280.85")) {
		t.Errorf("LatestQuotes = %s on %s, want 280.85 on %s", q.Price, q.On.Format(time.DateOnly), monday.Format(time.DateOnly))
	}
	old, err := store.QuoteOn(ctx, sber.ID, friday)
	if err != nil {
		t.Fatalf("QuoteOn(friday): %v", err)
	}
	if !old.On.Equal(friday) || !old.Price.Equal(dec("276.52")) {
		t.Errorf("QuoteOn(friday) = %s on %s, want 276.52 on %s", old.Price, old.On.Format(time.DateOnly), friday.Format(time.DateOnly))
	}
}

// When the provider dates a carried price to the day it was made, its later
// rows go; another source's later row stays.
func TestQuotesWorker_APriceDatedEarlierTakesBackTheLaterRowsOfItsSource(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store, instStore := marketdata.NewStore(pool), instrument.NewStore(pool)

	bond, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "СберИОС449", Ticker: "RU000A103AP6", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create bond: %v", err)
	}
	provider := fakeQuoteProvider{}
	traded, carried1, carried2, broker := date("2026-08-24"), date("2026-09-29"), date("2026-09-30"), date("2026-09-15")
	if err := store.UpsertQuotes(ctx, []marketdata.Quote{
		{InstrumentID: bond.ID, On: carried1, Price: dec("77.5"), Currency: "RUB", Source: provider.Name()},
		{InstrumentID: bond.ID, On: carried2, Price: dec("77.5"), Currency: "RUB", Source: provider.Name()},
		{InstrumentID: bond.ID, On: broker, Price: dec("76.02"), Currency: "RUB", Source: "tinvest"},
	}); err != nil {
		t.Fatalf("UpsertQuotes: %v", err)
	}

	provider.quotes = []marketdata.TickerQuote{{Ticker: "RU000A103AP6", Price: dec("77.5"), Currency: "RUB", On: traded}}
	worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())
	if err := worker.Work(ctx, &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT on_date, source FROM quotes WHERE instrument_id = $1 ORDER BY on_date`, bond.ID)
	if err != nil {
		t.Fatalf("read quotes: %v", err)
	}
	var got []string
	for rows.Next() {
		var on time.Time
		var source string
		if err := rows.Scan(&on, &source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, on.Format(time.DateOnly)+" "+source)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read quotes: %v", err)
	}
	want := []string{"2026-08-24 " + provider.Name(), "2026-09-15 tinvest"}
	if !slices.Equal(got, want) {
		t.Errorf("quotes after the refresh = %v, want %v", got, want)
	}
}

// A quote with no date or dated after today is refused: the provider has no
// today to check against, and a future row would outrank every refresh.
func TestQuotesWorker_RefusesAQuoteDatedZeroOrAfterToday(t *testing.T) {
	tests := []struct {
		name string
		on   time.Time
	}{
		{"zero date", time.Time{}},
		{"tomorrow", futureDay(1)},
		{"a year from now", futureDay(365)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, instStore, ctx := newJobsFixture(t)

			sber, err := instStore.Create(ctx, instrument.Instrument{
				Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
			})
			if err != nil {
				t.Fatalf("create sber: %v", err)
			}

			provider := fakeQuoteProvider{quotes: []marketdata.TickerQuote{
				{Ticker: "SBER", Price: dec("305.5"), Currency: "RUB", On: tt.on},
			}}
			logs := &logtest.Capture{}
			log := logs.Logger()
			worker := marketdata.NewQuotesWorker(store, instStore, provider, log)

			if err := worker.Work(ctx, quotesJob()); err != nil {
				t.Fatalf("Work: %v — one untrustworthy date must not fail the whole refresh", err)
			}

			latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
			if err != nil {
				t.Fatalf("LatestQuotes: %v", err)
			}
			if q, ok := latest[sber.ID]; ok {
				t.Errorf("quote was stored: %+v, want it refused — a zero or future date cannot be true, "+
					"and LatestQuotes would keep returning it forever", q)
			}

			line := logtest.Only(t, logs, futureOrZeroQuoteDateMsg)
			if line.Level != slog.LevelWarn {
				t.Errorf("the bad date was logged at %s, want WARN: this is not a routine absence like a "+
					"missing price, it is data that cannot be true", line.Level)
			}
			if got := logtest.Attr(line, "ticker"); got != "SBER" {
				t.Errorf("ticker attribute = %q, want SBER", got)
			}

			// The "no price" line must not also fire for it.
			if lines := logs.Lines("marketdata: no price for ticker, skipping"); len(lines) != 0 {
				t.Errorf("also logged the no-price line, want only the refusal above: the provider DID "+
					"answer for this ticker, just not with a storable date:\n%s", logtest.Describe(lines))
			}
		})
	}
}

// Today itself is accepted: the guard is >, not >=.
func TestQuotesWorker_AcceptsAQuoteDatedExactlyToday(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}

	today := futureDay(0)
	provider := fakeQuoteProvider{quotes: []marketdata.TickerQuote{
		{Ticker: "SBER", Price: dec("305.5"), Currency: "RUB", On: today},
	}}
	worker := marketdata.NewQuotesWorker(store, instStore, provider, slog.Default())

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[sber.ID]; !ok || !q.Price.Equal(dec("305.5")) {
		t.Errorf("sber quote = %+v, ok = %v, want 305.5 on today's own date to be accepted", q, ok)
	}
}

// futureDay returns midnight UTC n days after the real today.
func futureDay(n int) time.Time {
	now := time.Now().UTC().AddDate(0, 0, n)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Production messages are copied here, not imported, so rewording them
// breaks these tests.
const futureOrZeroQuoteDateMsg = "marketdata: provider reported a quote with no date or dated after today, refusing to store it (this instrument keeps whatever earlier quote it already has)"

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

// --- structured log capture -------------------------------------------------

const droppedRateMsg = "marketdata: source published a rate that is not positive, dropping it (this pair keeps whatever earlier rate it already has)"

// The backfill's three verdicts on a currency's series.
const (
	emptySeriesMsg = "marketdata: source published no rates for currency over the whole range (its amounts stay unconverted)"
	allRefusedMsg  = "marketdata: every rate the source published for this currency was refused as not positive (its amounts keep whatever earlier rates they already have)"
	downloadedMsg  = "marketdata: downloaded fx history"
)

// --- ticker collisions ------------------------------------------------------

// fakeInstrumentLister hands the worker a list directly: two tradable rows
// under one ticker can no longer be stored (migration 0011), but the worker
// consumes a list and must still handle it.
type fakeInstrumentLister struct {
	insts []instrument.Instrument
	err   error
}

func (l fakeInstrumentLister) ListTradable(context.Context) ([]instrument.Instrument, error) {
	return l.insts, l.err
}

func quotesJob() *river.Job[marketdata.RefreshQuotesArgs] {
	return &river.Job[marketdata.RefreshQuotesArgs]{Args: marketdata.RefreshQuotesArgs{}}
}

// One ticker in two currencies is priced apart: AT&T is "T" in dollars,
// Т-Технологии "T" in rubles.
func TestQuotesWorker_OneTickerTwoCurrenciesArePricedApart(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	russian, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Т-Технологии", Ticker: "T",
		ISIN: "RU000A107UL4", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create the Russian paper: %v", err)
	}
	american, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "AT&T", Ticker: "T",
		ISIN: "US00206R1023", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create AT&T — the catalog must take two papers under one ticker: %v", err)
	}

	var calls [][]string
	provider := fakeQuoteProvider{
		calls: &calls,
		quotes: []marketdata.TickerQuote{
			{Ticker: "T", Price: dec("3500"), Currency: "RUB", On: date("2026-07-25")},
		},
	}
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{russian, american}}, provider, slog.Default())

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// Asked once: the provider speaks bare tickers.
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"T"}) {
		t.Errorf("provider was asked for %v, want one T", calls)
	}

	latest, err := store.LatestQuotes(ctx, []uuid.UUID{russian.ID, american.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[russian.ID]; !ok || !q.Price.Equal(dec("3500")) {
		t.Errorf("the Russian paper's quote = %+v, ok = %v, want 3500 ₽", q, ok)
	}
	if q, ok := latest[american.ID]; ok {
		t.Errorf("AT&T was priced %+v from a RUBLE quote — that is another company's number, and off by the whole exchange rate", q)
	}
}

// Two rows agreeing on ticker and currency are priced neither way (#26), with
// a Warn, and the run continues: retrying cannot resolve a duplicate.
func TestQuotesWorker_TwoInstrumentsUnderOneTickerAndCurrencyArePricedNeitherWay(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	// Real catalog rows (quotes reference them); the collision is staged in the
	// list, the only place it can exist.
	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}
	twin, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк, второй раз", Ticker: "GAZP", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create twin: %v", err)
	}
	twin.Ticker = "SBER"

	var calls [][]string
	provider := fakeQuoteProvider{
		calls: &calls,
		quotes: []marketdata.TickerQuote{
			{Ticker: "SBER", Price: dec("305.5"), Currency: "RUB", On: date("2026-07-25")},
		},
	}
	logs := &logtest.Capture{}
	log := logs.Logger()
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{sber, twin}}, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v — one duplicated ticker must not cost the whole refresh", err)
	}

	line := logtest.Only(t, logs, "marketdata: two instruments share a ticker AND a currency, so neither can be priced by that alone")
	if line.Level != slog.LevelWarn {
		t.Errorf("the collision was logged at %s, want WARN: Debug is off on a production instance, "+
			"which is exactly where this went unnoticed", line.Level)
	}
	// The line names the ticker and both instruments.
	if got := logtest.Attr(line, "ticker"); got != "SBER" {
		t.Errorf("ticker attribute = %q, want SBER", got)
	}
	if got := logtest.Attr(line, "currency"); got != "RUB" {
		t.Errorf("currency attribute = %q, want RUB — it is half of what a price is matched by", got)
	}
	if got := logtest.Attr(line, "instrument_id"); got != sber.ID.String() {
		t.Errorf("instrument_id = %q, want %s", got, sber.ID)
	}
	if got := logtest.Attr(line, "other_instrument_id"); got != twin.ID.String() {
		t.Errorf("other_instrument_id = %q, want %s", got, twin.ID)
	}

	// The duplicate is dropped from the request too, not asked for twice.
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"SBER"}) {
		t.Errorf("provider was asked for %v, want one SBER: the second row adds nothing to ask for", calls)
	}

	// Neither is priced: being seen first buys nothing.
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID, twin.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[sber.ID]; ok {
		t.Errorf("the first row got the quote %+v — being listed first is not evidence that the price is its", q)
	}
	if q, ok := latest[twin.ID]; ok {
		t.Errorf("the second row got a quote %+v; neither of an ambiguous pair may be priced", q)
	}
}

// A ticker the catalog does not hold is logged at Debug, like "no price":
// nothing of ours goes unpriced over it.
func TestQuotesWorker_TickerTheCatalogDoesNotHoldIsLoggedAtDebug(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	sber, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create sber: %v", err)
	}

	provider := fakeQuoteProvider{quotes: []marketdata.TickerQuote{
		{Ticker: "SBER", Price: dec("305.5"), Currency: "RUB", On: date("2026-07-25")},
		{Ticker: "SBER-RM", Price: dec("306.0"), Currency: "RUB", On: date("2026-07-25")},
	}}
	logs := &logtest.Capture{}
	log := logs.Logger()
	worker := marketdata.NewQuotesWorker(store, instStore, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v — an unknown ticker must not fail the batch", err)
	}

	line := logtest.Only(t, logs, "marketdata: provider reported a ticker the catalog does not hold, ignoring it")
	if line.Level != slog.LevelDebug {
		t.Errorf("the unknown ticker was logged at %s, want DEBUG: it costs no instrument its price, "+
			"and a louder level would fill every production log with something nobody can act on", line.Level)
	}
	if got := logtest.Attr(line, "ticker"); got != "SBER-RM" {
		t.Errorf("ticker attribute = %q, want SBER-RM", got)
	}
	if got := logtest.Attr(line, "provider"); got != "fake-quotes" {
		t.Errorf("provider attribute = %q, want fake-quotes", got)
	}

	// The known ticker in the same batch is still stored.
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[sber.ID]; !ok || !q.Price.Equal(dec("305.5")) {
		t.Errorf("sber quote = %+v, ok = %v, want 305.5", q, ok)
	}
}

// Tickerless rows are reported on a line of their own, not as a collision on
// the empty ticker, and nothing is asked for them.
func TestQuotesWorker_InstrumentsWithNoTickerAreNotReportedAsACollision(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	var tickerless []instrument.Instrument
	for _, name := range []string{"Наличные", "Золотой слиток"} {
		inst, err := instStore.Create(ctx, instrument.Instrument{
			Type: instrument.TypeCustom, Name: name, Currency: "RUB",
		})
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		tickerless = append(tickerless, inst)
	}

	var calls [][]string
	provider := fakeQuoteProvider{calls: &calls}
	logs := &logtest.Capture{}
	log := logs.Logger()
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: tickerless}, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	for _, r := range logs.Records() {
		if r.Message == "marketdata: two instruments share a ticker, only one of them can be priced" {
			t.Errorf("two tickerless instruments were reported as sharing a ticker (ticker=%q): "+
				"they share the absence of one, and neither is priced either way", logtest.Attr(r, "ticker"))
		}
	}
	// Each is still logged, by id.
	var seen []string
	for _, r := range logs.Records() {
		if r.Message == "marketdata: instrument has no ticker, there is nothing to ask a price for" {
			if r.Level != slog.LevelDebug {
				t.Errorf("a tickerless instrument was logged at %s, want DEBUG: it loses no price it could have had", r.Level)
			}
			seen = append(seen, logtest.Attr(r, "instrument_id"))
		}
	}
	for _, inst := range tickerless {
		if !slices.Contains(seen, inst.ID.String()) {
			t.Errorf("%q was dropped from the mapping without a line naming it; logged ids: %v", inst.Name, seen)
		}
	}

	// The empty string never reaches the provider.
	if len(calls) != 1 || len(calls[0]) != 0 {
		t.Errorf("provider was asked for %q, want one call with nothing in it", calls)
	}
}

// --- historical fx backfill -------------------------------------------------

// cbrIDs uses the bank's real identifiers, so tests also pin that the
// identifier, not the ISO code, reaches the history endpoint.
var cbrIDs = map[string]string{
	"USD": "R01235",
	"EUR": "R01239",
	"GBP": "R01035",
	"TRY": "R01700J",
}

type rangeRequest struct {
	code       string
	currencyID string
	from, to   time.Time
}

// recordingHistoryProvider records every request, so tests can tell one
// request for the whole range from a range walked in pieces.
type recordingHistoryProvider struct {
	ids      map[string]string // ISO code -> internal id; absent = not quoted by this source
	idCalls  int
	idsErr   error  // failure CurrencyIDs returns; nil means it always succeeds
	emptyFor string // ISO code whose series comes back with no records at all
	zeroFor  string // ISO code whose series carries a zero rate at the older end
	allZero  string // ISO code whose series is non-positive from end to end
	requests []rangeRequest
	failFor  string // ISO code whose RatesRange fails
	err      error  // the failure it returns; nil means never fail
}

func (p *recordingHistoryProvider) Name() string { return "fake-fx" }

// RatesOn must never be called: history is never fetched a day at a time.
func (p *recordingHistoryProvider) RatesOn(context.Context, time.Time) ([]marketdata.FxRate, error) {
	return nil, errors.New("backfill must not fetch history one day at a time")
}

func (p *recordingHistoryProvider) CurrencyIDs(context.Context) (map[string]string, error) {
	p.idCalls++
	if p.idsErr != nil {
		return nil, p.idsErr
	}
	return p.ids, nil
}

// RatesRange answers a rate at each end of the range. For zeroFor the older
// one is zero, so a dropped record can be told from a dropped series.
func (p *recordingHistoryProvider) RatesRange(
	_ context.Context, code, currencyID string, from, to time.Time,
) ([]marketdata.FxRate, error) {
	p.requests = append(p.requests, rangeRequest{code: code, currencyID: currencyID, from: from, to: to})
	if p.err != nil && code == p.failFor {
		return nil, p.err
	}
	if code == p.emptyFor {
		return nil, nil
	}
	older, newer := dec("90.5"), dec("91.5")
	if code == p.zeroFor {
		older = dec("0")
	}
	if code == p.allZero {
		older, newer = dec("0"), dec("-1")
	}
	return []marketdata.FxRate{
		{Base: code, Quote: "RUB", On: from, Rate: older, Source: p.Name()},
		{Base: code, Quote: "RUB", On: to, Rate: newer, Source: p.Name()},
	}, nil
}

func (p *recordingHistoryProvider) codesAsked() []string {
	out := make([]string, len(p.requests))
	for i, r := range p.requests {
		out[i] = r.code
	}
	return out
}

// fakeOpStore stands in for the operation store; its two errors are
// independent, so either read can fail on its own.
type fakeOpStore struct {
	earliest      time.Time
	err           error
	currencies    []string
	currenciesErr error
}

func (s fakeOpStore) EarliestRecordedDay(context.Context) (time.Time, error) {
	if s.err != nil {
		return time.Time{}, s.err
	}
	return s.earliest, nil
}

func (s fakeOpStore) DistinctCurrencies(context.Context) ([]string, error) {
	if s.currenciesErr != nil {
		return nil, s.currenciesErr
	}
	return s.currencies, nil
}

// fakeAccountStore stands in for (*account.Store).DistinctCurrencies.
type fakeAccountStore struct {
	currencies []string
	err        error
}

func (s fakeAccountStore) DistinctCurrencies(context.Context) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.currencies, nil
}

// fakeSpaceStore stands in for (*family.Store).DistinctBaseCurrencies.
type fakeSpaceStore struct {
	base []string
	err  error
}

func (s fakeSpaceStore) DistinctBaseCurrencies(context.Context) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.base, nil
}

// pinnedToday is the backfill tests' clock, far from the wall clock so a worker
// reading time.Now() asks for a visibly different range.
var pinnedToday = date("2025-11-20")

func newBackfillFixture(t *testing.T) (*marketdata.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	return marketdata.NewStore(pool), pool, ctx
}

// newBackfillWorker builds the worker with its clock pinned to pinnedToday.
func newBackfillWorker(
	store *marketdata.Store,
	ops fakeOpStore,
	accounts fakeAccountStore,
	spaces fakeSpaceStore,
	provider marketdata.FxHistoryProvider,
	log *slog.Logger,
) river.Worker[marketdata.BackfillFxArgs] {
	return marketdata.NewBackfillFxWorkerWithClock(
		store, ops, accounts, spaces, provider, log, func() time.Time { return pinnedToday })
}

func backfillJob() *river.Job[marketdata.BackfillFxArgs] {
	return &river.Job[marketdata.BackfillFxArgs]{Args: marketdata.BackfillFxArgs{}}
}

// riverInsertClient returns a context carrying an insert-only River client, as
// a running job's context would, so an attempt to enqueue would not be hidden.
func riverInsertClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool) context.Context {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	return rivertest.WorkContext(ctx, client)
}

// queuedBackfillJobs counts the river_job rows for the backfill job kind.
func queuedBackfillJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1`,
		marketdata.BackfillFxArgs{}.Kind()).Scan(&n); err != nil {
		t.Fatalf("count queued jobs: %v", err)
	}
	return n
}

// countFxRates counts every stored rate, of any currency and any date.
func countFxRates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fx_rates`).Scan(&n); err != nil {
		t.Fatalf("count fx_rates: %v", err)
	}
	return n
}

// utcToday is today at midnight UTC.
func utcToday() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func showDates(ds []time.Time) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Format(time.DateOnly)
	}
	return strings.Join(out, ",")
}

// showRequests renders requests as "CODE(id):from..to".
func showRequests(rs []rangeRequest) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.code + "(" + r.currencyID + "):" +
			r.from.Format(time.DateOnly) + ".." + r.to.Format(time.DateOnly)
	}
	return strings.Join(out, " ")
}

func TestBackfillFx_NoOperationsSkipsProviderEntirely(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	// With no operations there is no range, so the source is not touched.
	worker := newBackfillWorker(store,
		fakeOpStore{err: pgx.ErrNoRows, currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"RUB", "USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none at all with no operations",
			provider.idCalls, showRequests(provider.requests))
	}
}

// A read error other than "no rows" fails the job instead of passing for
// "nothing to fetch".
func TestBackfillFx_EarliestOperationLookupErrorFailsTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("connection reset")
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{err: wantErr, currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when the earliest-operation read fails",
			provider.idCalls, showRequests(provider.requests))
	}
}

func TestBackfillFx_OnlyTheQuoteCurrencyInUseSkipsProviderEntirely(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	// An all-RUB instance needs no rates.
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"RUB"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when only RUB is in use",
			provider.idCalls, showRequests(provider.requests))
	}
}

func TestBackfillFx_RequestsEveryCurrencyInUseExceptTheQuoteCurrency(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	// This provider quotes RUB (the real one does not), so the "never fetched for
	// RUB" rule is actually exercised.
	ids := map[string]string{"RUB": "R00000"}
	maps.Copy(ids, cbrIDs)
	provider := &recordingHistoryProvider{ids: ids}
	// USD is in use twice and still downloaded once.
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR", "RUB", "USD"}},
		fakeAccountStore{currencies: []string{"RUB", "USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	want := []string{"EUR", "USD"}
	if got := provider.codesAsked(); !slices.Equal(got, want) {
		t.Fatalf("asked for %v, want exactly %v: an account currency, an operation currency, "+
			"each asked for exactly once, and no RUB", got, want)
	}
	if provider.idCalls != 1 {
		t.Fatalf("currency-id map fetched %d times, want exactly 1 per run", provider.idCalls)
	}
	for _, r := range provider.requests {
		if r.currencyID != cbrIDs[r.code] {
			t.Fatalf("%s was requested under id %q, want the source's own id %q",
				r.code, r.currencyID, cbrIDs[r.code])
		}
	}
}

// A failed read of any currency source fails the job before the provider is
// asked anything, so River retries instead of working from a partial set.
func TestBackfillFx_ACurrencyReadErrorFailsTheJob(t *testing.T) {
	wantErr := errors.New("db unreachable")
	for _, c := range []struct {
		name     string
		ops      fakeOpStore
		accounts fakeAccountStore
		spaces   fakeSpaceStore
	}{
		{
			"accounts",
			fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR"}},
			fakeAccountStore{err: wantErr},
			fakeSpaceStore{base: []string{"RUB"}},
		},
		{
			"operations",
			fakeOpStore{earliest: date("2024-01-10"), currenciesErr: wantErr},
			fakeAccountStore{currencies: []string{"USD"}},
			fakeSpaceStore{base: []string{"RUB"}},
		},
		{
			"space base currencies",
			fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR"}},
			fakeAccountStore{currencies: []string{"USD"}},
			fakeSpaceStore{err: wantErr},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, _, ctx := newBackfillFixture(t)
			provider := &recordingHistoryProvider{ids: cbrIDs}
			worker := newBackfillWorker(store, c.ops, c.accounts, c.spaces, provider, slog.Default())

			if err := worker.Work(ctx, backfillJob()); !errors.Is(err, wantErr) {
				t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
			}
			if provider.idCalls != 0 || len(provider.requests) != 0 {
				t.Fatalf("provider: %d currency-id calls, requests [%s]; want none",
					provider.idCalls, showRequests(provider.requests))
			}
		})
	}
}

func TestBackfillFx_IncludesTheSpaceBaseCurrency(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	// GBP is only a space's base currency and still needed.
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"RUB"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"GBP"}},
		provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := provider.codesAsked(); !slices.Equal(got, []string{"GBP"}) {
		t.Fatalf("asked for %v, want [GBP]: the space's base currency is needed even when nothing is held in it", got)
	}
}

// One request per currency, covering the oldest operation to today: no
// chunking, no calendar walk, no repeats.
func TestBackfillFx_AsksForEachSeriesOnceOverTheWholeRange(t *testing.T) {
	store, pool, ctx := newBackfillFixture(t)

	// Twelve years: hundreds of chunks under the old day-at-a-time scheme.
	earliest := date("2013-05-16")
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: earliest, currencies: []string{"EUR"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("provider got %d requests [%s], want exactly one per currency (2)",
			len(provider.requests), showRequests(provider.requests))
	}
	// A month before the oldest operation (see backfillLeadDays), so a weekend
	// purchase has an earlier rate to resolve to.
	wantFrom := earliest.AddDate(0, 0, -31)
	for _, r := range provider.requests {
		if !r.from.Equal(wantFrom) || !r.to.Equal(pinnedToday) {
			t.Fatalf("%s was asked for %s..%s, want the whole range %s..%s in one request",
				r.code, r.from.Format(time.DateOnly), r.to.Format(time.DateOnly),
				wantFrom.Format(time.DateOnly), pinnedToday.Format(time.DateOnly))
		}
	}

	// Both ends of both series are stored; the older end resolves the oldest
	// operation.
	for _, code := range []string{"EUR", "USD"} {
		got, err := store.FxRateOn(ctx, code, "RUB", earliest)
		if err != nil {
			t.Fatalf("FxRateOn(%s, %s): %v — the oldest operation has no rate to fall back to, which is what the lead exists to prevent",
				code, earliest.Format(time.DateOnly), err)
		}
		if got.On.After(earliest) {
			t.Fatalf("%s rate nearest %s is dated %s, which is AFTER it — a lookup takes the nearest EARLIER row and this one cannot be used",
				code, earliest.Format(time.DateOnly), got.On.Format(time.DateOnly))
		}
		today, err := store.FxRateOn(ctx, code, "RUB", pinnedToday)
		if err != nil {
			t.Fatalf("FxRateOn(%s, today): %v", code, err)
		}
		if !today.On.Equal(pinnedToday) {
			t.Fatalf("%s rate nearest today is dated %s, want the series to reach today",
				code, today.On.Format(time.DateOnly))
		}
	}
	if got := countFxRates(t, ctx, pool); got != 4 {
		t.Fatalf("stored %d rates, want 4 (two ends of two series)", got)
	}
}

// wantWarned asserts a line at WARN carrying substr; a substring match alone
// cannot tell Warn from Debug.
func wantWarned(t *testing.T, logs *bytes.Buffer, substr string) {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, substr) {
			return
		}
	}
	t.Fatalf("no WARN line mentioning %q:\n%s", substr, logs.String())
}

// A CurrencyIDs error fails the job: swallowed, the run would close green
// with no history downloaded.
func TestBackfillFx_CurrencyIDsErrorFailsTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("cbr unreachable")
	provider := &recordingHistoryProvider{ids: cbrIDs, idsErr: wantErr}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"EUR"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work: %v, want the currency-id failure to fail the job so River retries it", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("requests [%s], want none: without the id map there is nothing to ask under",
			showRequests(provider.requests))
	}
}

// A currency with an identifier but nothing published over the range is
// warned about, not logged as an ordinary download.
func TestBackfillFx_EmptySeriesIsWarnedNotReportedAsADownload(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs, emptyFor: "GBP"}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"GBP"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want an empty series to be reported, not to fail the run", err)
	}

	// The other currency still downloads.
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}
	wantWarned(t, &logs, "GBP")
}

// A non-positive record in a history series is dropped and the rest stored
// (#28); another currency in the same run is unaffected.
func TestBackfillFx_NonPositiveRateIsDroppedAndTheRestOfTheSeriesStored(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	logs := &logtest.Capture{}
	log := logs.Logger()
	// GBP's older record is zero; USD's series is sound.
	provider := &recordingHistoryProvider{ids: cbrIDs, zeroFor: "GBP"}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"GBP"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want one unusable record not to cost the whole series", err)
	}

	// The sound end of the poisoned series is stored...
	got, err := store.FxRateOn(ctx, "GBP", "RUB", pinnedToday)
	if err != nil {
		t.Fatalf("FxRateOn(GBP): %v, want the sound part of the series stored", err)
	}
	if !got.Rate.Equal(dec("91.5")) {
		t.Fatalf("GBP rate = %s, want 91.5", got.Rate)
	}
	// ...and the refused record is not stored.
	if got, err := store.FxRateOn(ctx, "GBP", "RUB", date("2024-01-10")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("FxRateOn(GBP, 2024-01-10) = %+v, err = %v, want pgx.ErrNoRows: the refused record must not be stored", got, err)
	}
	// The other currency is untouched by any of it.
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}

	dropped := logs.Lines(droppedRateMsg)
	if len(dropped) != 1 {
		t.Fatalf("%d dropped-rate lines, want exactly 1:\n%s", len(dropped), logtest.Describe(logs.Records()))
	}
	if dropped[0].Level != slog.LevelWarn || logtest.Attr(dropped[0], "base") != "GBP" {
		t.Fatalf("dropped-rate line = %s base=%q, want WARN for GBP:\n%s",
			dropped[0].Level, logtest.Attr(dropped[0], "base"), logtest.Describe(logs.Records()))
	}
}

// A series whose every record is refused is reported as refused, not as "the
// source published nothing", which would blame the source.
func TestBackfillFx_WholeSeriesRefusedIsNotReportedAsAnEmptySeries(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	logs := &logtest.Capture{}
	log := logs.Logger()
	provider := &recordingHistoryProvider{ids: cbrIDs, allZero: "GBP"}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"GBP"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want an entirely unusable series to be reported, not to fail the run", err)
	}

	if n := len(logs.Lines(emptySeriesMsg)); n != 0 {
		t.Fatalf("%d empty-series lines, want 0: the source published two records, they were refused here:\n%s",
			n, logtest.Describe(logs.Records()))
	}
	line := logtest.Only(t, logs, allRefusedMsg)
	if line.Level != slog.LevelWarn {
		t.Fatalf("all-refused line at %s, want WARN — it leaves the currency unconverted exactly as an empty series does:\n%s",
			line.Level, logtest.Describe(logs.Records()))
	}
	if logtest.Attr(line, "currency") != "GBP" {
		t.Fatalf("all-refused line names currency=%q, want GBP:\n%s", logtest.Attr(line, "currency"), logtest.Describe(logs.Records()))
	}
	for _, l := range logs.Lines(downloadedMsg) {
		if logtest.Attr(l, "currency") == "GBP" {
			t.Fatalf("GBP reported as a download:\n%s", logtest.Describe(logs.Records()))
		}
	}
	// The sound currency in the same run still lands.
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}
}

func TestBackfillFx_ClampsAbsurdlyEarlyOperationToTheFloor(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("1970-01-01"), currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	floor := date("2000-01-01")
	if len(provider.requests) != 1 || !provider.requests[0].from.Equal(floor) {
		t.Fatalf("requests [%s], want a single USD series starting at the floor %s",
			showRequests(provider.requests), floor.Format(time.DateOnly))
	}
	// The clamped-away days are reported, counted from where the request would
	// have started.
	wantDropped := int(floor.Sub(date("1970-01-01").AddDate(0, 0, -31)).Hours() / 24)
	wantWarned(t, &logs, "days_dropped="+strconv.Itoa(wantDropped))
}

func TestBackfillFx_UnquotedCurrencyIsSkippedWithALogAndTheRestStillDownloaded(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// BYN has no identifier, so its series cannot be requested.
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"BYN"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want the run to carry on past a currency the source doesn't quote", err)
	}

	if got := provider.codesAsked(); !slices.Equal(got, []string{"USD"}) {
		t.Fatalf("asked for %v, want [USD] only: BYN has no identifier to ask under", got)
	}
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}
	wantWarned(t, &logs, "BYN")
}

func TestBackfillFx_ProviderErrorFailsTheJobAndKeepsWhatWasStored(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("cbr unreachable")
	// EUR is requested first and is stored when USD fails.
	provider := &recordingHistoryProvider{ids: cbrIDs, failFor: "USD", err: wantErr}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
	}
	if got := provider.codesAsked(); !slices.Equal(got, []string{"EUR", "USD"}) {
		t.Fatalf("asked for %v, want [EUR USD]", got)
	}
	if _, err := store.FxRateOn(ctx, "EUR", "RUB", pinnedToday); err != nil {
		t.Fatalf("EUR rates fetched before the failure did not survive it: %v", err)
	}
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("USD lookup err = %v, want pgx.ErrNoRows: its request failed", err)
	}
}

// A store error after a successful fetch fails the job. The closed pool
// forces a real Postgres error.
func TestBackfillFx_StoreSaveErrorFailsTheJob(t *testing.T) {
	store, pool, ctx := newBackfillFixture(t)
	pool.Close()

	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"USD"}},
		fakeAccountStore{},
		fakeSpaceStore{},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if err == nil {
		t.Fatal("Work returned nil, want an error: the pool is closed so the save must fail")
	}
	if got := provider.codesAsked(); !slices.Equal(got, []string{"USD"}) {
		t.Fatalf("asked for %v, want [USD]: the provider call itself must still have gone through", got)
	}
}

// A re-run asks for the whole range again, overwrites the same rows, and
// queues nothing.
func TestBackfillFx_RepeatRunRefetchesTheRangeWithoutDuplicatingRows(t *testing.T) {
	store, pool, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"RUB"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())
	runCtx := riverInsertClient(t, ctx, pool)

	if err := worker.Work(runCtx, backfillJob()); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	firstRows := countFxRates(t, ctx, pool)
	if firstRows != 2 {
		t.Fatalf("stored %d rates after the first run, want 2", firstRows)
	}

	if err := worker.Work(runCtx, backfillJob()); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider got %d requests over two runs [%s], want 2: a re-run downloads the range again",
			len(provider.requests), showRequests(provider.requests))
	}
	if got := countFxRates(t, ctx, pool); got != firstRows {
		t.Fatalf("stored %d rates after the second run, want %d: a re-run overwrites, it does not accumulate",
			got, firstRows)
	}
	if got := queuedBackfillJobs(t, ctx, pool); got != 0 {
		t.Fatalf("queued backfill jobs = %d, want 0: the job must not enqueue continuations of itself", got)
	}
}

// A future-dated operation does not make the range run backwards, which the
// source would reject on every run.
func TestBackfillFx_FutureDatedOperationDoesNotInvertTheRange(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: pinnedToday.AddDate(0, 0, 30), currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests [%s], want exactly one", showRequests(provider.requests))
	}
	if r := provider.requests[0]; r.from.After(r.to) {
		t.Fatalf("asked for %s..%s: the range runs backwards",
			r.from.Format(time.DateOnly), r.to.Format(time.DateOnly))
	}
	wantWarned(t, &logs, "future")
}

// An operation a few days ahead is still reported, though the month's lead
// keeps the range itself valid: the check uses the operation's own day.
func TestBackfillFx_ANearFutureOperationIsStillReported(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: pinnedToday.AddDate(0, 0, 5), currencies: []string{"USD"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if r := provider.requests[0]; r.from.After(r.to) {
		t.Fatalf("asked for %s..%s: the range runs backwards",
			r.from.Format(time.DateOnly), r.to.Format(time.DateOnly))
	}
	wantWarned(t, &logs, "future")
}

// A future-dated earliest operation is reported even when there is no
// currency to fetch.
func TestBackfillFx_FutureDatedOperationWarnsEvenWhenNoCurrencyToFetch(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: pinnedToday.AddDate(0, 0, 30), currencies: []string{"RUB"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when only RUB is in use",
			provider.idCalls, showRequests(provider.requests))
	}
	wantWarned(t, &logs, "future")
}

// The production constructor wires the wall clock; every other test pins it.
func TestBackfillFx_ProductionConstructorUsesTheWallClock(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := marketdata.NewBackfillFxWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"RUB"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	before := utcToday()
	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	after := utcToday()

	if len(provider.requests) != 1 {
		t.Fatalf("requests [%s], want exactly one", showRequests(provider.requests))
	}
	// before and after differ only if the run straddled UTC midnight.
	if to := provider.requests[0].to; !to.Equal(before) && !to.Equal(after) {
		t.Fatalf("range ends at %s, want today (%s)", to.Format(time.DateOnly), showDates([]time.Time{before, after}))
	}
}

func TestBackfillFx_JobTimeoutOutlastsRiversDefault(t *testing.T) {
	store, _, _ := newBackfillFixture(t)

	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10")},
		fakeAccountStore{}, fakeSpaceStore{},
		&recordingHistoryProvider{ids: cbrIDs}, slog.Default())

	// River's default timeout is a minute; a run fetches multi-year series for
	// every currency in use.
	if got := worker.Timeout(backfillJob()); got <= time.Minute {
		t.Fatalf("Timeout = %s, want more than River's one-minute default", got)
	}
}

// fakeGoldProvider answers the gold history and records the range asked.
type fakeGoldProvider struct {
	rates []marketdata.FxRate
	err   error
	from  time.Time
	to    time.Time
	calls int
}

func (p *fakeGoldProvider) GoldRates(_ context.Context, from, to time.Time) ([]marketdata.FxRate, error) {
	p.calls++
	p.from, p.to = from, to
	return p.rates, p.err
}

// Gold rates come from the exchange into the same table as currencies, so a
// gold balance is valued by the same lookup.
func TestBackfillGold_StoresTheExchangesGoldHistory(t *testing.T) {
	store, pool, ctx := newBackfillFixture(t)

	provider := &fakeGoldProvider{rates: []marketdata.FxRate{
		{Base: "XAU", Quote: "RUB", On: date("2024-10-21"), Rate: dec("8422.2"), Source: "moex"},
		{Base: "XAU", Quote: "RUB", On: date("2024-10-22"), Rate: dec("8479"), Source: "moex"},
	}}
	worker := marketdata.NewBackfillGoldWorker(store,
		fakeOpStore{earliest: date("2024-10-25"), currencies: []string{"XAU"}}, provider, slog.Default())

	if err := worker.Work(ctx, &river.Job[marketdata.BackfillGoldArgs]{Args: marketdata.BackfillGoldArgs{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// A month before the earliest operation, as for currencies.
	if want := date("2024-10-25").AddDate(0, 0, -31); !provider.from.Equal(want) {
		t.Errorf("asked from %s, want %s", provider.from.Format(time.DateOnly), want.Format(time.DateOnly))
	}

	got, err := store.FxRateOn(ctx, "XAU", "RUB", date("2024-10-23"))
	if err != nil {
		t.Fatalf("FxRateOn: %v — a day after the last published one must fall back to it", err)
	}
	if got.Rate.String() != "8479" {
		t.Errorf("rate = %s, want 8479", got.Rate)
	}
	if n := countFxRates(t, ctx, pool); n != 2 {
		t.Errorf("stored %d rates, want 2", n)
	}
}

// An empty gold answer is warned about: gold amounts stay unconverted.
func TestBackfillGold_AnEmptyAnswerIsWarnedAboutRatherThanStoredSilently(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker := marketdata.NewBackfillGoldWorker(store,
		fakeOpStore{earliest: date("2024-10-25"), currencies: []string{"XAU"}},
		&fakeGoldProvider{}, log)

	if err := worker.Work(ctx, &river.Job[marketdata.BackfillGoldArgs]{Args: marketdata.BackfillGoldArgs{}}); err != nil {
		t.Fatalf("Work: %v — an empty answer is not a failure to retry for ever", err)
	}
	wantWarned(t, &logs, "no gold prices")
}

// The CBR is not asked about gold, so no false "stays unconverted" warning
// fires for it.
func TestBackfillFx_GoldIsNotAskedOfTheCentralBank(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-10-25"), currencies: []string{"XAU", "USD"}},
		fakeAccountStore{currencies: []string{"RUB"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	for _, r := range provider.requests {
		if r.code == "XAU" {
			t.Errorf("the central bank was asked about gold: %+v", r)
		}
	}
	if strings.Contains(logs.String(), "XAU") {
		t.Errorf("gold was reported as a currency this source does not quote — true of the source, and misleading about the money:\n%s", logs.String())
	}
}

// Prices match by ISIN, not ticker: two euro listings under one ticker on two
// exchanges would otherwise be indistinguishable (the owner's objection).
func TestQuotesWorker_PricesAreMatchedByISINNotByTicker(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	french, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Парижская", Ticker: "ABC",
		ISIN: "FR0000000001", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("create the French paper: %v", err)
	}
	german, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Франкфуртская", Ticker: "ABC",
		ISIN: "DE0000000002", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("create the German paper: %v", err)
	}

	provider := fakeQuoteProvider{
		quotes: []marketdata.TickerQuote{
			{Ticker: "ABC", ISIN: "DE0000000002", Price: dec("42"), Currency: "EUR", On: date("2026-07-25")},
		},
	}
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{french, german}}, provider, slog.Default())

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	latest, err := store.LatestQuotes(ctx, []uuid.UUID{french.ID, german.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[german.ID]; !ok || !q.Price.Equal(dec("42")) {
		t.Errorf("the German paper's quote = %+v, ok = %v, want 42 — the answer carried its ISIN", q, ok)
	}
	if q, ok := latest[french.ID]; ok {
		t.Errorf("the French paper was priced %+v from another company's number: same ticker, same currency, different security", q)
	}
}

// A row with no ISIN is still priced by its ticker.
func TestQuotesWorker_ARowWithNoISINIsStillPricedByItsTicker(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	byHand, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Заведена руками", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	provider := fakeQuoteProvider{
		quotes: []marketdata.TickerQuote{
			{Ticker: "SBER", ISIN: "RU0009029540", Price: dec("305.5"), Currency: "RUB", On: date("2026-07-25")},
		},
	}
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{byHand}}, provider, slog.Default())

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{byHand.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[byHand.ID]; !ok || !q.Price.Equal(dec("305.5")) {
		t.Errorf("quote = %+v, ok = %v, want 305.5 — a row with no ISIN has only its ticker", q, ok)
	}
}

// A row whose ISIN did not match is not priced by ticker: the exchange named a
// different security.
func TestQuotesWorker_ARowWithAnISINIsNotPricedByItsTickerAlone(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	att, err := instStore.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "AT&T", Ticker: "T",
		ISIN: "US00206R1023", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	provider := fakeQuoteProvider{
		quotes: []marketdata.TickerQuote{
			{Ticker: "T", ISIN: "RU000A107UL4", Price: dec("3500"), Currency: "RUB", On: date("2026-07-25")},
		},
	}
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{att}}, provider, slog.Default())

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{att.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[att.ID]; ok {
		t.Errorf("AT&T was priced %+v from Т-Технологии's number: the answer named a different security outright", q)
	}
}
