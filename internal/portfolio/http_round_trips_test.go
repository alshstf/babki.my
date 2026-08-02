package portfolio_test

import (
	"context"
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
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// formatMinor renders a *int64 API field for a failure message: the decimal
// value when present, "<nil>" when not. Passing the pointer itself to %v
// would print its address once it is non-nil — int64 is not one of the
// compound types fmt's default verb dereferences — so a genuine failure would
// read as "market_value_minor = 0x743c2bdf50b8" instead of the number the
// message is trying to show.
func formatMinor(p *int64) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatInt(*p, 10)
}

// formatText is formatMinor's twin for a *string API field — a currency code,
// a date — and exists for the identical reason: %v on a non-nil *string prints
// its address, so a genuine failure reads as "rate_on = 0x743c2bdf50b8"
// instead of the date the message is trying to show. Quoted rather than bare,
// so an empty string is visible as one and cannot be mistaken for the "<nil>"
// beside it.
func formatText(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.Quote(*p)
}

// countingConverter counts what one screen asks of the fx layer while
// delegating every answer to a real *marketdata.Converter, so the figures on
// the page stay the production ones and only their cost is observed.
//
// The two counters are kept apart because they cost different amounts: RatesOn
// is exactly one query however many pairs it resolves (see its doc), while
// each Rate is up to six — a direct row, its inverse, and both legs of a RUB
// bridge. Counting a Rate as one therefore UNDERSTATES the cost of falling
// back, which is the safe direction for a test asserting that the total does
// not grow.
//
// keep, when set, filters the batch down to the queries it accepts before
// passing them on — an enumeration with a hole in it, which is what
// TestPositionsIncompletePrewarmCostsTripsNotNumbers needs and no real
// converter would ever do.
type countingConverter struct {
	inner *marketdata.Converter
	keep  func(marketdata.RateQuery) bool
	rate  int
	batch int
}

func (c *countingConverter) Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error) {
	c.rate++
	return c.inner.Rate(ctx, from, to, on)
}

func (c *countingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	c.batch++
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

// countingInstruments, countingJournal and countingSpaces count the reads a
// single positions request makes of the catalog, the journal and the space —
// each one a round trip of its own — and pass every call through to the real
// store.
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

// screenCost is what one GET of the positions screen cost and what it
// answered: every round trip the handler made, split by which dependency it
// went to, plus the payload itself so a test can check the cost was paid for
// real work.
//
// trips is the figure that matters, and the one this file's own review found
// pinned by nothing: connections ACQUIRED FROM THE POOL during the request —
// one per statement it sends, since nothing on this path holds a connection
// across several (no transaction is opened while the positions screen is
// read; every pool.Begin in this codebase is on a write). It is measured
// BELOW every dependency this handler has, including instrumentStore, and
// that placement is the whole point: instrument.Store.ByIDs was once
// reimplemented as a loop of ByID — one handler call, N statements — and
// handlerCalls() below stayed exactly the same across that change, because it
// counts how many times the handler asked the catalog for something, not
// what the asking cost. trips is what caught it, by counting what actually
// left the process.
//
// handlerCalls (spaces + journal + instruments + quotes + rate + batch) is
// kept alongside for diagnosis and because trips cannot see everything on its
// own: fakeQuoteStore is an in-memory map that never touches the database, so
// a quote fetch never acquires a connection and trips would read the same
// whether LatestQuotes made one call or looped into many. What these count is
// what the handler ASKED for, which is the right question for that one
// dependency and the wrong one for whether an N+1 is hiding inside a call to
// a real store.
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

// handlerCalls sums the per-dependency counters — how many times the handler
// asked each collaborator for something, once per named one (see the
// counting* types) — as distinct from trips, which counts what left the
// process (see screenCost's doc comment).
func (c screenCost) handlerCalls() int {
	return c.spaces + c.journal + c.instruments + c.quotes + c.rate + c.batch
}

func (c screenCost) String() string {
	return fmt.Sprintf("%d db round trips, %d handler calls (space %d, journal %d, instruments %d, quotes %d, one-pair rates %d, batched rates %d)",
		c.trips, c.handlerCalls(), c.spaces, c.journal, c.instruments, c.quotes, c.rate, c.batch)
}

// positionsScreen builds an account of the given size (see seedPositions),
// fetches its positions once, and reports what that one request cost.
//
// keep, when non-nil, is applied to the prefetch: only the queries it accepts
// are resolved in the batch, so a test can simulate an enumeration that has
// fallen behind the rules and check what that costs — and, more importantly,
// what it does NOT change.
func positionsScreen(t *testing.T, size int, keep func(marketdata.RateQuery) bool) screenCost {
	t.Helper()
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)

	// One rate row per pair, dated before every operation in the fixture:
	// Store.FxRateOn resolves the nearest earlier date, so every date the
	// screen asks about resolves — including today's. EUR is only quoted
	// against RUB, so the bond's EUR -> USD valuation rate is a bridge
	// through RUB, which is the most query-hungry path a single pair has and
	// exactly the one worth collapsing into the batch.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := &countingConverter{inner: marketdata.NewConverter(mdStore), keep: keep}
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

	// Building the fixture goes through the operation handler, which replays
	// the journal on every write with stores of its own — but the counters
	// are zeroed here anyway, so what they hold afterwards can only be the
	// one GET below.
	*conv = countingConverter{inner: conv.inner, keep: conv.keep}
	instruments.calls, journal.calls, spaces.calls, quotes.calls = 0, 0, 0, 0
	before := poolTrips(pool)

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET positions = %d, want 200", resp.StatusCode)
	}
	cost := screenCost{
		spaces: spaces.calls, journal: journal.calls, instruments: instruments.calls,
		quotes: quotes.calls, rate: conv.rate, batch: conv.batch,
	}
	decodeJSON(t, resp, &cost.body)
	// trips is read only once decodeJSON has drained the body, not right after
	// do() returns: today's handler finishes every round trip before it ever
	// calls httpjson.Write (everything above builds the full response value
	// first), so the two orderings agree here, but reading the delta after the
	// response is fully consumed is what keeps agreeing even if that ever
	// stopped being true — do() returning only promises the status line and
	// headers have arrived, not that nothing is still running server-side.
	cost.trips = poolTrips(pool) - before
	return cost
}

// poolTrips reads the pool's lifetime count of acquired connections. Every
// statement the handler sends takes one acquire, so the difference across a
// request is how many round trips that request made — the same technique
// operation's identically named helper uses for the journal page
// (internal/operation/http_round_trips_test.go), applied here to the
// positions screen. Acquires equal statements only while nothing opens a
// transaction: a transaction holds one connection across every statement it
// runs, undercounting them as a single acquire. Every pool.Begin in this
// codebase is on a write, so every read path here — including this one — is
// unaffected.
func poolTrips(pool *pgxpool.Pool) int64 { return pool.Stat().AcquireCount() }

// seedPositions fills one USD account of the (RUB-based) space with size share
// positions and size bond positions, and returns the account's id.
//
// Every kind of figure that costs an fx lookup grows with size, and each kind
// needs a different date or a different currency pair, so a prefetch that
// missed any one of them shows up as a cost that grows too:
//
//   - each share is bought on `size` days of its own, so the basis needs one
//     rate per lot date;
//   - each share pays a dividend on a day of its own (income) and sells a
//     piece on another (the realized result needs both the sale's day and the
//     day the parcel it retired was bought);
//   - each bond is quoted as a percentage of a face value denominated in EUR
//     while the position is in USD, so its valuation needs a rate into the
//     POSITION's currency — a different target from every other lookup on the
//     page, which converts into the space's base currency instead;
//   - every position is quoted, so each also needs today's rate to bring that
//     valuation into the base currency.
func seedPositions(t *testing.T, url string, c *http.Client, quotes *fakeQuoteStore, size int) string {
	t.Helper()
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)

	// Every operation gets a day of its own, so the number of distinct dates
	// the screen must resolve grows with the fixture rather than collapsing
	// onto one rate that would hide an N+1 behind the memo.
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

// assertScreenIsFullyWorked fails unless the response actually contains every
// figure the round-trip counters are supposed to have paid for. Without it,
// the count assertions would be satisfied by a handler that converts nothing
// at all — the cheapest screen is the one that answers nothing, and it must
// not be able to pass a performance test.
func assertScreenIsFullyWorked(t *testing.T, got positionsResp) {
	t.Helper()
	var converted, valued, realized int
	for _, p := range got.Positions {
		if p.MarketValueSourceCurrency != nil && *p.MarketValueSourceCurrency == "EUR" {
			// The bond's valuation was brought from its face currency into
			// the position's own — the lookup whose target is NOT the base
			// currency.
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

// TestPositionsRoundTripsDoNotGrowWithTheData is the requirement this whole
// change exists for: rendering the positions screen costs a FIXED number of
// round trips, whatever the account holds. Four times the positions, four
// times the lots each, four times the distinct dates — and the same number of
// trips to the database.
//
// It compares two runs of different size rather than asserting one magic
// number, deliberately. A magic number would bake in today's incidental
// fetches — the space, the journal, the catalog, the quotes — and the next
// person to add or remove one would edit the expectation and never learn
// whether the thing this test guards still holds. What must be true is not
// "five", it is "the same".
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

// TestPositionsIncompletePrewarmCostsTripsNotNumbers pins the property that
// makes the prefetch safe to have at all: it is a cache warm-up, and the
// figures do not depend on it being complete. Whatever the enumeration fails
// to ask for, the per-pair lookup resolves on its own, and the page comes out
// byte-identical — only dearer.
//
// This is what lets the enumeration be read as an optimization rather than as
// a second, silent statement of the conversion rules: the day it falls behind
// them (a new figure, a new date, a new currency pair) the screen slows down
// and stays right, instead of quietly publishing a number struck from the
// wrong rate or no number at all.
func TestPositionsIncompletePrewarmCostsTripsNotNumbers(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	full := positionsScreen(t, 2, nil)
	assertScreenIsFullyWorked(t, full.body)
	// The baseline for the comparisons below, and a check of its own: with
	// nothing dropped, nothing should fall back. A pair the enumeration forgot
	// — or asked for with the wrong target currency, which is the same thing
	// to the memo — costs one lookup per PAIR rather than one per position, so
	// it stays constant as the account grows and the growth test above cannot
	// see it. This is where it shows.
	if full.rate != 0 {
		t.Fatalf("the complete prewarm still fell back to %d one-pair lookups: %s — some rate the loop asks for is not among the ones rateQueries enumerates, or is enumerated for a different pair than it is looked up by",
			full.rate, full)
	}

	for _, tc := range []struct {
		name string
		keep func(marketdata.RateQuery) bool
	}{
		// The pair whose target is the position's currency rather than the
		// space's base — the one an enumeration written from the in_base
		// figures alone would forget.
		{"the bond valuation pair is missed", func(q marketdata.RateQuery) bool { return q.To == "RUB" }},
		// Every historical date: the basis, the income and the realized
		// result all fall back, only today's rate stays prewarmed.
		{"every historical date is missed", func(q marketdata.RateQuery) bool {
			return q.On.Format("2006-01-02") == today
		}},
		// Nothing at all is prewarmed: the handler must behave exactly as it
		// did before the prewarm existed.
		{"nothing is prewarmed", func(marketdata.RateQuery) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partial := positionsScreen(t, 2, tc.keep)

			// The two runs are separate databases, so the instrument ids
			// differ by construction; everything else — every figure, every
			// currency, every date — must match exactly.
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

// TestPositionsSharedRateMemoKeepsTargetsApart is the hazard that comes with
// sharing one memo across the whole screen: this page converts into TWO target
// currencies, and one source currency can be asked about for both on the same
// day.
//
// A bond's valuation goes into the POSITION's currency (comparability with
// cost_minor — see toAPI), everything else goes into the space's base
// currency. So a EUR position holding a bond with a USD face value needs
// USD -> EUR today, while a USD position on the same screen needs USD -> RUB
// today. Both are "USD, today". A memo that keyed on the source and the date
// alone would answer the second question with the first one's rate — whichever
// position the map happened to visit first — and publish a valuation ninety
// times too large under a caption that says nothing is wrong.
//
//	fx: USD -> RUB = 90, EUR -> RUB = 100, so USD -> EUR = 0,9
//
//	bond position (EUR), face 1 000,00 USD, quoted at par, quantity 1:
//	  raw valuation      = 100 000 minor USD
//	  market_value_minor =  90 000 minor EUR   (100 000 * 0,9)
//	  ...if the memo collided:  9 000 000 "EUR" (100 000 * 90)
//
//	share position (USD), 10 bought at 100,00, quoted at 110,00:
//	  market_value_minor = 110 000 minor USD, converted for in_base at 90
//
// The in_base valuations below are struck from each row's RAW valuation at its
// own currency's rate (#39), so the bond's is 100 000 USD * 90 rather than
// 90 000 EUR * 100. Both arrive at 9 000 000 here, because this fixture's rates
// are a consistent triangle — it is about the memo's keys, not about how many
// conversions the figure went through, which
// TestPositionInBaseValuationIsConvertedOnceFromItsOwnCurrency pins on rates
// built to tell the two apart.
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

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET positions = %d, want 200", resp.StatusCode)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
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

// inBaseMarketValue reads a position's base-currency valuation, failing the
// test rather than returning a zero when the object or the figure is absent —
// a missing figure and a figure of zero are different news here.
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

// TestPositionsAbsentInstrumentIsLoud covers what the batched catalog read
// must do about an id it finds no row for. A foreign key makes that
// unreachable through the API (operations reference instruments with ON
// DELETE RESTRICT), which is exactly why it is worth pinning here: the
// tempting shape for a batch is to skip what it did not find, and a skipped
// position would vanish from the screen — one holding silently missing from a
// portfolio, with every total quietly smaller and nothing anywhere saying so.
// Reading one instrument at a time answered pgx.ErrNoRows and the request
// became a 404; it still must.
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

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET positions whose instrument has no catalog row = %d, want 404 — a position must never be dropped from the list in silence", resp.StatusCode)
	}
}

// emptyInstruments is a catalog that holds nothing: every id asked for is
// absent, the state a real store reports for a row that is not there.
type emptyInstruments struct{}

func (emptyInstruments) ByIDs(context.Context, []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error) {
	return map[uuid.UUID]instrument.Instrument{}, nil
}
