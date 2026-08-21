package marketdata_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// TestFxWorker_NonPositiveRateIsDroppedAndTheRestIsStored is the poison-batch
// half of issue #28. fx_rates.rate carries CHECK (rate > 0) (migration 0006)
// and the upsert is one pgx batch, which Postgres runs inside a single
// implicit transaction: before this, ONE non-positive row from the source made
// the whole call fail and not a single rate was written, so every currency
// went stale rather than one. River then retried the job, the source answered
// with the identical set, and it failed again — the same poison, forever.
//
// The assertion is therefore both halves at once: the run succeeds AND the
// sound rates are in the table. Dropping the bad rows without storing the good
// ones would be the same outage wearing a green log line.
func TestFxWorker_NonPositiveRateIsDroppedAndTheRestIsStored(t *testing.T) {
	store, _, ctx := newJobsFixture(t)
	on := date("2026-07-25")
	log, records := newRecordingLogger()

	// Zero and negative are both refused by the CHECK, and both are things a
	// source can emit: zero for a currency it has stopped quoting, a negative
	// for a parse or sign bug at either end.
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

	// The drop is a loss of data, so it is recorded rather than swallowed —
	// one line per rate, at Warn, naming which pair and which value. Debug
	// would not do: it is off on a production instance, which is the only
	// place this can happen.
	dropped := linesFor(*records, droppedRateMsg)
	if len(dropped) != 2 {
		t.Fatalf("%d dropped-rate lines, want 2 (one per dropped rate):\n%s", len(dropped), showLines(*records))
	}
	for _, line := range dropped {
		if line.level != slog.LevelWarn {
			t.Fatalf("dropped-rate line at %s, want WARN:\n%s", line.level, showLines(*records))
		}
	}
	if bases := []string{dropped[0].attrs["base"], dropped[1].attrs["base"]}; !slices.Equal(bases, []string{"XXX", "YYY"}) {
		t.Fatalf("dropped-rate lines name %v, want [XXX YYY]:\n%s", bases, showLines(*records))
	}
	if got := dropped[0].attrs["rate"]; got != "0" {
		t.Fatalf("dropped-rate line for XXX says rate=%q, want the value that was refused:\n%s", got, showLines(*records))
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

// TestQuotesWorker_StoresTheDayTheProviderNamed pins where a stored quote's
// date comes from: the provider's TickerQuote.On, which is the exchange's own
// word for which session the price belongs to. The worker used to date every
// quote time.Now() instead, so the previous session's price was written down
// as today's and nothing downstream could tell how old it was (#90).
//
// The date the provider names here is in the past and cannot be produced by
// any clock, so an implementation that reached for one fails on the value, not
// on a comparison that could go either way.
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

	// And the row really is at that date rather than merely reporting it:
	// asked for the day before, the store must say there is no quote yet.
	if _, err := store.QuoteOn(ctx, sber.ID, session.AddDate(0, 0, -1)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("QuoteOn(the day before the session) err = %v, want pgx.ErrNoRows: "+
			"a quote must not exist on a day before the session it belongs to", err)
	}
}

// TestQuotesWorker_RowsFollowTheExchangesSessionsNotTheRefreshes covers the
// side effect of dating quotes by session: repeat refreshes now rewrite one
// row instead of laying down a new one per calendar day, and a new row appears
// exactly when the exchange has a new session to report.
//
// Both store reads are checked against that, since both are how the rest of
// the app sees quotes: LatestQuotes must follow the exchange's newest session,
// and QuoteOn must still find the older one at its own date.
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

	// Two refreshes while the exchange still reports the same session: the
	// half-hourly job must not turn one session into a row per run.
	refresh("276.52", friday)
	refresh("276.52", friday)
	if n := rows(); n != 1 {
		t.Fatalf("%d rows after two refreshes of the same session, want 1", n)
	}

	// The exchange moves on: a second row, and the newer one is what the
	// positions screen reads.
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
	// The earlier session is still there, at its own date, for anything
	// valuing a position as of that day.
	old, err := store.QuoteOn(ctx, sber.ID, friday)
	if err != nil {
		t.Fatalf("QuoteOn(friday): %v", err)
	}
	if !old.On.Equal(friday) || !old.Price.Equal(dec("276.52")) {
		t.Errorf("QuoteOn(friday) = %s on %s, want 276.52 on %s", old.Price, old.On.Format(time.DateOnly), friday.Format(time.DateOnly))
	}
}

// TestQuotesWorker_RefusesAQuoteDatedZeroOrAfterToday is the worker-side
// guard against a quote the provider itself cannot be relied on to reject:
// QuoteProvider.QuotesFor deliberately takes no date argument any more
// (that is what #90 fixed), which means a provider also has no "today" of
// its own to compare a price's date against. Only the worker does.
//
// The stakes are why this is more than tidiness. LatestQuotes is
// `DISTINCT ON (instrument_id) ... ORDER BY on_date DESC`, so a single quote
// wrongly dated in the future would outrank every genuine refresh that
// follows it until the calendar caught up — for a date far enough out,
// effectively forever, on every screen showing that instrument, with nothing
// in any log to say why. The old time.Now()-stamped quotes could not produce
// this failure at all; it is new precisely because the provider now supplies
// the date.
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
			log, records := newRecordingLogger()
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

			line := onlyLine(t, *records, futureOrZeroQuoteDateMsg)
			if line.level != slog.LevelWarn {
				t.Errorf("the bad date was logged at %s, want WARN: this is not a routine absence like a "+
					"missing price, it is data that cannot be true", line.level)
			}
			if got := line.attrs["ticker"]; got != "SBER" {
				t.Errorf("ticker attribute = %q, want SBER", got)
			}

			// The debug line for "no price at all" belongs to a different
			// ticker in a different state; it must not also fire here, or the
			// log would carry two different explanations for the one quote.
			if lines := linesFor(*records, "marketdata: no price for ticker, skipping"); len(lines) != 0 {
				t.Errorf("also logged the no-price line, want only the refusal above: the provider DID "+
					"answer for this ticker, just not with a storable date:\n%s", showLines(lines))
			}
		})
	}
}

// TestQuotesWorker_AcceptsAQuoteDatedExactlyToday pins the boundary the
// guard above must not overreach past: "today" itself is a real session a
// quote can legitimately be dated, and only a date strictly AFTER it is
// refused. A guard written as >= instead of > would reject today's own
// price along with tomorrow's, which is a second false rejection this test
// exists to catch on its own.
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

// futureDay returns midnight UTC, n calendar days after the real "today" —
// used to build a date the worker's Work (which reads the wall clock
// directly, not through an injectable seam) will reliably see as today or
// later, whatever moment the test happens to run at. n == 0 is today itself.
func futureDay(n int) time.Time {
	now := time.Now().UTC().AddDate(0, 0, n)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// futureOrZeroQuoteDateMsg is the quotes worker's message for a quote it
// refused for carrying no date, or one dated after today — copied here
// rather than imported so that rewording the production message turns this
// test red, the same reasoning behind droppedRateMsg above.
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

// logLine is one record a worker logged, kept as structure rather than as
// rendered text. Matching a substring against a log buffer cannot tell a Warn
// from a Debug: the same words render either way, so demoting a message —
// which makes it vanish on a production instance, where the level is info —
// would leave such a test green while the operator loses the only signal there
// is. That mistake has been made in this repository before. Here the level and
// the attributes are separate fields, so an assertion has to name which one it
// means.
type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recordingHandler collects every record, at every level, into records.
type recordingHandler struct {
	records *[]logLine
	base    []slog.Attr
}

// newRecordingLogger returns a logger that keeps what it is given, and the
// slice it keeps it in.
func newRecordingLogger() (*slog.Logger, *[]logLine) {
	var records []logLine
	return slog.New(&recordingHandler{records: &records}), &records
}

// Enabled is true at every level: a test about a Debug line must be able to
// see one, and the level a record carries is asserted on directly rather than
// filtered here.
func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	line := logLine{
		level: r.Level,
		msg:   r.Message,
		attrs: make(map[string]string, len(h.base)+r.NumAttrs()),
	}
	for _, a := range h.base {
		line.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	*h.records = append(*h.records, line)
	return nil
}

// WithAttrs carries the attributes forward rather than dropping them. No
// worker uses logger.With today; one that started would otherwise lose exactly
// the attributes these tests assert on, and the failure would point at the
// code under test instead of at this helper.
func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{records: h.records, base: append(slices.Clip(h.base), attrs...)}
}

// WithGroup panics rather than pretending: grouped attributes would land in
// this flat map under their bare keys, which is not what the caller wrote, and
// a test passing on a misread record is worse than one that stops.
func (h *recordingHandler) WithGroup(string) slog.Handler {
	panic("recordingHandler: grouped attributes are not modelled; teach it if a worker starts using them")
}

// droppedRateMsg is the message the fx workers log for a rate the fx_rates
// table would refuse. It is spelled out here rather than imported from the
// package under test on purpose: a copy is what makes rewording the production
// message turn these tests red, where a shared constant would let the two move
// together and pin nothing.
const droppedRateMsg = "marketdata: source published a rate that is not positive, dropping it (this pair keeps whatever earlier rate it already has)"

// emptySeriesMsg, allRefusedMsg and downloadedMsg are the backfill worker's
// three verdicts on one currency's series, copied here for the same reason
// droppedRateMsg is: they name three different causes with the same
// user-visible outcome, and a test that let them drift would be a test that
// stopped telling them apart.
const (
	emptySeriesMsg = "marketdata: source published no rates for currency over the whole range (its amounts stay unconverted)"
	allRefusedMsg  = "marketdata: every rate the source published for this currency was refused as not positive (its amounts keep whatever earlier rates they already have)"
	downloadedMsg  = "marketdata: downloaded fx history"
)

// linesFor returns every captured record carrying this exact message, in the
// order they were logged.
func linesFor(records []logLine, msg string) []logLine {
	var found []logLine
	for _, r := range records {
		if r.msg == msg {
			found = append(found, r)
		}
	}
	return found
}

// onlyLine returns the single captured record carrying this exact message.
func onlyLine(t *testing.T, records []logLine, msg string) logLine {
	t.Helper()
	found := linesFor(records, msg)
	if len(found) != 1 {
		t.Fatalf("%d records with message %q, want exactly 1:\n%s", len(found), msg, showLines(records))
	}
	return found[0]
}

func showLines(records []logLine) string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.level.String() + " " + r.msg + " " + fmt.Sprint(r.attrs)
	}
	return strings.Join(out, "\n")
}

// --- ticker collisions ------------------------------------------------------

// fakeInstrumentLister hands the quotes worker a catalog listing directly.
//
// The state it exists to produce — two tradable instruments under one ticker —
// cannot be built through instrument.Store any more: migration 0011 makes
// the column unique and the store refuses the second row (see
// instrument.TestTickerIsUniqueAmongInstrumentsThatCarryOne). That is exactly
// why the worker checks as well: it consumes a LIST, not a table, and a
// mapping step that silently drops an entry has to say so whatever the list
// came from — a widened ListTradable, a second catalog source, or an index
// somebody dropped by hand.
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

// TestQuotesWorker_OneTickerTwoCurrenciesArePricedApart is the pair the catalog
// now allows, and the reason it can: AT&T trades as "T" in dollars and
// Т-Технологии as "T" in rubles. The exchange answering in rubles is answering
// about the Russian company, and matching on the currency is what says so.
//
// Before this, one of the two was priced with the other's number or not priced
// at all, depending on the order a list came back in — which is why migration
// 0011 forbade the pair outright and left the owner unable to catalogue the
// paper his broker reports.
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

	// Asked ONCE: the provider's interface speaks in bare tickers, and two rows
	// sharing one have nothing extra to ask about.
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

// TestQuotesWorker_TwoInstrumentsUnderOneTickerAndCurrencyArePricedNeitherWay is
// issue #26 as it stands now. The worker matches a price to a catalog row by
// ticker AND currency, so two papers under one ticker on two exchanges are told
// apart by the currency the provider itself reports — that is what lets AT&T and
// Т-Технологии both live under "T" (migration 0020).
//
// What remains ambiguous is a pair that agrees on BOTH. There nothing separates
// them, and the price goes to neither: handing it to whichever row was seen
// first is a coin toss over which company a real number belongs to. Before #26
// that coin toss happened in silence.
//
// WARN, and the run continues. Warn rather than Debug because Debug is off on a
// production instance, which is precisely where the silence hurt; Warn rather
// than a failed job because retrying cannot resolve a duplicate — River would
// retry forever and every other instrument would stop being priced too.
func TestQuotesWorker_TwoInstrumentsUnderOneTickerAndCurrencyArePricedNeitherWay(t *testing.T) {
	store, instStore, ctx := newJobsFixture(t)

	// Two real catalog rows, so the quote written for the winner has an
	// instrument to point at (quotes.instrument_id is a foreign key). The
	// collision is then staged in the LIST the worker is handed, which is the
	// only place it can exist now that the column is unique.
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
	log, records := newRecordingLogger()
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: []instrument.Instrument{sber, twin}}, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v — one duplicated ticker must not cost the whole refresh", err)
	}

	line := onlyLine(t, *records, "marketdata: two instruments share a ticker AND a currency, so neither can be priced")
	if line.level != slog.LevelWarn {
		t.Errorf("the collision was logged at %s, want WARN: Debug is off on a production instance, "+
			"which is exactly where this went unnoticed", line.level)
	}
	// The attributes have to name all three things a person needs to fix it:
	// which ticker, which instrument got the price, which one did not.
	if got := line.attrs["ticker"]; got != "SBER" {
		t.Errorf("ticker attribute = %q, want SBER", got)
	}
	if got := line.attrs["currency"]; got != "RUB" {
		t.Errorf("currency attribute = %q, want RUB — it is half of what a price is matched by", got)
	}
	if got := line.attrs["instrument_id"]; got != sber.ID.String() {
		t.Errorf("instrument_id = %q, want %s", got, sber.ID)
	}
	if got := line.attrs["other_instrument_id"]; got != twin.ID.String() {
		t.Errorf("other_instrument_id = %q, want %s", got, twin.ID)
	}

	// The duplicate is dropped from the request too, not asked for twice.
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"SBER"}) {
		t.Errorf("provider was asked for %v, want one SBER: the second row adds nothing to ask for", calls)
	}

	// NEITHER is priced, which is the outcome the warning exists to explain, so
	// it has to be the outcome that happens. Being seen first must buy nothing:
	// the price belongs to one of these two papers and nothing here knows which.
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

// TestQuotesWorker_TickerTheCatalogDoesNotHoldIsLoggedAtDebug closes the last
// silent drop in this worker: a ticker the provider reported that no catalog
// row carries used to be skipped with no record at all — unlike the sibling
// case, "we asked and got no price", which has always logged. Silence has to
// be a decision, so the two now match.
//
// DEBUG, deliberately, and the same level as that sibling. Nothing of ours
// goes unvalued because of it: the provider is asked for a fixed list and is
// free to answer with whatever it likes (MOEX filters its own boards, so this
// is empty in practice), and any instrument that did go unpriced says so on
// its own line. Promoting it to Warn would put lines an operator can do
// nothing about into every production log; leaving it silent would hide the
// one thing that explains a ticker spelt differently on the two sides.
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
	log, records := newRecordingLogger()
	worker := marketdata.NewQuotesWorker(store, instStore, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v — an unknown ticker must not fail the batch", err)
	}

	line := onlyLine(t, *records, "marketdata: provider reported a ticker the catalog does not hold, ignoring it")
	if line.level != slog.LevelDebug {
		t.Errorf("the unknown ticker was logged at %s, want DEBUG: it costs no instrument its price, "+
			"and a louder level would fill every production log with something nobody can act on", line.level)
	}
	if got := line.attrs["ticker"]; got != "SBER-RM" {
		t.Errorf("ticker attribute = %q, want SBER-RM", got)
	}
	if got := line.attrs["provider"]; got != "fake-quotes" {
		t.Errorf("provider attribute = %q, want fake-quotes", got)
	}

	// The known ticker in the same batch is still stored: one unknown name
	// must not cost the quotes that came with it.
	latest, err := store.LatestQuotes(ctx, []uuid.UUID{sber.ID})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if q, ok := latest[sber.ID]; !ok || !q.Price.Equal(dec("305.5")) {
		t.Errorf("sber quote = %+v, ok = %v, want 305.5", q, ok)
	}
}

// TestQuotesWorker_InstrumentsWithNoTickerAreNotReportedAsACollision is about
// what the worker SAYS in the one scenario its collision check exists for.
// That check defends against a list ListTradable would not have produced —
// including one that stopped excluding tickerless rows. Every such row keys on
// the empty string, so they would all collide with one another, and the
// collision line would announce "two instruments share a ticker, only one of
// them can be priced" with ticker="": they share no ticker, and neither is
// priced with or without the other. A guard kept for a case it describes
// wrongly is worse than no guard, so the empty ticker is recognised for what it
// is, on a line of its own, and nothing is asked of the provider for it.
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
	log, records := newRecordingLogger()
	worker := marketdata.NewQuotesWorker(store,
		fakeInstrumentLister{insts: tickerless}, provider, log)

	if err := worker.Work(ctx, quotesJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	for _, r := range *records {
		if r.msg == "marketdata: two instruments share a ticker, only one of them can be priced" {
			t.Errorf("two tickerless instruments were reported as sharing a ticker (ticker=%q): "+
				"they share the absence of one, and neither is priced either way", r.attrs["ticker"])
		}
	}
	// Each of them is still accounted for — silence is what this worker was
	// fixed for — and named by id, there being no ticker to name it by.
	var seen []string
	for _, r := range *records {
		if r.msg == "marketdata: instrument has no ticker, there is nothing to ask a price for" {
			if r.level != slog.LevelDebug {
				t.Errorf("a tickerless instrument was logged at %s, want DEBUG: it loses no price it could have had", r.level)
			}
			seen = append(seen, r.attrs["instrument_id"])
		}
	}
	for _, inst := range tickerless {
		if !slices.Contains(seen, inst.ID.String()) {
			t.Errorf("%q was dropped from the mapping without a line naming it; logged ids: %v", inst.Name, seen)
		}
	}

	// And the empty string never reaches the provider: it is not a ticker, and
	// asking for it would be asking for nothing under a name.
	if len(calls) != 1 || len(calls[0]) != 0 {
		t.Errorf("provider was asked for %q, want one call with nothing in it", calls)
	}
}

// --- historical fx backfill -------------------------------------------------

// cbrIDs stands in for the Bank of Russia's ISO code -> internal identifier
// map. The identifiers are the real ones (the lira's genuinely carries a
// letter suffix), so asserting on them also pins that it is the *internal*
// identifier, not the ISO code, that reaches the history endpoint.
var cbrIDs = map[string]string{
	"USD": "R01235",
	"EUR": "R01239",
	"GBP": "R01035",
	"TRY": "R01700J",
}

// rangeRequest is one call to RatesRange: which currency, under which
// internal identifier, over which range.
type rangeRequest struct {
	code       string
	currencyID string
	from, to   time.Time
}

// recordingHistoryProvider is a network-free stand-in for
// marketdata.FxHistoryProvider that records every request it receives, so a
// test can assert both how many requests a run made and what each one
// covered — the difference between "one request for the whole range" and
// "the range walked in pieces".
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

// RatesOn exists only because FxHistoryProvider embeds FxProvider: the
// backfill job must never fetch history one day at a time, so a call here is
// itself a failure.
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

// RatesRange answers with two rates, one dated at each end of the requested
// range, so what was stored reflects what was asked for.
//
// For zeroFor's code the older of the two carries a zero rate — a value the
// fx_rates CHECK refuses — while the newer stays sound, so a test can tell
// "the bad record was dropped" apart from "the series was thrown away".
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

// codesAsked lists the ISO codes the provider was asked for a series of, in
// call order.
func (p *recordingHistoryProvider) codesAsked() []string {
	out := make([]string, len(p.requests))
	for i, r := range p.requests {
		out[i] = r.code
	}
	return out
}

// fakeOpStore stands in for the two *operation.Store methods the backfill
// worker uses, so these tests can set a lower bound and a currency set
// without building a whole space/account/operation tree. err and
// currenciesErr are independent so a test can make EarliestOccurredOn
// succeed while DistinctCurrencies fails (or vice versa) — the two are read
// at different points in Work, and the read-currencies branch must be
// reachable without the range-start branch tripping first.
type fakeOpStore struct {
	earliest      time.Time
	err           error
	currencies    []string
	currenciesErr error
}

func (s fakeOpStore) EarliestOccurredOn(context.Context) (time.Time, error) {
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

// pinnedToday is the date backfill tests pin the worker's clock to: the upper
// end of every range it should request. It sits far from the real wall clock
// on purpose — a worker that read time.Now() instead of its injected clock
// would ask for a visibly different range.
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

// riverInsertClient wires up an insert-only River client and returns a
// context carrying it, the way River itself supplies one to a running
// worker's job context. The backfill job must not enqueue anything at all
// any more, and a context without a client could hide an attempt to.
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

// utcToday is the wall clock's current date at midnight UTC — the upper range
// bound a worker built by the production constructor must use.
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

// showRequests renders the recorded requests as "CODE(id):from..to", so a
// failure message says which ranges were actually asked for.
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
	// Currencies are in use, but with no operations there is no date range to
	// fetch them over, so the source must not be touched at all.
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

// TestBackfillFx_EarliestOperationLookupErrorFailsTheJob covers the read
// failure at jobs.go's rangeStart that is distinct from "no rows": unlike
// pgx.ErrNoRows, which means "nothing to fetch" and must not fail the job,
// any other error means the store could not be read at all and must fail
// it — otherwise the job goes green while the database silently sat there
// unreadable.
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
	// Rates are stored as "currency -> RUB", so an all-RUB instance needs no
	// rates at all: the rouble against itself is not a thing to download.
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

	// This provider quotes RUB, which the real source does not. Without it, a
	// RUB request would be dropped for want of an identifier and the "rates
	// are quoted against RUB, never fetched for it" rule would look enforced
	// when nothing enforced it.
	ids := map[string]string{"RUB": "R00000"}
	maps.Copy(ids, cbrIDs)
	provider := &recordingHistoryProvider{ids: ids}
	// USD is deliberately in both lists — an account is denominated in it and
	// operations are recorded in it — so a currency in use twice is still one
	// download.
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

// TestBackfillFx_AccountCurrenciesReadErrorFailsTheJob covers the account
// currency read in wantedCurrencies. A swallowed error here would let the
// job report success while silently working from an incomplete (or empty)
// currency set — rates for currencies only ever held in accounts, never
// mentioned in an operation or a space base, would quietly stop being
// fetched.
func TestBackfillFx_AccountCurrenciesReadErrorFailsTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("db unreachable")
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR"}},
		fakeAccountStore{err: wantErr},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when the account currency read fails",
			provider.idCalls, showRequests(provider.requests))
	}
}

// TestBackfillFx_OperationCurrenciesReadErrorFailsTheJob covers the
// operation currency read in wantedCurrencies — a different call than the
// EarliestOccurredOn read rangeStart makes, so both must fail the job on
// their own, independently of one another.
func TestBackfillFx_OperationCurrenciesReadErrorFailsTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("db unreachable")
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currenciesErr: wantErr},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when the operation currency read fails",
			provider.idCalls, showRequests(provider.requests))
	}
}

// TestBackfillFx_SpaceBaseCurrenciesReadErrorFailsTheJob covers the space
// base currency read in wantedCurrencies.
func TestBackfillFx_SpaceBaseCurrenciesReadErrorFailsTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	wantErr := errors.New("db unreachable")
	provider := &recordingHistoryProvider{ids: cbrIDs}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"EUR"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{err: wantErr},
		provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries the job", err, wantErr)
	}
	if provider.idCalls != 0 || len(provider.requests) != 0 {
		t.Fatalf("provider: %d currency-id calls, requests [%s]; want none when the space base currency read fails",
			provider.idCalls, showRequests(provider.requests))
	}
}

func TestBackfillFx_IncludesTheSpaceBaseCurrency(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingHistoryProvider{ids: cbrIDs}
	// Nothing is held or spent in GBP, but the space totals are displayed in
	// it, so its rates are needed just the same.
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

// TestBackfillFx_AsksForEachSeriesOnceOverTheWholeRange is the point of the
// whole job: one request per currency, covering the entire range from the
// oldest operation to today. Any implementation that splits the range into
// chunks, walks the calendar, or repeats a currency fails here.
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
	// A MONTH BEFORE THE OLDEST OPERATION, not the day of it. A rate is looked
	// up by nearest earlier date, so an operation dated before the first row in
	// the table can never be converted — and rates are published on business
	// days, so asking from the operation's own day leaves a purchase on a
	// Saturday, or on the second of January, with nothing behind it. See
	// backfillLeadDays.
	wantFrom := earliest.AddDate(0, 0, -31)
	for _, r := range provider.requests {
		if !r.from.Equal(wantFrom) || !r.to.Equal(pinnedToday) {
			t.Fatalf("%s was asked for %s..%s, want the whole range %s..%s in one request",
				r.code, r.from.Format(time.DateOnly), r.to.Format(time.DateOnly),
				wantFrom.Format(time.DateOnly), pinnedToday.Format(time.DateOnly))
		}
	}

	// Both ends of both series must have landed in the database — and the older
	// end is what the lead buys: the oldest operation itself resolves to a rate
	// dated at or before it, which is the whole reason the range starts before
	// it. (The fixture's provider answers with the ends of the range it was
	// asked for, so the older row IS wantFrom.)
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

// wantWarned asserts that some logged line carries BOTH level=WARN and the
// given substring. Matching the substring against the whole buffer cannot
// tell a Warn from a Debug, so demoting one of these messages — which makes
// it vanish entirely on a production instance, where the default level is
// info — would leave such a test green while the operator loses the only
// signal there is.
func wantWarned(t *testing.T, logs *bytes.Buffer, substr string) {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, substr) {
			return
		}
	}
	t.Fatalf("no WARN line mentioning %q:\n%s", substr, logs.String())
}

// TestBackfillFx_CurrencyIDsErrorFailsTheJob covers the one remaining way a
// run can fail before any series is asked for. Swallowing it would be the
// worst kind of quiet: River would close the job as successful, no retry
// would follow, no history would be downloaded for any currency at all, and
// the log would read like an ordinary run.
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

// TestBackfillFx_EmptySeriesIsWarnedNotReportedAsADownload covers a currency
// the source has an identifier for yet publishes nothing under, across the
// whole range — most likely a retired identifier. The user-visible outcome
// is the same as for a currency the source doesn't quote at all (amounts
// stay unconverted), so it has to be as visible; an Info line reading
// "rates=0" looks exactly like a run that worked.
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

	// The other currency still downloads: one silent series must not cost the
	// rest of the run.
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}
	wantWarned(t, &logs, "GBP")
}

// TestBackfillFx_NonPositiveRateIsDroppedAndTheRestOfTheSeriesStored is the
// history half of the poison batch (#28). One request brings back a whole
// multi-year series, and it is upserted as one batch: before this, a single
// unusable record anywhere in it discarded every other rate in that currency's
// entire history, and the retry brought the identical series back.
//
// A currency that is not the poisoned one is downloaded in the same run, so
// the test also says that one bad series does not end the run.
func TestBackfillFx_NonPositiveRateIsDroppedAndTheRestOfTheSeriesStored(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	log, records := newRecordingLogger()
	// GBP's series carries a zero at its older end and a sound rate at its
	// newer one; USD's is sound throughout.
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
	// ...and the refused record is not: asking on its own date must fall
	// through to nothing rather than find a zero.
	if got, err := store.FxRateOn(ctx, "GBP", "RUB", date("2024-01-10")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("FxRateOn(GBP, 2024-01-10) = %+v, err = %v, want pgx.ErrNoRows: the refused record must not be stored", got, err)
	}
	// The other currency is untouched by any of it.
	if _, err := store.FxRateOn(ctx, "USD", "RUB", pinnedToday); err != nil {
		t.Fatalf("USD rates missing after the run: %v", err)
	}

	dropped := linesFor(*records, droppedRateMsg)
	if len(dropped) != 1 {
		t.Fatalf("%d dropped-rate lines, want exactly 1:\n%s", len(dropped), showLines(*records))
	}
	if dropped[0].level != slog.LevelWarn || dropped[0].attrs["base"] != "GBP" {
		t.Fatalf("dropped-rate line = %s base=%q, want WARN for GBP:\n%s",
			dropped[0].level, dropped[0].attrs["base"], showLines(*records))
	}
}

// TestBackfillFx_WholeSeriesRefusedIsNotReportedAsAnEmptySeries is the caption
// half of the same change, and it is the mistake this repository keeps making:
// the number is right and the stated reason is not.
//
// "The source published no rates over the whole range" is a claim about the
// SOURCE. Checking it after the unusable records have been removed would make
// it fire for a currency the source published plenty for, naming a cause that
// is not the cause — while the operator, told the source is silent, goes
// looking at cbr.ru instead of at the values.
func TestBackfillFx_WholeSeriesRefusedIsNotReportedAsAnEmptySeries(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	log, records := newRecordingLogger()
	provider := &recordingHistoryProvider{ids: cbrIDs, allZero: "GBP"}
	worker := newBackfillWorker(store,
		fakeOpStore{earliest: date("2024-01-10"), currencies: []string{"GBP"}},
		fakeAccountStore{currencies: []string{"USD"}},
		fakeSpaceStore{base: []string{"RUB"}},
		provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want an entirely unusable series to be reported, not to fail the run", err)
	}

	if n := len(linesFor(*records, emptySeriesMsg)); n != 0 {
		t.Fatalf("%d empty-series lines, want 0: the source published two records, they were refused here:\n%s",
			n, showLines(*records))
	}
	line := onlyLine(t, *records, allRefusedMsg)
	if line.level != slog.LevelWarn {
		t.Fatalf("all-refused line at %s, want WARN — it leaves the currency unconverted exactly as an empty series does:\n%s",
			line.level, showLines(*records))
	}
	if line.attrs["currency"] != "GBP" {
		t.Fatalf("all-refused line names currency=%q, want GBP:\n%s", line.attrs["currency"], showLines(*records))
	}
	// An Info line reading "rates=0" would look like an ordinary run, which is
	// the same trap TestBackfillFx_EmptySeriesIsWarnedNotReportedAsADownload
	// closes for the other cause.
	for _, l := range linesFor(*records, downloadedMsg) {
		if l.attrs["currency"] == "GBP" {
			t.Fatalf("GBP reported as a download:\n%s", showLines(*records))
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
	// The dropped tail must be visible, not silently swallowed. Measured from
	// the day the request WOULD have started at — a month before the operation
	// (see backfillLeadDays) — because that is what the clamp actually cut.
	wantDropped := int(floor.Sub(date("1970-01-01").AddDate(0, 0, -31)).Hours() / 24)
	wantWarned(t, &logs, "days_dropped="+strconv.Itoa(wantDropped))
}

func TestBackfillFx_UnquotedCurrencyIsSkippedWithALogAndTheRestStillDownloaded(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// BYN is deliberately absent from cbrIDs: a source that doesn't quote a
	// currency has no identifier for it, so its series can't be requested.
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
	// EUR is requested before USD, so EUR's series is already stored when the
	// USD request fails.
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

// TestBackfillFx_StoreSaveErrorFailsTheJob covers the write side of a run: a
// series the provider handed over successfully but the store then fails to
// persist. Swallowing this would be the worst case for "honesty over
// silence" — the provider call, and therefore the whole run, would report
// success while nothing landed in the database at all. The pool is closed
// ahead of the call to force a real Postgres error out of UpsertFxRates,
// rather than stubbing the store behind an interface it doesn't otherwise
// need.
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

// TestBackfillFx_RepeatRunRefetchesTheRangeWithoutDuplicatingRows pins the
// deliberate choice behind dropping the old coverage bookkeeping: a re-run
// asks for the whole range again (which is what heals any hole an outage
// left), it just overwrites the same rows — and it never queues a follow-up
// job of its own.
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

// TestBackfillFx_FutureDatedOperationDoesNotInvertTheRange covers a mistyped
// (or genuinely future-dated) operation: without a clamp the range would run
// backwards, which the source rejects, and every run would fail for as long
// as that operation stays in the future.
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

// TestBackfillFx_ANearFutureOperationIsStillReported is the case the lead
// created and nearly hid. The range now starts a month before the earliest
// operation (backfillLeadDays), so an operation dated a few days ahead no longer
// makes the range run backwards — and a check written against the padded start
// would fall silent about a date nobody can have meant. It is asked of the
// OPERATION's own day for exactly that reason.
//
// Five days ahead: well inside the lead, so nothing about the range itself
// complains.
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

// TestBackfillFx_FutureDatedOperationWarnsEvenWhenNoCurrencyToFetch covers an
// instance where every account and operation is in RUB — wantedCurrencies
// comes back empty and the run skips the provider entirely — while the
// earliest operation's date is also corrupted into the future. The data
// problem exists either way, so the warning must fire regardless of whether
// there happens to be a currency left to fetch; a check order that lets the
// empty-currency exit skip it would hide the very mistake the warning exists
// to surface.
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

// TestBackfillFx_ProductionConstructorUsesTheWallClock guards the clock the
// production constructor wires in: every other backfill test pins it, so a
// missing (or zero) clock there would go unnoticed.
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

	// River's default job timeout is one minute; one currency's twelve-year
	// series is megabytes of XML, and a run fetches one such series per
	// currency in use.
	if got := worker.Timeout(backfillJob()); got <= time.Minute {
		t.Fatalf("Timeout = %s, want more than River's one-minute default", got)
	}
}
