package marketdata_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

// --- historical fx backfill -------------------------------------------------

// recordingFxProvider records every date it is asked about so backfill tests
// can assert exactly which dates were requested, and in which order. onShift
// models the Bank of Russia's behaviour on non-working days: the response
// carries the date the rates were published on, which may be earlier than the
// date that was asked for.
type recordingFxProvider struct {
	asked     []time.Time
	onShift   int   // days subtracted from the requested date in the response
	failAfter int   // start failing with call number failAfter+1
	err       error // failure to return; nil means never fail
}

func (p *recordingFxProvider) RatesOn(_ context.Context, on time.Time) ([]marketdata.FxRate, error) {
	p.asked = append(p.asked, on)
	if p.err != nil && len(p.asked) > p.failAfter {
		return nil, p.err
	}
	return []marketdata.FxRate{{
		Base:   "USD",
		Quote:  "RUB",
		On:     on.AddDate(0, 0, -p.onShift),
		Rate:   dec("90.5"),
		Source: p.Name(),
	}}, nil
}

func (p *recordingFxProvider) Name() string { return "fake-fx" }

// fakeOpStore stands in for the single *operation.Store method the backfill
// worker uses, so these tests can set a lower bound without building a whole
// space/account/operation tree.
type fakeOpStore struct {
	earliest time.Time
	err      error
}

func (s fakeOpStore) EarliestOccurredOn(context.Context) (time.Time, error) {
	if s.err != nil {
		return time.Time{}, s.err
	}
	return s.earliest, nil
}

func newBackfillFixture(t *testing.T) (*marketdata.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return marketdata.NewStore(pool), pool, ctx
}

func backfillJob() *river.Job[marketdata.BackfillFxArgs] {
	return &river.Job[marketdata.BackfillFxArgs]{Args: marketdata.BackfillFxArgs{}}
}

// today is the upper bound the worker starts from when there is no coverage.
func today() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func weekend(d time.Time) bool {
	return d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
}

// businessDaysBack returns the date n business days before from (from itself
// is day zero, business day or not).
func businessDaysBack(from time.Time, n int) time.Time {
	d := from
	for range n {
		d = d.AddDate(0, 0, -1)
		for weekend(d) {
			d = d.AddDate(0, 0, -1)
		}
	}
	return d
}

// businessDaysDesc lists the business days in [from, to], most recent first —
// the exact sequence a downward walk over that range must request. Built by
// walking upwards on purpose, so it doesn't mirror the worker's own loop.
func businessDaysDesc(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if !weekend(d) {
			out = append(out, d)
		}
	}
	slices.Reverse(out)
	return out
}

func sameDates(a, b []time.Time) bool {
	return slices.EqualFunc(a, b, func(x, y time.Time) bool { return x.Equal(y) })
}

func showDates(ds []time.Time) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Format(time.DateOnly)
	}
	return strings.Join(out, ",")
}

func TestBackfillFx_NoOperationsSkipsProviderEntirely(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{err: pgx.ErrNoRows}, provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(provider.asked) != 0 {
		t.Fatalf("provider asked for %s, want no calls at all when there are no operations",
			showDates(provider.asked))
	}
}

func TestBackfillFx_NoCoverageWalksDownFromTodayToEarliestOperation(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	floor := businessDaysBack(today(), 5)
	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{earliest: floor}, provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	want := businessDaysDesc(floor, today())
	if !sameDates(provider.asked, want) {
		t.Fatalf("asked for [%s], want [%s]", showDates(provider.asked), showDates(want))
	}
	for _, d := range provider.asked {
		if weekend(d) {
			t.Fatalf("asked for %s (%s): weekends must be skipped", d.Format(time.DateOnly), d.Weekday())
		}
	}
}

func TestBackfillFx_ResumesJustBelowExistingCoverage(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	// Coverage starts at have; pick it so have-1 is a business day, which
	// makes the expected first request unambiguous.
	have := today().AddDate(0, 0, -10)
	for weekend(have.AddDate(0, 0, -1)) {
		have = have.AddDate(0, 0, -1)
	}
	if err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: have, Rate: dec("90.5"), Source: "fake-fx"},
	}); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}

	floor := businessDaysBack(have, 3)
	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{earliest: floor}, provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(provider.asked) == 0 {
		t.Fatal("provider was never asked, want a walk below the coverage boundary")
	}
	wantFirst := have.AddDate(0, 0, -1)
	if !provider.asked[0].Equal(wantFirst) {
		t.Fatalf("first request = %s, want %s (not today %s, not the earliest operation %s)",
			provider.asked[0].Format(time.DateOnly), wantFirst.Format(time.DateOnly),
			today().Format(time.DateOnly), floor.Format(time.DateOnly))
	}
	// Every request must sit below the boundary and descend: the coverage
	// boundary is MIN(on_date), which only honestly means "nothing older
	// exists" while coverage grows downwards without gaps.
	for i, d := range provider.asked {
		if !d.Before(have) {
			t.Fatalf("asked for %s, at or above the coverage boundary %s",
				d.Format(time.DateOnly), have.Format(time.DateOnly))
		}
		if i > 0 && !d.Before(provider.asked[i-1]) {
			t.Fatalf("request %d (%s) does not descend below request %d (%s): [%s]",
				i, d.Format(time.DateOnly), i-1, provider.asked[i-1].Format(time.DateOnly),
				showDates(provider.asked))
		}
	}
	want := businessDaysDesc(floor, wantFirst)
	if !sameDates(provider.asked, want) {
		t.Fatalf("asked for [%s], want [%s]", showDates(provider.asked), showDates(want))
	}
}

func TestBackfillFx_ChunkIsCappedAndEnqueuesAFollowUpJob(t *testing.T) {
	store, pool, ctx := newBackfillFixture(t)

	// Five years back is far more than one chunk of business days.
	floor := today().AddDate(-5, 0, 0)
	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorkerWithPause(
		store, fakeOpStore{earliest: floor}, provider, slog.Default(), 0)

	// An insert-only River client, injected into the job context the way
	// River itself does it for a running worker.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := worker.Work(rivertest.WorkContext(ctx, client), backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	const chunk = 180
	if len(provider.asked) != chunk {
		t.Fatalf("provider asked %d times, want exactly %d per run", len(provider.asked), chunk)
	}

	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1`,
		marketdata.BackfillFxArgs{}.Kind()).Scan(&queued); err != nil {
		t.Fatalf("count queued jobs: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued follow-up jobs = %d, want 1 while the walk is unfinished", queued)
	}
}

func TestBackfillFx_MissingRiverClientDoesNotFailTheJob(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	floor := today().AddDate(-5, 0, 0)
	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorkerWithPause(
		store, fakeOpStore{earliest: floor}, provider, slog.Default(), 0)

	// Plain context: no River client in it. The chunk itself succeeded, so
	// failing the job would only make River re-fetch what is already stored.
	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v, want nil when the follow-up job cannot be enqueued", err)
	}
	if len(provider.asked) != 180 {
		t.Fatalf("provider asked %d times, want 180", len(provider.asked))
	}
}

func TestBackfillFx_ClampsAbsurdlyEarlyOperationToTheFloor(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	// Coverage starts on Wed 2000-01-05, so the walk covers 01-04 and 01-03,
	// then hits the weekend and drops below the 2000-01-01 floor.
	if err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2000-01-05"), Rate: dec("28.5"), Source: "fake-fx"},
	}); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	provider := &recordingFxProvider{}
	worker := marketdata.NewBackfillFxWorker(
		store, fakeOpStore{earliest: date("1970-01-01")}, provider, log)

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// Without clamping, the demand floor would be 1970 and the run would
	// burn a whole 180-request chunk instead of stopping at 2000-01-01.
	want := []time.Time{date("2000-01-04"), date("2000-01-03")}
	if !sameDates(provider.asked, want) {
		t.Fatalf("asked for [%s], want [%s]", showDates(provider.asked), showDates(want))
	}

	wantDropped := int(date("2000-01-01").Sub(date("1970-01-01")).Hours() / 24)
	if !strings.Contains(logs.String(), "days_dropped="+strconv.Itoa(wantDropped)) {
		t.Fatalf("log does not report the %d dropped days:\n%s", wantDropped, logs.String())
	}
}

func TestBackfillFx_CursorFollowsRequestedDatesNotPublishedOnes(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	floor := businessDaysBack(today(), 5)
	// The source answers with rates published three days before the date
	// asked for — a cursor driven by the response would jump the queue.
	provider := &recordingFxProvider{onShift: 3}
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{earliest: floor}, provider, slog.Default())

	if err := worker.Work(ctx, backfillJob()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	want := businessDaysDesc(floor, today())
	if !sameDates(provider.asked, want) {
		t.Fatalf("asked for [%s], want [%s]", showDates(provider.asked), showDates(want))
	}
}

func TestBackfillFx_ProviderErrorFailsTheJobAndKeepsWhatWasStored(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	floor := businessDaysBack(today(), 5)
	wantErr := errors.New("cbr unreachable")
	provider := &recordingFxProvider{failAfter: 2, err: wantErr}
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{earliest: floor}, provider, slog.Default())

	err := worker.Work(ctx, backfillJob())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work err = %v, want %v so River retries", err, wantErr)
	}
	if len(provider.asked) != 3 {
		t.Fatalf("provider asked %d times, want 3 (two good, one failing)", len(provider.asked))
	}

	earliest, err := store.EarliestFxDate(ctx, "fake-fx")
	if err != nil {
		t.Fatalf("EarliestFxDate after a mid-chunk failure: %v", err)
	}
	if !earliest.Equal(provider.asked[1]) {
		t.Fatalf("earliest stored date = %s, want %s (both pre-failure fetches must survive)",
			earliest.Format(time.DateOnly), provider.asked[1].Format(time.DateOnly))
	}
}

func TestBackfillFx_PauseBetweenRequestsRespectsContextCancellation(t *testing.T) {
	store, _, ctx := newBackfillFixture(t)

	floor := businessDaysBack(today(), 5)
	provider := &recordingFxProvider{}
	// Production pause (250ms): the deadline lands mid-pause, so a sleep that
	// ignores the context would return late and fire a second request.
	worker := marketdata.NewBackfillFxWorker(store, fakeOpStore{earliest: floor}, provider, slog.Default())

	deadlined, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := worker.Work(deadlined, backfillJob())
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Work err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("Work took %s, want a return as soon as the context is done", elapsed)
	}
	if len(provider.asked) != 1 {
		t.Fatalf("provider asked %d times, want 1: the pause must not outlive the context",
			len(provider.asked))
	}
}

func TestBackfillFx_JobTimeoutOutlastsAWholeChunk(t *testing.T) {
	store, _, _ := newBackfillFixture(t)

	worker := marketdata.NewBackfillFxWorker(
		store, fakeOpStore{earliest: today()}, &recordingFxProvider{}, slog.Default())

	// River's default job timeout is one minute; a chunk spends more than
	// that on its pauses alone, before any network time.
	const chunkPauses = 180 * 250 * time.Millisecond
	if got := worker.Timeout(backfillJob()); got <= chunkPauses {
		t.Fatalf("Timeout = %s, want more than one chunk of pauses (%s)", got, chunkPauses)
	}
}
