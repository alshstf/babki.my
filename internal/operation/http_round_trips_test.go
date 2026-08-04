package operation_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/marketdata"
)

// journalCost is what one GET of the journal page cost and what it answered.
//
// trips is the figure that matters, and the one issue #45 says was pinned by
// nothing: connections ACQUIRED FROM THE POOL during the request — one per
// statement it sends, since nothing on this path holds a connection across
// several (no transaction is opened while a journal page is read). It is
// measured BELOW the converter on purpose. A counter on Converter.Rate — what
// TestListOperationInBaseMemoizesRatePerCurrencyAndDate had, and all it had —
// counts how often the handler ASKED for a rate, not what the asking cost: one
// Rate is between one and six statements depending on whether the pair
// resolves directly, by inversion, or through a RUB bridge, and a batch that
// looped FxRateOn internally would count as one call while costing one
// statement per query. Both of those are exactly the N+1 this test exists to
// catch, and neither is visible from above the converter.
//
// rate and batch are kept alongside for diagnosis and for the fallback
// assertions below (a prefetch that misses shows up as one-pair lookups), never
// as the cost itself.
type journalCost struct {
	trips int64
	rate  int64
	batch int64
	body  []journalItem
}

func (c journalCost) String() string {
	return fmt.Sprintf("%d database round trips (one-pair rate lookups %d, batched rate resolutions %d, rows %d)",
		c.trips, c.rate, c.batch, len(c.body))
}

// journalScreen builds an account whose journal grows with size (see
// seedJournal), fetches its page once, and reports what that one request cost.
//
// tune, when non-nil, bends the converter double before the page is fetched:
// dropping() gives the prefetch a hole in it, failingBatch() kills the batch
// statement outright. Both let a test check what a degraded prefetch costs and —
// more importantly — what it does NOT change.
func journalScreen(t *testing.T, size int, tune func(*countingConverter)) journalCost {
	t.Helper()
	pool, mdStore := newTestPool(t)
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore)}
	if tune != nil {
		tune(conv)
	}
	url, c := newAPIOn(t, pool, conv)

	// One USD -> RUB row, dated before every operation in the fixture:
	// Store.FxRateOn resolves the nearest EARLIER date, so every date the page
	// asks about resolves to it. The rates still have to be asked for once per
	// distinct date — the lookup is keyed by the date requested, not by the
	// date answered — so a page spanning many days costs many queries without
	// the batch, which is the whole point of the fixture.
	seedFxRate(t, mdStore, "2024-12-31", "90")

	accountID := seedJournal(t, url, c, size)

	// The fixture is built through the same HTTP stack, so the counters are
	// read fresh here: what they hold afterwards can only be the one GET below.
	conv.rate.Store(0)
	conv.batch.Store(0)
	conv.queries.Store(0)
	before := poolTrips(pool)

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations?limit=200", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations = %d, want 200: %s", resp.StatusCode, b)
	}
	cost := journalCost{trips: poolTrips(pool) - before, rate: conv.rate.Load(), batch: conv.batch.Load()}
	decodeJSON(t, resp, &cost.body)
	return cost
}

// dropping and failingBatch are the two ways journalScreen bends the converter
// double: an enumeration with a hole in it, and a batch statement that dies on
// its own. Both are passed as a tune rather than set on the fixture's own line,
// so the helper takes one parameter however many failure modes the double grows.
func dropping(pred func(marketdata.RateQuery) bool) func(*countingConverter) {
	return func(c *countingConverter) { c.keep = pred }
}

func failingBatch(err error) func(*countingConverter) {
	return func(c *countingConverter) { c.batchErr = err }
}

// poolTrips reads the pool's lifetime count of acquired connections. Every
// statement the handler sends takes one, so the difference across a request is
// how many round trips that request made — the technique
// marketdata.TestFxRatesOnBatch uses on the store directly, applied here to a
// whole HTTP request so that a round trip made anywhere beneath the handler is
// counted, whichever layer decided to make it.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// seedJournal fills a receiving account with a journal whose page grows with
// size, and returns that account's id.
//
// Both kinds of row on the page need fx rates, and they need them for
// DIFFERENT dates, which is what makes an enumeration that covers only one
// kind visible as a cost:
//
//   - size transfers arrive, each carrying a FIFO breakdown of size pieces
//     bought on size days of their own. Those purchase dates appear NOWHERE
//     else on this account's page — the shares were bought on the other
//     account — so the only way to know the page needs them is to read the
//     breakdown (see amountTerms);
//   - size withdrawals, each on a day of its own, which is the ordinary case:
//     the amount is money that moved on the day the row is dated.
//
// The two live in different YEARS (purchases and transfers in 2026,
// withdrawals in 2025) so a test can drop one kind from the prefetch and keep
// the other with a predicate over the query alone.
func seedJournal(t *testing.T, url string, c *http.Client, size int) string {
	t.Helper()
	from := mkAccount(t, url, c, "Источник", "USD")
	to := mkAccount(t, url, c, "Получатель", "USD")

	// Every operation gets a day of its own, so the number of distinct dates
	// the page must resolve grows with the fixture instead of collapsing onto
	// one rate that would hide an N+1 behind the memo.
	tradingDay, cashDay := dayCounter(t, "2026-01-02"), dayCounter(t, "2025-01-02")

	for i := range size {
		share := mkInstrument(t, url, c, fmt.Sprintf(
			`{"type":"share","name":"Акция %02d","ticker":"ACME%d","currency":"USD"}`, i, i))
		for range size {
			mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
				"occurred_on":%q,"quantity":"10","price":"100",
				"amount_minor":-100000,"currency":"USD"}`, from, share, tradingDay()))
		}
		mkTransfer(t, url, c, fmt.Sprintf(
			`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"%d","occurred_on":%q}`,
			from, to, share, 10*size, tradingDay()))
		mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
			"occurred_on":%q,"amount_minor":-10000,"currency":"USD"}`, to, cashDay()))
	}
	return to
}

// dayCounter returns a function handing out consecutive days from first, one
// per call, formatted the way the API takes them.
func dayCounter(t *testing.T, first string) func() string {
	t.Helper()
	day := mustDate(t, first).AddDate(0, 0, -1)
	return func() string {
		day = day.AddDate(0, 0, 1)
		return day.Format("2006-01-02")
	}
}

// mkTransfer moves a parcel between two accounts, failing the test on anything
// but a 201, and hands back the created pair. The body is passed whole so a
// caller can add cost_minor (a basis typed in by hand) or leave it out (a FIFO
// carryover from the source account's own lots) without a second helper. Tests
// that only need the transfer to exist ignore the result.
func mkTransfer(t *testing.T, url string, c *http.Client, body string) transferResp {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)
	return pair
}

// assertJournalIsFullyWorked fails unless the page really published every
// figure the round trips were supposed to buy. Without it the count assertions
// would be satisfied by a handler that converts nothing at all — the cheapest
// page is the one that answers nothing, and it must not be able to pass a
// performance test.
func assertJournalIsFullyWorked(t *testing.T, rows []journalItem) {
	t.Helper()
	var assembled, plain int
	for _, r := range rows {
		if r.InBase == nil {
			t.Fatalf("in_base = null on the row dated %s: every row here is in USD against an RUB base, with a rate seeded before every date the page needs",
				r.OccurredOn)
		}
		if r.AssembledFromLots {
			assembled++
			continue
		}
		plain++
	}
	if assembled == 0 {
		t.Fatalf("no row was assembled from a stored breakdown: the purchase dates behind the transfers were never converted, so the dates only amountTerms knows about were never asked for")
	}
	if plain == 0 {
		t.Fatalf("no ordinary row on the page: the everyday case — an amount valued on the day its own row is dated — was never converted")
	}
}

// TestJournalRoundTripsDoNotGrowWithTheData is the requirement this change
// exists for: rendering a page of the journal costs a FIXED number of round
// trips, whatever the page holds. Twice the rows, and more than three times
// the distinct dates behind them (6 against 20, since the purchase dates in a
// transfer's breakdown grow with the size of the fixture as well as with the
// number of transfers) — and the same number of trips to the database.
//
// It compares two runs of different size rather than asserting one magic
// number, deliberately. A magic number would bake in today's incidental
// statements — the session, the space, the journal itself, the breakdowns —
// and the next person to add or remove one would edit the expectation and
// never learn whether the thing this test guards still holds. What must be
// true is not "six", it is "the same".
func TestJournalRoundTripsDoNotGrowWithTheData(t *testing.T) {
	small := journalScreen(t, 2, nil)
	large := journalScreen(t, 4, nil)

	if len(large.body) <= len(small.body) {
		t.Fatalf("large run has %d rows, small run %d — the fixture must actually grow for this test to mean anything",
			len(large.body), len(small.body))
	}
	assertJournalIsFullyWorked(t, small.body)
	assertJournalIsFullyWorked(t, large.body)
	t.Logf("%d rows: %s", len(small.body), small)
	t.Logf("%d rows: %s", len(large.body), large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the data: %d rows cost %s, %d rows cost %s",
			len(small.body), small, len(large.body), large)
	}
}

// TestJournalIncompletePrewarmCostsTripsNotNumbers pins the property that
// makes the prefetch safe to have at all: it is a cache warm-up, and the
// figures do not depend on it being complete. Whatever the enumeration fails to
// ask for, the per-pair lookup resolves on its own, and the page comes out
// byte-identical — only dearer.
//
// This is what lets the enumeration be read as an optimization rather than as
// a second, silent statement of the conversion rules: the day it falls behind
// them (a new kind of row, a new date, a new currency pair) the page slows down
// and stays right, instead of quietly publishing a number struck from the wrong
// rate or no number at all.
func TestJournalIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	full := journalScreen(t, 2, nil)
	assertJournalIsFullyWorked(t, full.body)
	// The baseline for the comparisons below, and a check of its own: with
	// nothing dropped, nothing should fall back. A date the enumeration forgot
	// costs one lookup per distinct DATE rather than one per row, so it stays
	// constant as the page grows and the growth test above cannot see it. This
	// is where it shows.
	if full.rate != 0 {
		t.Fatalf("the complete prewarm still fell back to %d one-pair lookups: %s — some rate the loop asks for is not among the ones rateQueries enumerates, or is enumerated under a different key than it is looked up by",
			full.rate, full)
	}
	if full.batch != 1 {
		t.Fatalf("the page made %d batched rate resolutions, want exactly 1: %s", full.batch, full)
	}

	for _, tc := range []struct {
		name string
		keep func(marketdata.RateQuery) bool
	}{
		// The dates only the stored breakdown knows about — the ones an
		// enumeration written from the rows' own occurred_on would forget.
		{"the purchase dates behind the transfers are missed", func(q marketdata.RateQuery) bool {
			return q.On.Year() == 2025
		}},
		// And the everyday case, dropped instead.
		{"the ordinary rows' own dates are missed", func(q marketdata.RateQuery) bool {
			return q.On.Year() == 2026
		}},
		// Nothing at all is prewarmed: the handler must behave exactly as it
		// did before the prewarm existed.
		{"nothing is prewarmed", func(marketdata.RateQuery) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial := journalScreen(t, 2, dropping(tc.keep))

			// The two runs are separate databases, so the operation ids differ
			// by construction; everything else — every figure, every currency,
			// every date — must match exactly.
			if !reflect.DeepEqual(blankJournalIDs(partial.body), blankJournalIDs(full.body)) {
				t.Fatalf("an incomplete prewarm changed the answer:\n got %+v\nwant %+v", partial.body, full.body)
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

// TestJournalFailedBatchCostsTripsNotNumbers is the neighbouring property, and
// the one the shared walk must not quietly lose: the batch STATEMENT dies —
// timed out, or refused for how its array argument was encoded — while the
// database is otherwise perfectly well, so every one-pair lookup still answers.
//
// The page must come out identical to the one a working batch produces, paid for
// with the round trips the batch was there to save. What must NOT happen is an
// error page: the failure is the optimization's, not the answer's, and a handler
// that surfaced it would turn every request the fallback could serve correctly
// into a 500. (An outage that takes the fallback down too is the opposite case
// and does fail the request — see TestListOperationInBaseRealRateErrorFailsRequest.)
func TestJournalFailedBatchCostsTripsNotNumbers(t *testing.T) {
	full := journalScreen(t, 2, nil)
	assertJournalIsFullyWorked(t, full.body)

	dead := journalScreen(t, 2, failingBatch(errors.New("statement timeout on the batched fx lookup")))
	assertJournalIsFullyWorked(t, dead.body)

	// The two runs are separate databases, so the operation ids differ by
	// construction; everything else — every figure, every currency, every date
	// — must match exactly.
	if !reflect.DeepEqual(blankJournalIDs(dead.body), blankJournalIDs(full.body)) {
		t.Fatalf("a failed batch changed the answer:\n got %+v\nwant %+v", dead.body, full.body)
	}
	if dead.rate <= full.rate {
		t.Fatalf("the batch failed but nothing fell back: %s (working batch: %s) — the double is not failing what it claims to",
			dead, full)
	}
	if dead.trips <= full.trips {
		t.Fatalf("the batch failed but the request cost no more: %s (working batch: %s)", dead, full)
	}
}

// TestJournalGapIsFiledNotAskedAgain pins the other half of what the walk hands
// back. A date the rate table does not reach back to comes out of the batch as a
// resolved query carrying marketdata.ErrNoRate — an honest answer, not a miss —
// and the memo must take it as one.
//
// Nothing on the page can tell the difference: filed or not, in_base is null for
// that row either way, because the per-pair fallback would ask the store and be
// told the same thing. The only trace is the cost, which is why this test
// asserts on the fallback count and not only on the payload. Without it, a walk
// that dropped every entry carrying an error would leave every figure right and
// every gap paying for a second lookup, forever, with nothing to show for it.
func TestJournalGapIsFiledNotAskedAgain(t *testing.T) {
	pool, mdStore := newTestPool(t)
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore)}
	url, c := newAPIOn(t, pool, conv)

	// The rate table starts in 2025, so the 2025 operation resolves and the
	// 2024 one has nothing on or before its date.
	seedFxRate(t, mdStore, "2025-01-01", "90")
	acc := mkAccount(t, url, c, "US брокер", "USD")
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2025-06-01","amount_minor":-10000,"currency":"USD"}`, acc))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2024-06-01","amount_minor":-20000,"currency":"USD"}`, acc))

	conv.rate.Store(0)
	conv.batch.Store(0)
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?limit=200", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations = %d, want 200: %s", resp.StatusCode, b)
	}
	var rows []journalItem
	decodeJSON(t, resp, &rows)

	var gaps, converted int
	for _, r := range rows {
		if r.InBase == nil {
			gaps++
			continue
		}
		converted++
	}
	if gaps != 1 || converted != 1 {
		t.Fatalf("page shows %d gap(s) and %d converted rows, want 1 and 1 — the fixture is not exercising a gap beside a working conversion", gaps, converted)
	}
	if got := conv.batch.Load(); got != 1 {
		t.Fatalf("the page made %d batched rate resolutions, want exactly 1", got)
	}
	if got := conv.rate.Load(); got != 0 {
		t.Fatalf("the page fell back to %d one-pair lookups — the batch answered «no rate» for 2024-06-01 and that answer must be filed in the memo, not thrown away and asked for again",
			got)
	}
}

// blankJournalIDs clears the one field two runs of the same fixture cannot
// agree on, so everything else can be compared as a whole.
func blankJournalIDs(rows []journalItem) []journalItem {
	out := make([]journalItem, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].ID = ""
	}
	return out
}
