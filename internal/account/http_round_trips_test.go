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
//
// batchErr, when set, fails the batch and only the batch: every one-pair lookup
// still answers from the real converter. That is the failure #70 is about — a
// timeout on the one large statement, an array-encoding problem — as opposed to
// an outage, which takes the fallback down with it and is what failingConverter
// stands in for.
type countingConverter struct {
	inner    *marketdata.Converter
	keep     func(marketdata.RateQuery) bool
	batchErr error
	rate     atomic.Int64
	batch    atomic.Int64
}

// dropping and failingBatch are the two ways the screens below bend the
// converter double: an enumeration with a hole in it, and a batch statement that
// dies on its own. Both are passed as a tune rather than set on the fixture's
// own line, so a screen helper takes one parameter however many failure modes
// the double grows.
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
		// The zero Rates alongside the error, which is what
		// marketdata.RatesOn itself returns on a failure and what the handler
		// must be able to survive being handed.
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

// newAPIOnPool wires the family and account modules onto pool with conv as the
// account handler's fx converter, and returns the server URL and a logged-in
// client. It differs from newAPIWithConverter by taking the pool rather than
// making its own, which is what lets a round-trip test count acquisitions on
// the very pool the handler uses while the converter it passed in reports what
// fell back.
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
			partial, partialBody := accountsScreen(t, 3, dropping(tc.keep))

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

// TestAccountsFailedBatchCostsTripsNotNumbers is the neighbouring property, and
// the one the shared walk must not quietly lose: the batch STATEMENT dies —
// timed out, or refused for how its array argument was encoded — while the
// database is otherwise perfectly well, so every one-pair lookup still answers.
//
// The screen must come out identical to the one a working batch produces, paid
// for with the round trips the batch was there to save. What must NOT happen is
// an error page: the failure is the optimization's, not the answer's, and a
// handler that surfaced it would turn every request the fallback could serve
// correctly into a 500. (An outage that takes the fallback down too is the
// opposite case and does fail the request — see TestListRealRateErrorFailsRequest.)
func TestAccountsFailedBatchCostsTripsNotNumbers(t *testing.T) {
	full, fullBody := accountsScreen(t, 3, nil)
	assertAccountsAreFullyWorked(t, fullBody, 3)

	dead, deadBody := accountsScreen(t, 3, failingBatch(errors.New("statement timeout on the batched fx lookup")))
	assertAccountsAreFullyWorked(t, deadBody, 3)

	// The two runs are separate databases, so the account ids differ by
	// construction; everything else — every figure, every currency, every date
	// — must match exactly.
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

// TestAccountsGapIsFiledNotAskedAgain pins the other half of what the walk hands
// back. A currency the rate provider does not cover comes out of the batch as a
// resolved query carrying marketdata.ErrNoRate — an honest answer, not a miss —
// and the memo must take it as one.
//
// Nothing on screen can tell the difference: filed or not, balance_in_base is
// null for that account either way, because the per-pair fallback would ask the
// store and be told the same thing. The only trace is the cost, which is why
// this test asserts on the fallback count and not only on the payload. Without
// it, a walk that dropped every entry carrying an error would leave every figure
// right and every gap paying for a second lookup, forever, with nothing to show
// for it.
func TestAccountsGapIsFiledNotAskedAgain(t *testing.T) {
	// Two currencies with rates, plus one the fixture deliberately leaves
	// unseeded — screenCurrencies beyond the first two are not given rate rows.
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
