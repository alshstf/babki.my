package portfolio_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// formatMinor renders a *int64 for a failure message; %v would print the
// address.
func formatMinor(p *int64) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatInt(*p, 10)
}

// formatText renders a *string the same way, quoted.
func formatText(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.Quote(*p)
}

// countingInstruments, countingJournal and countingSpaces count a request's
// reads and pass them through.
type countingInstruments struct {
	inner instrumentStoreLike
	calls int
}

func (s *countingInstruments) ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error) {
	s.calls++
	return s.inner.ByIDs(ctx, ids)
}

type countingJournal struct {
	inner journalStoreLike
	calls int
}

func (s *countingJournal) ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]portfolio.Operation, error) {
	s.calls++
	return s.inner.ListForEngine(ctx, spaceID, accountID)
}

type countingSpaces struct {
	inner spaceStoreLike
	calls int
}

func (s *countingSpaces) SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error) {
	s.calls++
	return s.inner.SpaceByID(ctx, id)
}

// screenCost is what one GET of the positions screen cost and answered.
// trips counts pool acquisitions — one per statement, since reads hold no
// transaction — and catches an N+1 hidden inside a store call (as ByIDs once
// was). The per-dependency counts cover the in-memory quote store, which
// never touches the pool.
type screenCost struct {
	spaces      int
	journal     int
	instruments int
	quotes      int
	rate        int
	batch       int
	trips       int64
	body        positionsResp
}

// handlerCalls sums what the handler asked of each dependency.
func (c screenCost) handlerCalls() int {
	return c.spaces + c.journal + c.instruments + c.quotes + c.rate + c.batch
}

func (c screenCost) String() string {
	return fmt.Sprintf("%d db round trips, %d handler calls (space %d, journal %d, instruments %d, quotes %d, one-pair rates %d, batched rates %d)",
		c.trips, c.handlerCalls(), c.spaces, c.journal, c.instruments, c.quotes, c.rate, c.batch)
}

// positionsScreen builds an account of the given size, fetches its positions
// once, and reports the cost. tune bends the converter first.
func positionsScreen(t *testing.T, size int, tune func(*ratetest.Counting)) screenCost {
	t.Helper()
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)

	// One rate row per pair, dated before every operation. EUR is quoted only
	// against RUB, so the bond's EUR->USD rate is a bridge — the costliest path.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := &ratetest.Counting{Inner: marketdata.NewConverter(mdStore)}
	if tune != nil {
		tune(conv)
	}
	var instruments *countingInstruments
	var journal *countingJournal
	var spaces *countingSpaces
	url, c := setupAPI(t, pool, quotes, conv, func(s portfolioStores) portfolioStores {
		instruments = &countingInstruments{inner: s.instruments}
		journal = &countingJournal{inner: s.ops}
		spaces = &countingSpaces{inner: s.spaces}
		return portfolioStores{ops: journal, instruments: instruments, spaces: spaces}
	})

	accountID := seedPositions(t, url, c, quotes, size)

	// Zero the counters so they hold only the GET below.
	conv.Reset()
	instruments.calls, journal.calls, spaces.calls, quotes.calls = 0, 0, 0, 0
	before := poolTrips(pool)

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET positions = %d, want 200", resp.StatusCode)
	}
	cost := screenCost{
		spaces: spaces.calls, journal: journal.calls, instruments: instruments.calls,
		quotes: quotes.calls, rate: int(conv.Singles.Load()), batch: int(conv.Batches.Load()),
	}
	apitest.Decode(t, resp, &cost.body)
	// Read trips after the body is drained, so a handler still working after the
	// headers would be counted.
	cost.trips = poolTrips(pool) - before
	return cost
}

// poolTrips is the pool's lifetime acquisition count; acquisitions equal
// statements while no transaction is open, which holds for every read.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// seedPositions fills a USD account (RUB space) with size shares and size
// bonds, each figure needing its own date or pair: lots on distinct days,
// dividends and partial sales on others, bonds valued in EUR into the
// position's USD, and today's rate for every valuation.
func seedPositions(t *testing.T, url string, c *http.Client, quotes *fakeQuoteStore, size int) string {
	t.Helper()
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)

	// Every operation on its own day, so distinct dates grow with the fixture.
	firstDay := mustDate(t, "2026-02-01")
	days := 0
	nextDay := func() string {
		days++
		return firstDay.AddDate(0, 0, days).Format("2006-01-02")
	}

	for i := range size {
		share := createInstrument(t, c, url, fmt.Sprintf(
			`{"type":"share","name":"Акция %02d","ticker":"ACME%d","currency":"USD"}`, i, i))
		shareID, err := uuid.Parse(share.ID)
		if err != nil {
			t.Fatalf("parse share id: %v", err)
		}
		quotes.byInstrument[shareID] = marketdata.Quote{
			InstrumentID: shareID, On: mustDate(t, "2026-07-21"),
			Price: decimal.RequireFromString("110"), Currency: "USD", Source: "test",
		}
		for range size {
			createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
				"occurred_on":%q,"quantity":"10","price":"100",
				"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, nextDay()))
		}
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
			"occurred_on":%q,"amount_minor":5000,"currency":"USD"}`, acc.ID, share.ID, nextDay()))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":%q,"quantity":"5","price":"120",
			"amount_minor":60000,"currency":"USD"}`, acc.ID, share.ID, nextDay()))

		bond := createInstrument(t, c, url, fmt.Sprintf(
			`{"type":"bond","name":"Облигация %02d","ticker":"BOND%d","currency":"USD","face_value_minor":100000,"face_currency":"EUR"}`, i, i))
		bondID, err := uuid.Parse(bond.ID)
		if err != nil {
			t.Fatalf("parse bond id: %v", err)
		}
		quotes.byInstrument[bondID] = marketdata.Quote{
			InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
			Price: decimal.RequireFromString("95.20"), Currency: "USD", Source: "test",
		}
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"2","price":"950",
			"amount_minor":-190000,"currency":"USD"}`, acc.ID, bond.ID, nextDay()))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon",
			"occurred_on":%q,"amount_minor":1000,"currency":"USD"}`, acc.ID, bond.ID, nextDay()))
	}
	return acc.ID
}

// assertScreenIsFullyWorked fails unless every figure was produced: the
// cheapest screen converts nothing.
func assertScreenIsFullyWorked(t *testing.T, got positionsResp) {
	t.Helper()
	var converted, valued, realized int
	for _, p := range got.Positions {
		if p.MarketValueSourceCurrency != nil && *p.MarketValueSourceCurrency == "EUR" {
			// The bond's valuation was converted into the position's currency.
			converted++
		}
		if p.InBase == nil {
			t.Fatalf("in_base = null for %s: every position here is in USD against an RUB base, with a rate seeded for every date it needs", p.Instrument.Name)
		}
		if p.InBase.MarketValueMinor != nil {
			valued++
		}
		if p.InBase.RealizedPnlMinor != nil && *p.InBase.RealizedPnlMinor != 0 {
			realized++
		}
	}
	if converted == 0 {
		t.Fatalf("no position published market_value_source_currency=EUR: the bond valuations were never converted into the position currency, so the pair with the odd target was never asked for")
	}
	if valued != len(got.Positions) {
		t.Fatalf("in_base.market_value_minor present on %d of %d positions, want all: today's rate values every one of them", valued, len(got.Positions))
	}
	if realized == 0 {
		t.Fatalf("no position published a non-zero in_base.realized_pnl_minor: the disposals' own dates were never resolved")
	}
}

// The positions screen costs the same round trips whatever the account holds:
// four times the positions, lots and dates. Two runs are compared rather than
// a magic number.
func TestPositionsRoundTripsDoNotGrowWithTheData(t *testing.T) {
	small := positionsScreen(t, 1, nil)
	large := positionsScreen(t, 4, nil)

	if len(large.body.Positions) <= len(small.body.Positions) {
		t.Fatalf("large run has %d positions, small run %d — the fixture must actually grow for this test to mean anything",
			len(large.body.Positions), len(small.body.Positions))
	}
	assertScreenIsFullyWorked(t, small.body)
	assertScreenIsFullyWorked(t, large.body)
	t.Logf("%d positions: %s", len(small.body.Positions), small)
	t.Logf("%d positions: %s", len(large.body.Positions), large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the data: %d positions cost %s, %d positions cost %s",
			len(small.body.Positions), small, len(large.body.Positions), large)
	}
}

// A hole in the prefetch costs round trips, never numbers: the page is
// byte-identical.
func TestPositionsIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	full := positionsScreen(t, 2, nil)
	assertScreenIsFullyWorked(t, full.body)
	// With nothing dropped nothing falls back; a missed pair costs one lookup per
	// pair, invisible to the growth test.
	if full.rate != 0 {
		t.Fatalf("the complete prewarm still fell back to %d one-pair lookups: %s — some rate the loop asks for is not among the ones rateQueries enumerates, or is enumerated for a different pair than it is looked up by",
			full.rate, full)
	}

	for _, tc := range []struct {
		name string
		keep func(marketdata.RateQuery) bool
	}{
		// The pair whose target is the position's currency.
		{"the bond valuation pair is missed", func(q marketdata.RateQuery) bool { return q.To == "RUB" }},
		// Every historical date; only today's rate stays prewarmed.
		{"every historical date is missed", func(q marketdata.RateQuery) bool {
			return q.On.Format("2006-01-02") == today
		}},
		// Nothing prewarmed: behave as before the prewarm existed.
		{"nothing is prewarmed", func(marketdata.RateQuery) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial := positionsScreen(t, 2, ratetest.Dropping(tc.keep))

			// Separate databases: only instrument ids differ.
			blankIDs := func(r positionsResp) positionsResp {
				for i := range r.Positions {
					r.Positions[i].Instrument.Id = ""
				}
				return r
			}
			if !reflect.DeepEqual(blankIDs(partial.body), blankIDs(full.body)) {
				t.Fatalf("an incomplete prewarm changed the answer:\n got %+v\nwant %+v", partial.body, full.body)
			}
			if partial.rate <= full.rate {
				t.Fatalf("prewarm dropped queries but nothing fell back: %s (complete prewarm: %s) — the fake is not dropping what it claims to", partial, full)
			}
			if partial.trips <= full.trips {
				t.Fatalf("prewarm dropped queries but the request cost no more: %s (complete prewarm: %s)", partial, full)
			}
		})
	}
}

// A batch that fails alone (#70) gives the identical page at the cost of round
// trips, never an error. An outage fails the request (see
// TestPositionsRealRateErrorFailsRequest).
func TestPositionsFailedBatchCostsTripsNotNumbers(t *testing.T) {
	full := positionsScreen(t, 2, nil)
	assertScreenIsFullyWorked(t, full.body)

	dead := positionsScreen(t, 2, ratetest.FailingBatch(errors.New("statement timeout on the batched fx lookup")))
	assertScreenIsFullyWorked(t, dead.body)

	// Separate databases: only instrument ids differ.
	blankIDs := func(r positionsResp) positionsResp {
		for i := range r.Positions {
			r.Positions[i].Instrument.Id = ""
		}
		return r
	}
	if !reflect.DeepEqual(blankIDs(dead.body), blankIDs(full.body)) {
		t.Fatalf("a failed batch changed the answer:\n got %+v\nwant %+v", dead.body, full.body)
	}
	if dead.rate <= full.rate {
		t.Fatalf("the batch failed but nothing fell back: %s (working batch: %s) — the double is not failing what it claims to", dead, full)
	}
	if dead.trips <= full.trips {
		t.Fatalf("the batch failed but the request cost no more: %s (working batch: %s)", dead, full)
	}
}

// A lot date the rate table does not reach comes back from the batch as
// ErrNoRate and is filed, not asked again; only the fallback count shows it.
func TestPositionsGapIsFiledNotAskedAgain(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	// Rates start at lateRateOn: the early lot has none, the late lot and today
	// do.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := &ratetest.Counting{Inner: marketdata.NewConverter(mdStore)}
	url, c := setupAPI(t, pool, quotes, conv)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	gapped := createInstrument(t, c, url, `{"type":"share","name":"Старая","ticker":"OLD","currency":"USD"}`)
	fine := createInstrument(t, c, url, `{"type":"share","name":"Новая","ticker":"NEW","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, gapped.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, fine.ID, lateBuyOn))

	// Zero the counters so they hold only the GET below.
	conv.Reset()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET positions = %d, want 200", resp.StatusCode)
	}
	var body positionsResp
	apitest.Decode(t, resp, &body)

	var gaps, converted int
	for _, p := range body.Positions {
		if p.InBase == nil {
			gaps++
			continue
		}
		converted++
	}
	if gaps != 1 || converted != 1 {
		t.Fatalf("screen shows %d gap(s) and %d converted positions, want 1 and 1 — the fixture is not exercising a gap beside a working conversion", gaps, converted)
	}
	if conv.Batches.Load() != 1 {
		t.Fatalf("the screen made %d batched rate resolutions, want exactly 1", conv.Batches.Load())
	}
	if conv.Singles.Load() != 0 {
		t.Fatalf("the screen fell back to %d one-pair lookups — the batch answered «no rate» for the %s lot and that answer must be filed in the memo, not thrown away and asked for again",
			conv.Singles.Load(), earlyBuyOn)
	}
}

// The shared memo keeps targets apart: a bond's valuation converts into the
// position's currency, everything else into the base, so "USD, today" is asked
// for two targets on one page.
//
//	USD->RUB 90, EUR->RUB 100, so USD->EUR 0.9
//	bond (EUR position), face 1 000 USD at par: 90 000 EUR
//	  (a colliding memo would give 9 000 000)
//	share (USD), 10 @ 100, quoted 110: 110 000 USD, at 90 in base
//
// The rates form a consistent triangle; this test is about keys, not #39.
func TestPositionsSharedRateMemoKeepsTargetsApart(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)

	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация","ticker":"BONDFX","currency":"EUR","face_value_minor":100000,"face_currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("110"), Currency: "USD", Source: "test",
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"1","price":"800",
		"amount_minor":-80000,"currency":"EUR"}`, acc.ID, bond.ID))

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET positions = %d, want 200", resp.StatusCode)
	}
	var got positionsResp
	apitest.Decode(t, resp, &got)
	byID := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		byID[p.Instrument.Id] = p
	}

	bondPos, ok := byID[bond.ID]
	if !ok {
		t.Fatalf("no position for the bond: %+v", got.Positions)
	}
	if bondPos.MarketValueCurrency == nil || *bondPos.MarketValueCurrency != "EUR" {
		t.Fatalf("bond market_value_currency = %v, want EUR (the position's own currency)", bondPos.MarketValueCurrency)
	}
	if bondPos.MarketValueMinor == nil || *bondPos.MarketValueMinor != 90000 {
		t.Errorf("bond market_value_minor = %s, want 90000 (100000 USD at USD->EUR 0,9) — 9000000 would be the USD->RUB rate answering a USD->EUR question",
			formatMinor(bondPos.MarketValueMinor))
	}
	if got := inBaseMarketValue(t, bondPos); got != 9_000_000 {
		t.Errorf("bond in_base.market_value_minor = %d, want 9000000 (the raw 100000 USD at USD->RUB 90)", got)
	}

	sharePos, ok := byID[share.ID]
	if !ok {
		t.Fatalf("no position for the share: %+v", got.Positions)
	}
	if sharePos.MarketValueMinor == nil || *sharePos.MarketValueMinor != 110000 {
		t.Fatalf("share market_value_minor = %s, want 110000 (already in the position's currency, nothing converted)", formatMinor(sharePos.MarketValueMinor))
	}
	if got := inBaseMarketValue(t, sharePos); got != 9_900_000 {
		t.Errorf("share in_base.market_value_minor = %d, want 9900000 (110000 USD at USD->RUB 90) — 99000 is the USD->EUR rate answering a USD->RUB question, which is what a memo that lost the target currency returns when the bond is valued first",
			got)
	}
}

// inBaseMarketValue reads the base valuation and fails if it is absent.
func inBaseMarketValue(t *testing.T, p positionResp) int64 {
	t.Helper()
	if p.InBase == nil {
		t.Fatalf("%s: in_base = null, want the object", p.Instrument.Name)
	}
	if p.InBase.MarketValueMinor == nil {
		t.Fatalf("%s: in_base.market_value_minor = null, want a figure", p.Instrument.Name)
	}
	return *p.InBase.MarketValueMinor
}

// An instrument the catalog lacks is a 404, not a silently skipped position
// (the foreign key makes it unreachable through the API).
func TestPositionsAbsentInstrumentIsLoud(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)),
		func(s portfolioStores) portfolioStores {
			s.instruments = emptyInstruments{}
			return s
		})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, share.ID))

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET positions whose instrument has no catalog row = %d, want 404 — a position must never be dropped from the list in silence", resp.StatusCode)
	}
}

// emptyInstruments is a catalog that holds nothing.
type emptyInstruments struct{}

func (emptyInstruments) ByIDs(context.Context, []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error) {
	return map[uuid.UUID]instrument.Instrument{}, nil
}
