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
	return []marketdata.FxRate{
		{Base: code, Quote: "RUB", On: from, Rate: dec("90.5"), Source: p.Name()},
		{Base: code, Quote: "RUB", On: to, Rate: dec("91.5"), Source: p.Name()},
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
	for _, r := range provider.requests {
		if !r.from.Equal(earliest) || !r.to.Equal(pinnedToday) {
			t.Fatalf("%s was asked for %s..%s, want the whole range %s..%s in one request",
				r.code, r.from.Format(time.DateOnly), r.to.Format(time.DateOnly),
				earliest.Format(time.DateOnly), pinnedToday.Format(time.DateOnly))
		}
	}

	// Both ends of both series must have landed in the database.
	for _, code := range []string{"EUR", "USD"} {
		for _, on := range []time.Time{earliest, pinnedToday} {
			got, err := store.FxRateOn(ctx, code, "RUB", on)
			if err != nil {
				t.Fatalf("FxRateOn(%s, %s): %v", code, on.Format(time.DateOnly), err)
			}
			if !got.On.Equal(on) {
				t.Fatalf("%s rate nearest %s is dated %s, want the series to cover both ends",
					code, on.Format(time.DateOnly), got.On.Format(time.DateOnly))
			}
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
	// The dropped tail must be visible, not silently swallowed.
	wantDropped := int(floor.Sub(date("1970-01-01")).Hours() / 24)
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
