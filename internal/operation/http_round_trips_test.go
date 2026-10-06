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
	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/apitest"
)

// journalCost is what one GET of the journal page cost and answered.
//
// trips counts pool acquisitions during the request: one per statement, since a
// journal read holds no transaction. It is measured below the converter because a
// single Rate is one to six statements and a looping batch is one call costing N
// (#45). rate and batch are kept for diagnosis and the fallback assertions.
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

// journalScreen builds an account whose journal grows with size, fetches its
// page once and reports the cost. tune bends the converter double (dropping,
// failingBatch).
func journalScreen(t *testing.T, size int, tune func(*ratetest.Counting)) journalCost {
	t.Helper()
	pool, mdStore := newTestPool(t)
	conv := &ratetest.Counting{Inner: marketdata.NewConverter(mdStore)}
	if tune != nil {
		tune(conv)
	}
	url, c := newAPIOn(t, pool, conv)

	// One USD -> RUB rate before every operation: every date resolves to it,
	// but each distinct date is still asked for, so without the batch a page of
	// many days costs many queries.
	seedFxRate(t, mdStore, "2024-12-31", "90")

	accountID := seedJournal(t, url, c, size)

	// Reset after the fixture is built, so the counts are the GET's alone.
	conv.Reset()
	before := poolTrips(pool)

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations?limit=200", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations = %d, want 200: %s", resp.StatusCode, b)
	}
	cost := journalCost{trips: poolTrips(pool) - before, rate: conv.Singles.Load(), batch: conv.Batches.Load()}
	var page journalPage
	apitest.Decode(t, resp, &page)
	cost.body = page.Operations
	return cost
}

// poolTrips is the pool's lifetime count of acquired connections, so the
// difference across a request counts its round trips at every layer.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// seedJournal fills a receiving account whose page grows with size and returns
// its id. Two kinds of row need rates for different dates:
//
//   - size transfers, each with a breakdown of size pieces bought on days that
//     appear nowhere else on this page (see amountTerms);
//   - size withdrawals, each on its own day.
//
// Transfers live in 2026 and withdrawals in 2025, so a test can drop one kind
// from the prefetch by year.
func seedJournal(t *testing.T, url string, c *http.Client, size int) string {
	t.Helper()
	from := mkAccount(t, url, c, "Источник", "USD")
	to := mkAccount(t, url, c, "Получатель", "USD")

	// A day per operation, so distinct dates grow with the fixture.
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

// dayCounter hands out consecutive days from first.
func dayCounter(t *testing.T, first string) func() string {
	t.Helper()
	day := mustDate(t, first).AddDate(0, 0, -1)
	return func() string {
		day = day.AddDate(0, 0, 1)
		return day.Format("2006-01-02")
	}
}

// mkTransfer moves a parcel and returns the pair, failing on anything but
// 201. The body is whole so a caller may add cost_minor or not.
func mkTransfer(t *testing.T, url string, c *http.Client, body string) transferResp {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)
	return pair
}

// assertJournalIsFullyWorked fails unless every figure was published, so a
// handler that converts nothing cannot pass a performance test.
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

// A journal page costs a fixed number of round trips: twice the rows and more
// than three times the distinct dates (6 against 20) cost the same. Two runs are
// compared rather than one number pinned, because the claim is "the same".
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

// The figures do not depend on the prefetch being complete: whatever it
// misses is resolved per pair, and the page is identical, only dearer.
func TestJournalIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	full := journalScreen(t, 2, nil)
	assertJournalIsFullyWorked(t, full.body)
	// The baseline: nothing dropped, nothing falls back. A forgotten date
	// costs one lookup per date, constant with page size, so only this shows
	// it.
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
		// The dates only the stored breakdown knows.
		{"the purchase dates behind the transfers are missed", func(q marketdata.RateQuery) bool {
			return q.On.Year() == 2025
		}},
		// And the everyday case, dropped instead.
		{"the ordinary rows' own dates are missed", func(q marketdata.RateQuery) bool {
			return q.On.Year() == 2026
		}},
		// Nothing prewarmed.
		{"nothing is prewarmed", func(marketdata.RateQuery) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial := journalScreen(t, 2, ratetest.Dropping(tc.keep))

			// Separate databases, so ids differ; everything else must match.
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

// A batch statement that dies while the database is fine (timeout, array
// encoding) leaves the page identical, paid in round trips, not a 500. An outage
// that takes the fallback down too does fail the request
// (TestListOperationInBaseRealRateErrorFailsRequest).
func TestJournalFailedBatchCostsTripsNotNumbers(t *testing.T) {
	full := journalScreen(t, 2, nil)
	assertJournalIsFullyWorked(t, full.body)

	dead := journalScreen(t, 2, ratetest.FailingBatch(errors.New("statement timeout on the batched fx lookup")))
	assertJournalIsFullyWorked(t, dead.body)

	// Separate databases, so ids differ; everything else must match.
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

// A date the rate table does not reach comes back from the batch carrying
// ErrNoRate, and the memo files it as an answer. The page looks the same either
// way; only the fallback count shows a gap being asked for twice.
func TestJournalGapIsFiledNotAskedAgain(t *testing.T) {
	pool, mdStore := newTestPool(t)
	conv := &ratetest.Counting{Inner: marketdata.NewConverter(mdStore)}
	url, c := newAPIOn(t, pool, conv)

	// Rates start in 2025: the 2025 operation resolves, the 2024 one does
	// not.
	seedFxRate(t, mdStore, "2025-01-01", "90")
	acc := mkAccount(t, url, c, "US брокер", "USD")
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2025-06-01","amount_minor":-10000,"currency":"USD"}`, acc))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2024-06-01","amount_minor":-20000,"currency":"USD"}`, acc))

	conv.Reset()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?limit=200", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations = %d, want 200: %s", resp.StatusCode, b)
	}
	var page journalPage
	apitest.Decode(t, resp, &page)
	rows := page.Operations

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
	if got := conv.Batches.Load(); got != 1 {
		t.Fatalf("the page made %d batched rate resolutions, want exactly 1", got)
	}
	if got := conv.Singles.Load(); got != 0 {
		t.Fatalf("the page fell back to %d one-pair lookups — the batch answered «no rate» for 2024-06-01 and that answer must be filed in the memo, not thrown away and asked for again",
			got)
	}
}

// blankJournalIDs clears the ids, which differ between two runs.
func blankJournalIDs(rows []journalItem) []journalItem {
	out := make([]journalItem, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].ID = ""
	}
	return out
}
