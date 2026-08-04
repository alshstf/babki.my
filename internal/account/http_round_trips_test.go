package account_test

import (
	"context"
	"encoding/json"
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

// screenCost is what one GET of an account screen cost and what it answered.
//
// trips is the figure that matters, and the one issue #72 says was pinned by
// nothing: connections ACQUIRED FROM THE POOL during the request — one per
// statement it sends, since nothing on either of these two paths holds a
// connection across several. Both are pure reads: account.Store.ListWithBalance,
// account.Store.SummaryByCurrency and family.Store.SpaceByID each send a single
// query on the pool, and the only transactions in this codebase's read-side
// packages are family's three write paths (CreateSpaceWithOwner,
// CreateFirstUserWithSpace, CreateUserInSpace), none of which a GET reaches.
// That is what makes AcquireCount the right instrument here and the wrong one
// in operation.TestTransferLotsCostOneRoundTrip, where a single connection is
// held from Begin to Commit and every statement inside it is invisible from the
// pool.
//
// rate and batch are kept alongside for diagnosis and for the fallback
// assertions below (a prefetch that misses shows up as one-pair lookups), never
// as the cost itself: one Rate is between one and six statements depending on
// whether the pair resolves directly, by inversion or through a RUB bridge, so
// counting calls above the converter understates what they cost.
type screenCost struct {
	trips int64
	rate  int64
	batch int64
}

func (c screenCost) String() string {
	return fmt.Sprintf("%d database round trips (one-pair rate lookups %d, batched rate resolutions %d)",
		c.trips, c.rate, c.batch)
}

// screenCurrencies are the non-base currencies the fixtures below hand out, in
// order. Every one of them gets a direct <currency>/RUB row seeded (see
// accountsFixture), so each resolves in a single store lookup on the unbatched
// path: the cost of a screen is then exactly the number of distinct currencies
// on it plus the handful of statements the request makes anyway, which is the
// growth this test exists to remove.
var screenCurrencies = []string{"USD", "EUR", "GBP", "CHF", "CNY", "KZT", "TRY", "SEK"}

// countingConverter counts what one screen asks of the fx layer while
// delegating every answer to a real *marketdata.Converter, so the figures on
// the screen stay the production ones and only their cost is observed. Mirrors
// operation's and portfolio's identically named doubles.
//
// The counts are atomic because the increments happen on the http.Server's
// handler goroutine while the assertions read them on the test's own goroutine.
//
// keep, when set, filters the batch down to the queries it accepts before
// passing them on — an enumeration with a hole in it, which is what
// TestAccountsIncompletePrewarmCostsTripsNotNumbers needs and no real converter
// would ever do.
type countingConverter struct {
	inner *marketdata.Converter
	keep  func(marketdata.RateQuery) bool
	rate  atomic.Int64
	batch atomic.Int64
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

// newAPIOnPool wires the family and account modules onto pool with conv as the
// account handler's fx converter, and returns the server URL, a logged-in
// client and the pool itself — newAPIWithConverter plus the two handles a
// round-trip test needs (the pool to count acquisitions on, the converter to
// ask what fell back).
func newAPIOnPool(t *testing.T, pool *pgxpool.Pool, conv *countingConverter) (string, *http.Client) {
	t.Helper()
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, conv, auth, sm).Mount(srv)

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

// accountsFixture builds a space holding TWO accounts in each of the first
// `currencies` entries of screenCurrencies, each with a balance of its own and
// each currency with a direct rate into the space's base currency (RUB).
//
// Two accounts per currency, not one, so the growth being measured is the
// number of distinct CURRENCIES rather than the number of rows: the memo
// already collapses same-currency accounts onto one lookup, and a fixture with
// one account each could not tell the two axes apart.
func accountsFixture(t *testing.T, currencies int, keep func(marketdata.RateQuery) bool) (string, *http.Client, *pgxpool.Pool, *countingConverter) {
	t.Helper()
	if currencies > len(screenCurrencies) {
		t.Fatalf("fixture asks for %d currencies, only %d are defined", currencies, len(screenCurrencies))
	}
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore), keep: keep}
	url, c := newAPIOnPool(t, pool, conv)

	on := pastOn()
	rates := make([]marketdata.FxRate, 0, currencies)
	for i, currency := range screenCurrencies[:currencies] {
		// A rate of its own per currency, so a memo that mixed two currencies
		// up would publish a visibly wrong number rather than the same one
		// twice.
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

// poolTrips reads the pool's lifetime count of acquired connections. Every
// statement these handlers send takes one, so the difference across a request
// is how many round trips that request made — the technique
// marketdata.TestFxRatesOnBatch uses on the store directly, applied here to a
// whole HTTP request so that a round trip made anywhere beneath the handler is
// counted, whichever layer decided to make it.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// getScreen fetches path once and reports what that one request cost, decoding
// the body into out.
//
// The counters are reset immediately before the measured request: the fixture
// was built through the same HTTP stack, so what they hold afterwards can only
// be this one GET.
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

// accountsScreen builds a space of `currencies` distinct currencies and reports
// what one GET /accounts cost and answered.
func accountsScreen(t *testing.T, currencies int, keep func(marketdata.RateQuery) bool) (screenCost, []accountListItem) {
	t.Helper()
	url, c, pool, conv := accountsFixture(t, currencies, keep)
	var body []accountListItem
	cost := getScreen(t, url, "/api/v1/accounts", c, pool, conv, &body)
	return cost, body
}

// summaryScreen is accountsScreen's twin for GET /summary.
func summaryScreen(t *testing.T, currencies int, keep func(marketdata.RateQuery) bool) (screenCost, summaryResponse) {
	t.Helper()
	url, c, pool, conv := accountsFixture(t, currencies, keep)
	var body summaryResponse
	cost := getScreen(t, url, "/api/v1/summary", c, pool, conv, &body)
	return cost, body
}

// assertAccountsAreFullyWorked fails unless the screen really published every
// figure the round trips were supposed to buy. Without it the count assertions
// would be satisfied by a handler that converts nothing at all — the cheapest
// screen is the one that answers nothing, and it must not be able to pass a
// performance test.
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

// assertSummaryIsFullyWorked is assertAccountsAreFullyWorked's twin: a total
// that came out null, or a currency left unconverted, would make the cost
// meaningless.
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

// TestAccountsScreenRoundTripsDoNotGrowWithCurrencies is half of the
// requirement this change exists for: rendering the accounts screen costs a
// FIXED number of round trips, whatever currencies it holds. The axis here is
// not the amount of data — the memo has always collapsed same-currency accounts
// onto one lookup — it is the number of DISTINCT currencies, which is what the
// memo cannot collapse and what nothing until now batched.
//
// It compares two runs of different size rather than asserting one magic
// number, deliberately. A magic number would bake in today's incidental
// statements — the session, the space, the account list — and the next person
// to add or remove one would edit the expectation and never learn whether the
// thing this test guards still holds. What must be true is not "five", it is
// "the same".
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

// TestSummaryRoundTripsDoNotGrowWithCurrencies is the other half: the overall
// total is a second request over the same axis, and it converts through
// ConvertMany rather than through the accounts screen's memo, so it has to be
// pinned in its own right.
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

// TestAccountsIncompletePrewarmCostsTripsNotNumbers pins the property that
// makes the prefetch safe to have at all: it is a cache warm-up, and the
// figures do not depend on it being complete. Whatever the enumeration fails to
// ask for, the per-pair lookup resolves on its own, and the screen comes out
// identical — only dearer.
//
// This is what lets the enumeration be read as an optimization rather than as a
// second, silent statement of which accounts need converting: the day it falls
// behind (a new reason to convert, a new currency) the screen slows down and
// stays right, instead of quietly publishing a number struck from the wrong
// rate or no number at all.
func TestAccountsIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	full, fullBody := accountsScreen(t, 3, nil)
	assertAccountsAreFullyWorked(t, fullBody, 3)
	// The baseline for the comparisons below, and a check of its own: with
	// nothing dropped, nothing should fall back. A currency the enumeration
	// forgot costs one lookup however many accounts hold it, so it stays
	// constant as the screen grows and the growth test above cannot see it.
	// This is where it shows.
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
			partial, partialBody := accountsScreen(t, 3, tc.keep)

			// The two runs are separate databases, so the account ids differ by
			// construction; everything else — every figure, every currency,
			// every date — must match exactly.
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

// blankAccountIDs clears the one field two runs of the same fixture cannot
// agree on, so everything else can be compared as a whole.
func blankAccountIDs(rows []accountListItem) []accountListItem {
	out := make([]accountListItem, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].ID = ""
	}
	return out
}
