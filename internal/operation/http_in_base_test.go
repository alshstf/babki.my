package operation_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// operationInBase mirrors apitypes.OperationInBase for decoding in tests.
type operationInBase struct {
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor"`
	Currency    string `json:"currency"`
	RateOn      string `json:"rate_on"`
}

// journalItem is the subset of apitypes.Operation these tests care about. A
// nil *operationInBase covers both an omitted key and an explicit JSON null,
// which is what in_base always is when there is nothing to publish (the
// handler sets it to either a value or an explicit null, never leaves it
// unset).
type journalItem struct {
	ID          string           `json:"id"`
	OccurredOn  string           `json:"occurred_on"`
	AmountMinor int64            `json:"amount_minor"`
	FeeMinor    int64            `json:"fee_minor"`
	Currency    string           `json:"currency"`
	InBase      *operationInBase `json:"in_base"`
	// HasUndatedLots says WHY in_base is null when it is: a rate nobody has
	// fetched yet, or a purchase date nobody ever wrote down. The two must not
	// be explained to a reader with the same sentence (see the API contract).
	HasUndatedLots bool `json:"has_undated_lots"`
	// AssembledFromLots says whether amount_minor was assembled piece by piece
	// from a stored breakdown — a fact about the operation, published here
	// regardless of whether in_base exists at all (#67; see the API contract).
	AssembledFromLots bool `json:"assembled_from_lots"`
}

// listJournal fetches GET .../operations and decodes it, failing the test on
// a non-200 or a decode error.
func listJournal(t *testing.T, url string, c *http.Client, accountID string) []journalItem {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list operations = %d: %s", resp.StatusCode, b)
	}
	var out []journalItem
	decodeJSON(t, resp, &out)
	return out
}

// mkAccount creates an account in currency and returns its id.
func mkAccount(t *testing.T, url string, c *http.Client, name, currency string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/accounts",
		fmt.Sprintf(`{"name":%q,"type":"brokerage","currency":%q}`, name, currency))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %s account = %d: %s", currency, resp.StatusCode, b)
	}
	var a idResp
	decodeJSON(t, resp, &a)
	return a.ID
}

// mkInstrument creates an instrument and returns its id.
func mkInstrument(t *testing.T, url string, c *http.Client, body string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/instruments", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var i idResp
	decodeJSON(t, resp, &i)
	return i.ID
}

// mkOperation creates one operation and returns its id.
func mkOperation(t *testing.T, url string, c *http.Client, body string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create operation = %d: %s", resp.StatusCode, b)
	}
	var o idResp
	decodeJSON(t, resp, &o)
	return o.ID
}

// findOperation picks one operation out of a journal listing by id, so a
// test never depends on the listing's ordering.
func findOperation(t *testing.T, list []journalItem, id string) journalItem {
	t.Helper()
	for _, o := range list {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("operation %s not found in journal of %d items", id, len(list))
	return journalItem{}
}

// mustDate parses a YYYY-MM-DD test fixture date, failing the test on a
// malformed literal rather than silently building a zero time.Time.
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

// seedFxRate stores one USD->RUB rate on the given date.
func seedFxRate(t *testing.T, mdStore *marketdata.Store, on, rate string) {
	t.Helper()
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, on), Rate: decimal.RequireFromString(rate), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate %s@%s: %v", rate, on, err)
	}
}

// TestListOperationInBaseConvertsAtOperationDate is the brief's main case: a
// USD purchase in an RUB-based space, with a rate seeded on exactly the
// operation's own date, must come back with in_base filled in from THAT
// date's rate.
//
// Manual arithmetic (rate 65.4567 RUB per USD, a plausible March 2019 CBR
// quote; the fractional digits are deliberate so both figures actually have
// something to round):
//
//	amount_minor -123_456 (= -1_234.56 USD, a purchase, so negative)
//	  -123_456 * 65.4567 = -8_081_022.3552 -> -8_081_022 (= -80_810.22 RUB)
//	fee_minor 799 (= 7.99 USD)
//	  799 * 65.4567 = 52_299.9033 -> 52_300 (= 522.99... rounds UP to 523.00 RUB)
//
// The fee rounds up while the amount rounds down (in magnitude), which is
// only possible if the two are converted and rounded as independent figures
// — the brief's requirement. rate_on is the rate's own date, 2019-03-12.
//
// A much later rate is seeded as well, so an implementation that converted
// at TODAY's rate (the way an account balance does, and a position's market
// valuation — but not a position's basis, which since plan 6 is historical
// like this) would pick 100 instead of 65.4567 and fail every assertion
// below — the journal's whole point is that it does not.
func TestListOperationInBaseConvertsAtOperationDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-12", "65.4567")
	seedFxRate(t, mdStore, "2025-01-09", "100")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	share := mkInstrument(t, url, c,
		`{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	buy := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2019-03-12","quantity":"10","price":"123.456",
		"amount_minor":-123456,"currency":"USD","fee_minor":799}`, acc, share))

	op := findOperation(t, listJournal(t, url, c, acc), buy)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want a conversion at the 2019-03-12 rate")
	}
	if op.InBase.AmountMinor != -8081022 {
		t.Errorf("in_base.amount_minor = %d, want -8081022 (-123456 * 65.4567, sign preserved)", op.InBase.AmountMinor)
	}
	if op.InBase.FeeMinor != 52300 {
		t.Errorf("in_base.fee_minor = %d, want 52300 (799 * 65.4567, rounded on its own)", op.InBase.FeeMinor)
	}
	if op.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB (the space's base currency)", op.InBase.Currency)
	}
	if op.InBase.RateOn != "2019-03-12" {
		t.Errorf("in_base.rate_on = %q, want 2019-03-12 (the operation's own date, where the rate lives)", op.InBase.RateOn)
	}
}

// TestListOperationInBaseFeeIsNotDerivedFromTheCombinedTotal pins the fee as
// a figure converted in its own right, which the case above cannot do: on its
// fixture, deriving the fee as convert(amount+fee) - convert(amount) happens
// to land on the same number, so that implementation would pass unnoticed.
//
// Manual arithmetic (rate 65.50, chosen so both products land on exactly
// half a minor unit and therefore round away from zero):
//
//	amount_minor 12_345 * 65.50 = 808_597.50 -> 808_598
//	fee_minor       799 * 65.50 =  52_334.50 ->  52_335
//	combined     13_144 * 65.50 = 860_932.00 -> 860_932  (nothing to round)
//
// Converted independently the fee is 52_335. Derived from the combined
// total it is 860_932 - 808_598 = 52_334 — one minor unit adrift, because
// the two half-unit remainders that each round up on their own are gone by
// the time the sum is rounded once. A dividend (positive amount) is used so
// both figures share a sign and the divergence is not masked by the way
// half-away-from-zero treats a negative amount against a positive fee.
func TestListOperationInBaseFeeIsNotDerivedFromTheCombinedTotal(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-12", "65.50")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	share := mkInstrument(t, url, c,
		`{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	div := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2019-03-12","amount_minor":12345,"currency":"USD","fee_minor":799}`, acc, share))

	op := findOperation(t, listJournal(t, url, c, acc), div)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want a conversion at the 2019-03-12 rate")
	}
	if op.InBase.AmountMinor != 808598 {
		t.Errorf("in_base.amount_minor = %d, want 808598 (12345 * 65.50)", op.InBase.AmountMinor)
	}
	if op.InBase.FeeMinor != 52335 {
		t.Errorf("in_base.fee_minor = %d, want 52335 (799 * 65.50 rounded on its own; "+
			"52334 means the fee was derived from the combined total)", op.InBase.FeeMinor)
	}
}

// TestListOperationInBaseUsesNearestEarlierRateDate is what separates the
// journal from balances and positions: the CBR publishes nothing on
// weekends and holidays, so an operation dated 2019-03-17 (a Sunday) has no
// rate of its own. Store.FxRateOn then resolves the nearest EARLIER date —
// Friday 2019-03-15 — and rate_on must report that date, not occurred_on.
// The next task's tooltip reads exactly this field, so publishing
// occurred_on here would claim a rate that never existed.
//
// Manual arithmetic: -10_000 (= -100.00 USD) * 65 = -650_000 (= -6_500.00 RUB).
func TestListOperationInBaseUsesNearestEarlierRateDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Friday's rate only. Nothing on the Saturday/Sunday that follow — but a
	// much later rate exists, so "nearest earlier than the OPERATION" and
	// "nearest earlier than TODAY" resolve to visibly different rows.
	seedFxRate(t, mdStore, "2019-03-15", "65")
	seedFxRate(t, mdStore, "2025-01-09", "100")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-17","amount_minor":-10000,"currency":"USD"}`, acc))

	op := findOperation(t, listJournal(t, url, c, acc), wd)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want the nearest earlier (2019-03-15) rate to be used")
	}
	if op.InBase.RateOn != "2019-03-15" {
		t.Errorf("in_base.rate_on = %q, want 2019-03-15 — the date of the rate ACTUALLY used, not occurred_on (2019-03-17)", op.InBase.RateOn)
	}
	if op.InBase.AmountMinor != -650000 {
		t.Errorf("in_base.amount_minor = %d, want -650000 (-10000 * 65)", op.InBase.AmountMinor)
	}
}

// TestListOperationInBaseNullWhenBaseCurrency covers: an operation already
// denominated in the space's base currency (RUB, the setup default) has
// nothing to convert, so in_base must be null — not a copy of the operation
// at rate 1, which would imply an fx conversion that never happened and
// would need a rate_on date it does not have.
func TestListOperationInBaseNullWhenBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, so a null here can only come from the base-currency
	// short-circuit, never from a failed lookup.
	seedFxRate(t, mdStore, "2019-03-12", "65")

	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"RUB","fee_minor":100}`, acc))

	op := findOperation(t, listJournal(t, url, c, acc), wd)
	if op.InBase != nil {
		t.Fatalf("in_base = %+v, want null (operation already in the base currency)", op.InBase)
	}
}

// TestListOperationInBaseNullWhenNoRateOnOrBeforeDate covers the honest
// "can't convert" case: the only seeded rate is LATER than the operation, so
// FxRateOn's nearest-earlier-date lookup finds nothing at all. in_base must
// be null in its entirety — never partially filled, and never back-filled
// from a future rate, which would answer "what would this cost today" to a
// question about 2019 — and the request must still succeed with 200, since a
// currency the rate history doesn't reach back far enough for is expected,
// not an error.
func TestListOperationInBaseNullWhenNoRateOnOrBeforeDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Seeded AFTER the operation's date: nothing on or before 2019-03-12.
	seedFxRate(t, mdStore, "2020-01-10", "65")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))

	list := listJournal(t, url, c, acc)
	op := findOperation(t, list, wd)
	if op.InBase != nil {
		t.Fatalf("in_base = %+v, want null (no USD->RUB rate on or before 2019-03-12)", op.InBase)
	}
}

// converterLike mirrors the operation package's unexported converter
// interface so this external test package can name the type of the double it
// passes to operation.NewHandler. Assignability is by method set, not by
// name, so anything satisfying this satisfies the handler's own interface —
// the same trick account's and portfolio's http_test.go use.
type converterLike interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// failingConverter is a converter double whose fx lookups always fail with
// the given error — deliberately NOT marketdata.ErrNoRate, standing in for a
// genuine outage (a dropped DB connection, a canceled context) rather than
// the ordinary "this pair has no rate on this date" outcome. A real,
// Postgres-backed *marketdata.Converter can't be made to fail on demand, so
// the distinction operationInBase draws between those two cases is only
// testable through a double.
type failingConverter struct{ err error }

func (c failingConverter) Rate(_ context.Context, _, _ string, _ time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Decimal{}, time.Time{}, c.err
}

func (c failingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratesFromRate(ctx, c, queries)
}

// rateResolver is the one-pair half of converterLike, which is all
// ratesFromRate needs of a double.
type rateResolver interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// ratesFromRate answers a whole batch by asking the double's OWN Rate once per
// query. It exists so no test double can answer the batch differently from the
// pair: marketdata.Converter guarantees the two agree ("no number it produces
// differs from Rate's" — see RatesOn), and a fake that had to state its
// behavior twice would eventually state it twice differently, leaving a test
// that pins a rule the production converter does not follow. Here the batch is
// derived from the pair, so whatever a test makes Rate say — a rate, a missing
// rate, an outage — the prewarm says exactly the same.
//
// The two kinds of failure are sorted the way RatesOn sorts them:
// marketdata.ErrNoRate is that one query's answer and the rest of the page
// stands, anything else voids the whole batch.
//
// It is a deliberate twin of portfolio's identically named test helper rather
// than something shared with it: both are glue for a package's own doubles,
// and a test fixture shared between two packages' fakes would tie each
// package's next fake to the other package's needs. There is no production
// code here to drift.
func ratesFromRate(ctx context.Context, r rateResolver, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	out := make(map[marketdata.RateQuery]marketdata.RateResult, len(queries))
	for _, q := range queries {
		rate, on, err := r.Rate(ctx, q.From, q.To, q.On)
		switch {
		case err == nil:
			out[q] = marketdata.RateResult{Rate: rate, RateDate: on}
		case errors.Is(err, marketdata.ErrNoRate):
			out[q] = marketdata.RateResult{Err: err}
		default:
			return marketdata.Rates{}, err
		}
	}
	return marketdata.NewRates(out), nil
}

// TestListOperationInBaseRealRateErrorFailsRequest pins the distinction the
// whole in_base contract rests on: a genuine failure while resolving the fx
// rate (DB down, context canceled) must fail the request, NOT be rendered as
// in_base: null.
//
// Both outcomes look identical on screen otherwise — the journal shows the
// operation's native amount with a "no rate" marker — so an outage would be
// presented to the user as ordinary, expected degradation and nobody would
// ever learn the database had stopped answering. Only the status code tells
// them apart, which is why this test asserts on it: without it, deleting
// operationInBase's error propagation (returning null instead of the error)
// breaks nothing visible and no other test notices. Mirrors
// account.TestListRealRateErrorFailsRequest and
// portfolio.TestPositionsRealRateErrorFailsRequest.
func TestListOperationInBaseRealRateErrorFailsRequest(t *testing.T) {
	url, c := newAPIWithConverterDouble(t, failingConverter{err: errors.New("connection reset by peer")})

	// USD operation in an RUB-based space: the base-currency short-circuit is
	// avoided, so the rate lookup really is attempted.
	acc := mkAccount(t, url, c, "US брокер", "USD")
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations with a failing rate lookup = %d, want 500 — a real outage must not be served as a 200 with in_base: null: %s",
			resp.StatusCode, b)
	}
}

// countingConverter wraps a real *marketdata.Converter and counts what one
// request ASKS of the fx layer. It is a counter, not a stub: every call is
// delegated, so the amounts the handler publishes are the real ones and the
// memoization assertion can sit next to correctness assertions in the same
// test. The counts are atomic because the increments happen on the
// http.Server's handler goroutine while the assertions read them on the test's
// own goroutine.
//
// NONE OF THESE IS A COUNT OF DATABASE ROUND TRIPS, and reading them as one is
// the defect issue #45 names: a single Rate is between one and six statements
// depending on whether the pair resolves directly, by inversion or through a
// RUB bridge, and a batch is one statement however many queries it carries —
// unless it loops internally, which from up here looks identical. What the
// page costs the database is measured below the converter instead, by
// journalCost.trips (see http_round_trips_test.go). What these say is what the
// handler asked for, which is the right question when the memo key or the
// enumeration is what is under test.
//
// keep, when set, filters the batch down to the queries it accepts before
// passing them on — an enumeration with a hole in it, which is what
// TestJournalIncompletePrewarmCostsTripsNotNumbers needs and no real converter
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
	// rate counts one-pair lookups (the fallback), batch counts calls to the
	// batched resolution, and queries counts the individual rates handed to
	// those batches — as the handler asked for them, before keep drops any.
	rate    atomic.Int64
	batch   atomic.Int64
	queries atomic.Int64
}

func (c *countingConverter) Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error) {
	c.rate.Add(1)
	return c.inner.Rate(ctx, from, to, on)
}

func (c *countingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	c.batch.Add(1)
	c.queries.Add(int64(len(queries)))
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

// TestListOperationInBaseMemoizesRatePerCurrencyAndDate pins the cache key.
// A journal page mixes many dates for the same currency — unlike the account
// list, which converts everything at today's rate and can therefore key its
// cache by currency alone. (The position list mixes dates too, since plan 6:
// each lot and each income payment is valued at its own date, so
// portfolio.rateKey carries the date for exactly the reason spelled out
// here.) Keying this cache by
// currency only would silently reuse the first operation's rate for every
// later date, which is invisible in the response shape: the numbers would
// simply be wrong, in the same currency, with a plausible rate_on.
//
// So the test asserts both halves at once:
//
//   - two USD operations on DIFFERENT dates get DIFFERENT rates (60 vs 70)
//     and different rate_on values — this is what a currency-only key breaks;
//   - two USD operations on the SAME date cost exactly one rate lookup, so
//     three operations across two distinct dates resolve two rates, not three
//     — this is what dropping the cache entirely breaks.
//
// Manual arithmetic:
//
//	2019-03-12 @ 60: -10_000 * 60 = -600_000 ; -20_000 * 60 = -1_200_000
//	2019-04-12 @ 70: -10_000 * 70 =  -700_000
//
// The two -10_000 operations differ only by date, so their converted amounts
// (-600_000 vs -700_000) can only both be right if the date is part of the key.
func TestListOperationInBaseMemoizesRatePerCurrencyAndDate(t *testing.T) {
	pool, mdStore := newTestPool(t)
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore)}
	url, c := newAPIOn(t, pool, conv)

	seedFxRate(t, mdStore, "2019-03-12", "60")
	seedFxRate(t, mdStore, "2019-04-12", "70")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	marchA := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))
	marchB := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-20000,"currency":"USD"}`, acc))
	april := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-04-12","amount_minor":-10000,"currency":"USD"}`, acc))

	// Only the listing below is under measurement.
	conv.rate.Store(0)
	conv.batch.Store(0)
	conv.queries.Store(0)
	list := listJournal(t, url, c, acc)

	for _, tc := range []struct {
		name       string
		id         string
		wantMinor  int64
		wantRateOn string
	}{
		{"march A", marchA, -600000, "2019-03-12"},
		{"march B", marchB, -1200000, "2019-03-12"},
		{"april", april, -700000, "2019-04-12"},
	} {
		op := findOperation(t, list, tc.id)
		if op.InBase == nil {
			t.Fatalf("%s in_base = null, want a conversion", tc.name)
		}
		if op.InBase.AmountMinor != tc.wantMinor {
			t.Errorf("%s in_base.amount_minor = %d, want %d — each date must use its own rate",
				tc.name, op.InBase.AmountMinor, tc.wantMinor)
		}
		if op.InBase.RateOn != tc.wantRateOn {
			t.Errorf("%s in_base.rate_on = %q, want %q", tc.name, op.InBase.RateOn, tc.wantRateOn)
		}
	}

	// One rate per distinct (currency, date) — the two 2019-03-12 operations
	// share a single one — asked for in a single batch, with nothing left over
	// for the per-pair fallback to resolve.
	//
	// This used to count calls to Converter.Rate and nothing else, which is
	// what issue #45 objects to: it pinned how often the handler asked, never
	// what the asking cost, so an implementation resolving one rate per
	// statement passed it unchanged. The cost is now pinned where it can be
	// seen, below the converter, by TestJournalRoundTripsDoNotGrowWithTheData;
	// what remains here is the question this test is actually about — which
	// distinct rates a page of three operations on two dates asks for.
	if got := conv.queries.Load(); got != 2 {
		t.Errorf("rates asked for = %d, want 2 — one per distinct (currency, date), so the two 2019-03-12 operations share a single one", got)
	}
	if got := conv.batch.Load(); got != 1 {
		t.Errorf("batched resolutions = %d, want 1 — the whole page's rates are resolved in one go", got)
	}
	if got := conv.rate.Load(); got != 0 {
		t.Errorf("one-pair fallback lookups = %d, want 0 — every date this page needs was enumerated and prewarmed", got)
	}
}
