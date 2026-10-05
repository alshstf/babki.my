package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// screenCost is what one GET of an account screen cost and answered. trips
// counts pool acquisitions — one per statement, since these read paths hold no
// connection across statements (#72). rate and batch are kept for diagnosing
// fallbacks; a Rate call is one to six statements, so they are not the cost.
type screenCost struct {
	trips int64
	rate  int64
	batch int64
}

func (c screenCost) String() string {
	return fmt.Sprintf("%d database round trips (one-pair rate lookups %d, batched rate resolutions %d)",
		c.trips, c.rate, c.batch)
}

// screenCurrencies are the fixtures' non-base currencies, each with a direct
// RUB rate, so the unbatched cost is one lookup per distinct currency.
var screenCurrencies = []string{"USD", "EUR", "GBP", "CHF", "CNY", "KZT", "TRY", "SEK"}

// countingConverter counts what a screen asks of the fx layer while a real
// converter answers. keep filters the batch (a hole in the enumeration);
// batchErr fails the batch alone (#70), unlike failingConverter's outage.
// Counts are atomic: the handler runs on the server's goroutine.
type countingConverter struct {
	inner    *marketdata.Converter
	keep     func(marketdata.RateQuery) bool
	batchErr error
	rate     atomic.Int64
	batch    atomic.Int64
}

// dropping and failingBatch tune the double: a hole in the enumeration, or a
// batch that dies alone.
func dropping(pred func(marketdata.RateQuery) bool) func(*countingConverter) {
	return func(c *countingConverter) { c.keep = pred }
}

func failingBatch(err error) func(*countingConverter) {
	return func(c *countingConverter) { c.batchErr = err }
}

func (c *countingConverter) ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (int64, []string, time.Time, error) {
	return c.inner.ConvertMany(ctx, amounts, to, on)
}

func (c *countingConverter) Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error) {
	c.rate.Add(1)
	return c.inner.Rate(ctx, from, to, on)
}

func (c *countingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	c.batch.Add(1)
	if c.batchErr != nil {
		// The zero Rates with the error, as RatesOn returns on failure.
		return marketdata.Rates{}, c.batchErr
	}
	if c.keep == nil {
		return c.inner.RatesOn(ctx, queries)
	}
	kept := make([]marketdata.RateQuery, 0, len(queries))
	for _, q := range queries {
		if c.keep(q) {
			kept = append(kept, q)
		}
	}
	return c.inner.RatesOn(ctx, kept)
}

// newAPIOnPool wires family and account onto pool with conv as the converter
// and returns the URL and a logged-in client, so tests count acquisitions on
// the handler's own pool.
func newAPIOnPool(t *testing.T, pool *pgxpool.Pool, conv *countingConverter) (string, *http.Client) {
	t.Helper()
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, conv, nil, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("setup: %v %d", err, resp.StatusCode)
	}
	return ts.URL, client
}

// accountsFixture builds two accounts in each of the first `currencies`
// screen currencies, each currency with a direct RUB rate. Two per currency so
// the measured growth is in currencies, not rows.
func accountsFixture(t *testing.T, currencies int, tune func(*countingConverter)) (string, *http.Client, *pgxpool.Pool, *countingConverter) {
	t.Helper()
	if currencies > len(screenCurrencies) {
		t.Fatalf("fixture asks for %d currencies, only %d are defined", currencies, len(screenCurrencies))
	}
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore)}
	if tune != nil {
		tune(conv)
	}
	url, c := newAPIOnPool(t, pool, conv)

	on := pastOn()
	rates := make([]marketdata.FxRate, 0, currencies)
	for i, currency := range screenCurrencies[:currencies] {
		// A distinct rate per currency, so a mixed-up memo shows a wrong number.
		rates = append(rates, marketdata.FxRate{
			Base: currency, Quote: "RUB", On: on,
			Rate: decimal.NewFromInt(int64(10 + i)), Source: "test",
		})
	}
	if err := mdStore.UpsertFxRates(t.Context(), rates); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	for i, currency := range screenCurrencies[:currencies] {
		for j := range 2 {
			id := mkAccount(t, url, c, fmt.Sprintf("%s счёт %d", currency, j), currency)
			setBalance(t, url, c, id, int64(100000+1000*i+j))
		}
	}
	return url, c, pool, conv
}

// poolTrips is the pool's lifetime acquisition count; its difference across a
// request is the request's round trips.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// getScreen fetches path once and reports what that request cost; counters are
// reset just before it.
func getScreen(t *testing.T, url, path string, c *http.Client, pool *pgxpool.Pool, conv *countingConverter, out any) screenCost {
	t.Helper()
	conv.rate.Store(0)
	conv.batch.Store(0)
	before := poolTrips(pool)

	resp := do(t, c, "GET", url+path, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, b)
	}
	cost := screenCost{trips: poolTrips(pool) - before, rate: conv.rate.Load(), batch: conv.batch.Load()}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return cost
}

func accountsScreen(t *testing.T, currencies int, tune func(*countingConverter)) (screenCost, []accountListItem) {
	t.Helper()
	url, c, pool, conv := accountsFixture(t, currencies, tune)
	var body []accountListItem
	cost := getScreen(t, url, "/api/v1/accounts", c, pool, conv, &body)
	return cost, body
}

// summaryScreen is accountsScreen's twin for GET /summary.
func summaryScreen(t *testing.T, currencies int, tune func(*countingConverter)) (screenCost, summaryResponse) {
	t.Helper()
	url, c, pool, conv := accountsFixture(t, currencies, tune)
	var body summaryResponse
	cost := getScreen(t, url, "/api/v1/summary", c, pool, conv, &body)
	return cost, body
}

// assertAccountsAreFullyWorked fails unless every figure was published: a
// screen converting nothing would be cheapest.
func assertAccountsAreFullyWorked(t *testing.T, rows []accountListItem, currencies int) {
	t.Helper()
	if len(rows) != 2*currencies {
		t.Fatalf("screen has %d accounts, want %d", len(rows), 2*currencies)
	}
	seen := make(map[string]bool, currencies)
	for _, a := range rows {
		if a.BalanceInBase == nil {
			t.Fatalf("balance_in_base = null on the %s account: every currency here has a rate seeded before today", a.Currency)
		}
		seen[a.Currency] = true
	}
	if len(seen) != currencies {
		t.Fatalf("screen shows %d distinct currencies, want %d — the fixture is not exercising the axis under test", len(seen), currencies)
	}
}

// assertSummaryIsFullyWorked is the same for the summary.
func assertSummaryIsFullyWorked(t *testing.T, sum summaryResponse, currencies int) {
	t.Helper()
	if len(sum.Totals) != currencies {
		t.Fatalf("summary covers %d currencies, want %d", len(sum.Totals), currencies)
	}
	if sum.TotalInBaseMinor == nil {
		t.Fatalf("total_in_base_minor = null: every currency here has a rate seeded before today")
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want [] — nothing on this screen should fail to convert", sum.Unconverted)
	}
}

// The accounts screen costs the same round trips whatever the number of
// distinct currencies. Two runs are compared rather than a magic number.
func TestAccountsScreenRoundTripsDoNotGrowWithCurrencies(t *testing.T) {
	small, smallBody := accountsScreen(t, 2, nil)
	large, largeBody := accountsScreen(t, 5, nil)

	assertAccountsAreFullyWorked(t, smallBody, 2)
	assertAccountsAreFullyWorked(t, largeBody, 5)
	t.Logf("2 currencies: %s", small)
	t.Logf("5 currencies: %s", large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the number of currencies: 2 currencies cost %s, 5 currencies cost %s", small, large)
	}
}

// The summary, which converts through ConvertMany, is pinned the same way.
func TestSummaryRoundTripsDoNotGrowWithCurrencies(t *testing.T) {
	small, smallBody := summaryScreen(t, 2, nil)
	large, largeBody := summaryScreen(t, 5, nil)

	assertSummaryIsFullyWorked(t, smallBody, 2)
	assertSummaryIsFullyWorked(t, largeBody, 5)
	t.Logf("2 currencies: %s", small)
	t.Logf("5 currencies: %s", large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the number of currencies: 2 currencies cost %s, 5 currencies cost %s", small, large)
	}
}

// A hole in the prefetch costs round trips, never numbers: the per-pair lookup
// resolves what was not asked for, and the screen is identical.
func TestAccountsIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	full, fullBody := accountsScreen(t, 3, nil)
	assertAccountsAreFullyWorked(t, fullBody, 3)
	// With nothing dropped nothing falls back; a forgotten currency costs one
	// lookup regardless of size, which only this shows.
	if full.rate != 0 {
		t.Fatalf("the complete prewarm still fell back to %d one-pair lookups: %s — some rate the loop asks for is not among the ones the enumeration names, or is enumerated under a different key than it is looked up by",
			full.rate, full)
	}
	if full.batch != 1 {
		t.Fatalf("the screen made %d batched rate resolutions, want exactly 1: %s", full.batch, full)
	}

	for _, tc := range []struct {
		name string
		keep func(marketdata.RateQuery) bool
	}{
		{"one currency is missed", func(q marketdata.RateQuery) bool { return q.From != screenCurrencies[1] }},
		{"nothing is prewarmed", func(marketdata.RateQuery) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial, partialBody := accountsScreen(t, 3, dropping(tc.keep))

			// Separate databases: only account ids differ.
			if !reflect.DeepEqual(blankAccountIDs(partialBody), blankAccountIDs(fullBody)) {
				t.Fatalf("an incomplete prewarm changed the answer:\n got %+v\nwant %+v", partialBody, fullBody)
			}
			if partial.rate <= full.rate {
				t.Fatalf("prewarm dropped queries but nothing fell back: %s (complete prewarm: %s) — the fake is not dropping what it claims to",
					partial, full)
			}
			if partial.trips <= full.trips {
				t.Fatalf("prewarm dropped queries but the request cost no more: %s (complete prewarm: %s)", partial, full)
			}
		})
	}
}

// A batch that fails alone (#70) gives the identical screen at the cost of
// round trips, never an error page. An outage that takes the fallback down
// fails the request (see TestListRealRateErrorFailsRequest).
func TestAccountsFailedBatchCostsTripsNotNumbers(t *testing.T) {
	full, fullBody := accountsScreen(t, 3, nil)
	assertAccountsAreFullyWorked(t, fullBody, 3)

	dead, deadBody := accountsScreen(t, 3, failingBatch(errors.New("statement timeout on the batched fx lookup")))
	assertAccountsAreFullyWorked(t, deadBody, 3)

	// Separate databases: only account ids differ.
	if !reflect.DeepEqual(blankAccountIDs(deadBody), blankAccountIDs(fullBody)) {
		t.Fatalf("a failed batch changed the answer:\n got %+v\nwant %+v", deadBody, fullBody)
	}
	if dead.rate <= full.rate {
		t.Fatalf("the batch failed but nothing fell back: %s (working batch: %s) — the double is not failing what it claims to",
			dead, full)
	}
	if dead.trips <= full.trips {
		t.Fatalf("the batch failed but the request cost no more: %s (working batch: %s)", dead, full)
	}
}

// A currency without a rate comes back from the batch as ErrNoRate and is
// filed as an answer, not asked again. Only the fallback count shows it.
func TestAccountsGapIsFiledNotAskedAgain(t *testing.T) {
	// Two currencies with rates and one without.
	url, c, pool, conv := accountsFixture(t, 2, nil)
	unrated := screenCurrencies[len(screenCurrencies)-1]
	id := mkAccount(t, url, c, unrated+" счёт", unrated)
	setBalance(t, url, c, id, 500000)

	var body []accountListItem
	cost := getScreen(t, url, "/api/v1/accounts", c, pool, conv, &body)
	t.Logf("%d currencies, one of them unrated: %s", 3, cost)

	var gaps, converted int
	for _, a := range body {
		switch {
		case a.Currency == unrated && a.BalanceInBase == nil:
			gaps++
		case a.Currency != unrated && a.BalanceInBase != nil:
			converted++
		default:
			t.Fatalf("account in %s published balance_in_base %v: every currency but %s has a rate seeded, and %s has none",
				a.Currency, a.BalanceInBase, unrated, unrated)
		}
	}
	if gaps != 1 || converted != 4 {
		t.Fatalf("screen shows %d gap(s) and %d converted rows, want 1 and 4 — the fixture is not exercising a gap beside working conversions", gaps, converted)
	}
	if cost.batch != 1 {
		t.Fatalf("the screen made %d batched rate resolutions, want exactly 1: %s", cost.batch, cost)
	}
	if cost.rate != 0 {
		t.Fatalf("the screen fell back to %d one-pair lookups: %s — the batch answered «no rate» for %s and that answer must be filed in the memo, not thrown away and asked for again",
			cost.rate, cost, unrated)
	}
}

// blankAccountIDs clears the ids, which differ between runs.
func blankAccountIDs(rows []accountListItem) []accountListItem {
	out := make([]accountListItem, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].ID = ""
	}
	return out
}
