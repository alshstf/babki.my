package moex_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/moex"
	"babki.my/babki/internal/platform/logtest"
)

// Every board path QuotesFor must query, spelled out so a board dropped from
// or added to the provider fails by name.
const (
	sharesPath = "/iss/engines/stock/markets/shares/boards/TQBR/securities.json"
	bondsPath  = "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json"
	corpPath   = "/iss/engines/stock/markets/bonds/boards/TQCB/securities.json"
	corpDPath  = "/iss/engines/stock/markets/bonds/boards/TQRD/securities.json"
)

// wantBoardPaths is every path QuotesFor must request, exactly once each.
var wantBoardPaths = []string{sharesPath, bondsPath, corpPath, corpDPath}

// emptyBoard is a valid securities response with no rows.
var emptyBoard = []byte(`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[]}}`)

// fixtureDate is the PREVDATE of every fixture row (ISS reports one per
// board), a date no clock or caller produces.
var fixtureDate = time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)

// allBoards serves emptyBoard for every board a test does not override.
func allBoards(overrides map[string]route) map[string]route {
	routes := make(map[string]route, len(wantBoardPaths))
	for _, p := range wantBoardPaths {
		routes[p] = route{status: http.StatusOK, body: emptyBoard}
	}
	for p, r := range overrides {
		routes[p] = r
	}
	return routes
}

func TestName(t *testing.T) {
	c := moex.New(nil, "", nil)
	if got := c.Name(); got != "moex" {
		t.Fatalf("Name() = %q, want %q", got, "moex")
	}
}

// route is one path's canned response: status and body.
type route struct {
	status int
	body   []byte
}

// noHistory is ISS's answer about a security it holds no sessions of.
const noHistory = `{"history":{"columns":["TRADEDATE","NUMTRADES"],"data":[]}}`

// serve dispatches by exact path to routes and records each path's query.
func serve(t *testing.T, routes map[string]route) (*httptest.Server, map[string]string) {
	t.Helper()
	gotQueries := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
		if !ok && strings.HasPrefix(r.URL.Path, "/iss/history/engines/stock/") {
			// Unknown history answers an empty block, so the price keeps its session's
			// date.
			rt, ok = route{status: http.StatusOK, body: []byte(noHistory)}, true
		}
		if !ok {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotQueries[r.URL.Path] = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = w.Write(rt.body)
	}))
	t.Cleanup(srv.Close)
	return srv, gotQueries
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestQuotesFor_ParsesFixture(t *testing.T) {
	shares := readFixture(t, "shares.json")
	bonds := readFixture(t, "bonds.json")
	srv, gotQueries := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)

	// A plain share, a null price (dropped), a high-precision price, a SUR bond,
	// a non-SUR bond, a dollar bond traded for roubles, and a ticker on no board
	// (absent, not an error).
	tickers := []string{"SBER", "GAZP", "LKOH", "SU26238RMFS4", "RU000A105EX7", "BYM000001818", "NOPE"}
	quotes, err := c.QuotesFor(context.Background(), tickers)
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	// PREVDATE must be requested: without it the quote has no day (#90). A bond
	// board is also asked for the face and the accrued interest.
	for _, p := range wantBoardPaths {
		wantQuery := "iss.meta=off&iss.only=securities&securities.columns=SECID,ISIN,PREVPRICE,PREVDATE,CURRENCYID"
		if strings.Contains(p, "/markets/bonds/") {
			wantQuery += ",FACEVALUE,ACCRUEDINT,FACEUNIT"
		}
		if gotQueries[p] != wantQuery {
			t.Errorf("request query for %s = %q, want %q", p, gotQueries[p], wantQuery)
		}
	}

	// GAZP (null price) and NOPE (absent) do not appear.
	if len(quotes) != 5 {
		t.Fatalf("len(quotes) = %d, want 5: %+v", len(quotes), quotes)
	}

	byTicker := make(map[string]marketdata.TickerQuote, len(quotes))
	for _, q := range quotes {
		byTicker[q.Ticker] = q
		if !q.On.Equal(fixtureDate) {
			t.Errorf("%s.On = %v, want %v — the quote's date is the session ISS named in PREVDATE, "+
				"never a date this process chose", q.Ticker, q.On, fixtureDate)
		}
	}

	if _, ok := byTicker["GAZP"]; ok {
		t.Error("GAZP has null PREVPRICE and must be omitted, but was present")
	}
	if _, ok := byTicker["NOPE"]; ok {
		t.Error("NOPE is not in either fixture and must be omitted, but was present")
	}

	sber, ok := byTicker["SBER"]
	if !ok {
		t.Fatalf("no SBER quote in %+v", quotes)
	}
	if want := decimal.RequireFromString("305.55"); !sber.Price.Equal(want) {
		t.Errorf("SBER.Price = %s, want %s", sber.Price, want)
	}
	if sber.Currency != "RUB" {
		t.Errorf("SBER.Currency = %q, want RUB (SUR must map to RUB)", sber.Currency)
	}
	// The ISIN comes through: prices are matched to the catalog by it.
	if sber.ISIN != "RU0009029540" {
		t.Errorf("SBER.ISIN = %q, want RU0009029540", sber.ISIN)
	}

	// More digits than a float64 holds: an exact string match catches decoding
	// through float64.
	lkoh, ok := byTicker["LKOH"]
	if !ok {
		t.Fatalf("no LKOH quote in %+v", quotes)
	}
	wantLkoh := decimal.RequireFromString("1234.567890123456789")
	if !lkoh.Price.Equal(wantLkoh) {
		t.Errorf("LKOH.Price = %s, want %s", lkoh.Price, wantLkoh)
	}
	if got := lkoh.Price.String(); got != "1234.567890123456789" {
		t.Errorf("LKOH.Price.String() = %q, want exact digit match %q (precision lost)", got, "1234.567890123456789")
	}

	bond, ok := byTicker["SU26238RMFS4"]
	if !ok {
		t.Fatalf("no SU26238RMFS4 quote in %+v", quotes)
	}
	if want := decimal.RequireFromString("99.85"); !bond.Price.Equal(want) {
		t.Errorf("SU26238RMFS4.Price = %s, want %s", bond.Price, want)
	}
	if bond.Currency != "RUB" {
		t.Errorf("SU26238RMFS4.Currency = %q, want RUB (SUR must map to RUB)", bond.Currency)
	}
	// A federal bond's SECID (SU26238RMFS4) is not its ISIN (RU000A1038V6), unlike
	// corporate bonds'.
	if bond.ISIN != "RU000A1038V6" {
		t.Errorf("SU26238RMFS4.ISIN = %q, want RU000A1038V6 — the SECID is not the ISIN here", bond.ISIN)
	}

	usdBond, ok := byTicker["RU000A105EX7"]
	if !ok {
		t.Fatalf("no RU000A105EX7 quote in %+v", quotes)
	}
	if usdBond.Currency != "USD" {
		t.Errorf("RU000A105EX7.Currency = %q, want USD unchanged (only SUR maps)", usdBond.Currency)
	}

	// A bond carries its face and accrued interest; a share does not.
	if sber.Bond != nil {
		t.Errorf("SBER.Bond = %+v, want nil: a share has no face to quote against", sber.Bond)
	}
	for ticker, want := range map[string]struct{ face, accrued, currency string }{
		"SU26238RMFS4": {"1000", "12.34", "RUB"},
		"RU000A105EX7": {"1000", "5.67", "USD"},
		// Settled in roubles, the interest is stated in roubles: not kept.
		"BYM000001818": {"1000", "", "USD"},
	} {
		b := byTicker[ticker].Bond
		if b == nil {
			t.Errorf("%s.Bond = nil, want face %s %s", ticker, want.face, want.currency)
			continue
		}
		accrued := ""
		if b.Accrued != nil {
			accrued = b.Accrued.String()
		}
		if b.Face.String() != want.face || b.Currency != want.currency || accrued != want.accrued {
			t.Errorf("%s.Bond = face %s %s, accrued %q; want %s %s, %q", ticker, b.Face, b.Currency, accrued, want.face, want.currency, want.accrued)
		}
	}
}

func TestQuotesFor_FiltersToRequestedTickers(t *testing.T) {
	shares := readFixture(t, "shares.json")
	bonds := readFixture(t, "bonds.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	var tickers []string
	for _, q := range quotes {
		tickers = append(tickers, q.Ticker)
	}
	sort.Strings(tickers)

	if len(tickers) != 1 || tickers[0] != "SBER" {
		t.Errorf("QuotesFor(tickers=[SBER]) returned tickers %v, want [SBER]", tickers)
	}
}

func TestQuotesFor_MissingColumn(t *testing.T) {
	// A missing PREVPRICE column is an error, not an empty result.
	body := []byte(`{"securities":{"columns":["SECID","PREVDATE","CURRENCYID"],"data":[["SBER","2026-07-24","SUR"]]}}`)
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: body},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err == nil {
		t.Fatal("QuotesFor: want error when PREVPRICE column is missing, got nil")
	}
}

// The date comes from each row's own PREVDATE: the two boards here differ, so
// a clock, the caller or one date per call would fail.
func TestQuotesFor_DateTravelsWithTheRow(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["SBER",305.55,"2026-07-24","SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["RU000A0JSGV0",98.76,"2026-07-17","SUR"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "RU000A0JSGV0"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	want := map[string]time.Time{
		"SBER":         time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC),
		"RU000A0JSGV0": time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
	}
	if len(quotes) != len(want) {
		t.Fatalf("len(quotes) = %d, want %d: %+v", len(quotes), len(want), quotes)
	}
	for _, q := range quotes {
		if !q.On.Equal(want[q.Ticker]) {
			t.Errorf("%s.On = %v, want %v", q.Ticker, q.On.Format(time.RFC3339), want[q.Ticker].Format(time.RFC3339))
		}
		// Midnight UTC, as pgx reads a DATE back.
		if h, m, s := q.On.Clock(); h != 0 || m != 0 || s != 0 || q.On.Location() != time.UTC {
			t.Errorf("%s.On = %v, want midnight UTC", q.Ticker, q.On)
		}
	}
}

// A missing PREVDATE column is an error, not an invented date (#90).
func TestQuotesFor_MissingPrevdateColumn(t *testing.T) {
	body := []byte(`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[["SBER",305.55,"SUR"]]}}`)
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: body},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err == nil {
		t.Fatal("QuotesFor: want error when the PREVDATE column is missing, got nil")
	}
	if !strings.Contains(err.Error(), "PREVDATE") {
		t.Errorf("error %q does not name the missing column PREVDATE", err)
	}
}

const unreadableDateMsg = "moex: price came without a readable date, dropping it (this instrument keeps whatever earlier quote it already has)"

// nonPositivePriceMsg is the line a zero or negative price must leave behind.
const nonPositivePriceMsg = "moex: price is not positive, dropping it (this instrument keeps whatever earlier quote it already has)"

// A priced row with an unreadable date ("0000-00-00", as ISS sends for a
// security listed that morning) is dropped with a warning; the ticker is
// absent. A null-priced row stays silent.
func TestQuotesFor_PriceWithUnreadableDateIsDroppedAndWarned(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[` +
				`["SBER",305.55,"2026-07-24","SUR"],` +
				`["NODATE",111.11,"0000-00-00","SUR"],` +
				`["NEVERTRADED",null,"0000-00-00","SUR"]]}}`)},
	}))

	logs := &logtest.Capture{}
	c := moex.New(srv.Client(), srv.URL, logs.Logger())
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "NODATE", "NEVERTRADED"})
	if err != nil {
		t.Fatalf("QuotesFor: %v — one undatable row must not fail the call", err)
	}

	byTicker := make(map[string]marketdata.TickerQuote, len(quotes))
	for _, q := range quotes {
		byTicker[q.Ticker] = q
	}
	if q, ok := byTicker["NODATE"]; ok {
		t.Errorf("NODATE was published as %+v; a price whose day cannot be read has no date to be stored under", q)
	}
	if _, ok := byTicker["SBER"]; !ok {
		t.Errorf("SBER lost its quote: one undatable row must not cost the rest of the board its prices")
	}

	var warned []string
	for _, r := range logs.Records() {
		if r.Message != unreadableDateMsg {
			continue
		}
		if r.Level != slog.LevelWarn {
			t.Errorf("the undatable price was logged at %s, want WARN: Debug is off on a production instance, "+
				"which is exactly where a silently un-priced instrument would go unnoticed", r.Level)
		}
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		warned = append(warned, attrs["ticker"])
		// The raw cell is in the line: "0000-00-00" and a changed format are
		// different problems to whoever reads it.
		if attrs["prevdate"] != "0000-00-00" {
			t.Errorf("warning carried prevdate=%q, want the raw cell %q", attrs["prevdate"], "0000-00-00")
		}
	}
	if len(warned) != 1 || warned[0] != "NODATE" {
		t.Fatalf("warned about %v, want exactly [NODATE]: the priced row must be reported and the "+
			"null-priced one must not — it loses nothing", warned)
	}
}

// Each unreadable form costs the price; "0001-01-01" parses but is Go's zero
// time and is refused too.
func TestQuotesFor_WhatCountsAsAnUnreadableDate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cell string // the PREVDATE cell, as JSON
	}{
		{"no previous session", `"0000-00-00"`},
		{"the zero day", `"0001-01-01"`},
		{"empty", `""`},
		{"day-first format", `"31.07.2026"`},
		{"unpadded month", `"2026-7-31"`},
		{"a day that does not exist", `"2026-02-30"`},
		{"a timestamp rather than a day", `"2026-07-31 00:00:00"`},
		{"json null", `null`},
		{"not a string at all", `20260731`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, allBoards(map[string]route{
				sharesPath: {status: http.StatusOK, body: []byte(
					`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["SBER",305.55,` + tc.cell + `,"SUR"]]}}`)},
			}))

			c := moex.New(srv.Client(), srv.URL, (&logtest.Capture{}).Logger())
			quotes, err := c.QuotesFor(context.Background(), []string{"SBER"})
			if err != nil {
				t.Fatalf("QuotesFor: %v — an unreadable date costs the price, it does not fail the call", err)
			}
			if len(quotes) != 0 {
				t.Fatalf("PREVDATE %s produced %+v; want no quote at all", tc.cell, quotes)
			}
		})
	}
}

// An undatable row does not take the ticker's slot from a later board.
func TestQuotesFor_UnreadableDateDoesNotClaimPrecedence(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",111.11,"0000-00-00","SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",222.22,"2026-07-24","SUR"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"COLLIDE"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("len(quotes) = %d, want 1: %+v", len(quotes), quotes)
	}
	if want := decimal.RequireFromString("222.22"); !quotes[0].Price.Equal(want) {
		t.Errorf("COLLIDE.Price = %s, want %s — an undatable row on an earlier board must not block a later usable one",
			quotes[0].Price, want)
	}
	if want := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC); !quotes[0].On.Equal(want) {
		t.Errorf("COLLIDE.On = %v, want %v", quotes[0].On, want)
	}
}

// One failing board fails the whole call, discarding the boards already read:
// a partial result would read as "no prices" for the failed board.
func TestQuotesFor_OneBoardFailingFailsTheWholeCall(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		corpPath:   {status: http.StatusInternalServerError, body: emptyBoard},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "RU000A0JSGV0"})
	if err == nil {
		t.Fatal("QuotesFor: want error when a board returns HTTP 500, got nil")
	}
	if quotes != nil {
		t.Errorf("QuotesFor returned %+v alongside the error; a partial result must never be published", quotes)
	}
	// The error names the board.
	if !strings.Contains(err.Error(), "TQCB") {
		t.Errorf("error %q does not name the failing board TQCB", err)
	}
}

// Exactly the expected boards are requested.
func TestQuotesFor_QueriesEveryBoard(t *testing.T) {
	srv, gotQueries := serve(t, allBoards(nil))

	c := moex.New(srv.Client(), srv.URL, nil)
	if _, err := c.QuotesFor(context.Background(), []string{"SBER"}); err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	// Only the missing direction: serve already fails on an unrouted path.
	for _, p := range wantBoardPaths {
		if _, ok := gotQueries[p]; !ok {
			t.Errorf("board %s is in the expected set but was never requested", p)
		}
	}
}

// Corporate bonds are priced on TQCB and TQRD, in percent of face.
func TestQuotesFor_CorporateBondsAreQuoted(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		corpPath:  {status: http.StatusOK, body: readFixture(t, "corp_bonds.json")},
		corpDPath: {status: http.StatusOK, body: readFixture(t, "corp_bonds_d.json")},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(),
		[]string{"RU000A0JSGV0", "RU000A0JWRV9", "RU000A105SZ2"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	byTicker := make(map[string]marketdata.TickerQuote, len(quotes))
	for _, q := range quotes {
		byTicker[q.Ticker] = q
	}

	for _, tc := range []struct {
		ticker string
		price  string
		board  string
	}{
		{"RU000A0JSGV0", "98.76", "TQCB"},
		{"RU000A0JWRV9", "101.54", "TQCB"},
		{"RU000A105SZ2", "12.9", "TQRD"},
	} {
		q, ok := byTicker[tc.ticker]
		if !ok {
			t.Errorf("corporate bond %s (%s) got no quote; board not queried?", tc.ticker, tc.board)
			continue
		}
		if want := decimal.RequireFromString(tc.price); !q.Price.Equal(want) {
			t.Errorf("%s.Price = %s, want %s", tc.ticker, q.Price, want)
		}
		if q.Currency != "RUB" {
			t.Errorf("%s.Currency = %q, want RUB", tc.ticker, q.Currency)
		}
	}
}

// Records the decision that funds trade on TQBR (live evidence is in the
// boards doc) and guards the TMOS fixture row it rests on.
func TestQuotesFor_TMOSRowRecordsETFOnTQBRDecision(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: readFixture(t, "shares.json")},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"TMOS"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("len(quotes) = %d, want 1 (the ETF TMOS): %+v", len(quotes), quotes)
	}
	if want := decimal.RequireFromString("5.57"); !quotes[0].Price.Equal(want) {
		t.Errorf("TMOS.Price = %s, want %s", quotes[0].Price, want)
	}
}

// The first board reporting a ticker wins, rather than whichever upsert lands
// last.
func TestQuotesFor_TickerOnTwoBoardsTakesTheFirst(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",111.11,"2026-07-24","SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",222.22,"2026-07-24","USD"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"COLLIDE"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("len(quotes) = %d, want exactly 1 — a ticker on two boards must collapse to one quote: %+v", len(quotes), quotes)
	}
	if want := decimal.RequireFromString("111.11"); !quotes[0].Price.Equal(want) {
		t.Errorf("COLLIDE.Price = %s, want %s (TQBR precedes TQCB in the board list)", quotes[0].Price, want)
	}
	if quotes[0].Currency != "RUB" {
		t.Errorf("COLLIDE.Currency = %q, want RUB (TQBR's row, not TQCB's USD one)", quotes[0].Currency)
	}
}

// A null price on an earlier board leaves the ticker for a later board.
func TestQuotesFor_NullPriceDoesNotClaimPrecedence(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",null,"2026-07-24","SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[["COLLIDE",222.22,"2026-07-24","SUR"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"COLLIDE"})
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("len(quotes) = %d, want 1: %+v", len(quotes), quotes)
	}
	if want := decimal.RequireFromString("222.22"); !quotes[0].Price.Equal(want) {
		t.Errorf("COLLIDE.Price = %s, want %s — a null on an earlier board must not block a later real price", quotes[0].Price, want)
	}
}

// A zero or negative price (a suspended issue, #191) is dropped with a
// warning and leaves the ticker for a later board.
func TestQuotesFor_NonPositivePriceIsNotAPrice(t *testing.T) {
	const cols = `"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"]`
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(`{"securities":{` + cols + `,"data":[` +
			`["SBER",305.55,"2026-07-24","SUR"],` +
			`["ZERO",0,"2026-07-24","SUR"],` +
			`["NEGATIVE",-1.5,"2026-07-24","SUR"],` +
			`["COLLIDE",0,"2026-07-24","SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(`{"securities":{` + cols + `,"data":[` +
			`["COLLIDE",222.22,"2026-07-24","SUR"]]}}`)},
	}))

	logs := &logtest.Capture{}
	c := moex.New(srv.Client(), srv.URL, logs.Logger())
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "ZERO", "NEGATIVE", "COLLIDE"})
	if err != nil {
		t.Fatalf("QuotesFor: %v — a zero price must not fail the call", err)
	}

	got := make(map[string]string, len(quotes))
	for _, q := range quotes {
		got[q.Ticker] = q.Price.String()
	}
	want := map[string]string{"SBER": "305.55", "COLLIDE": "222.22"}
	if len(got) != len(want) {
		t.Fatalf("quotes = %v, want exactly %v", got, want)
	}
	for ticker, price := range want {
		if got[ticker] != price {
			t.Errorf("%s = %q, want %q", ticker, got[ticker], price)
		}
	}

	warned := map[string]bool{}
	for _, r := range logs.Records() {
		if r.Message != nonPositivePriceMsg {
			continue
		}
		if r.Level != slog.LevelWarn {
			t.Errorf("the dropped price was logged at %s, want WARN", r.Level)
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "ticker" {
				warned[a.Value.String()] = true
			}
			return true
		})
	}
	for _, ticker := range []string{"ZERO", "NEGATIVE", "COLLIDE"} {
		if !warned[ticker] {
			t.Errorf("no warning named %s", ticker)
		}
	}
}

func TestQuotesFor_InvalidJSON(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(`{"securities":`)},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err == nil {
		t.Fatal("QuotesFor: want error on invalid JSON, got nil")
	}
}

func TestQuotesFor_NoTickersRequested(t *testing.T) {
	shares := readFixture(t, "shares.json")
	bonds := readFixture(t, "bonds.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	}))

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 0 {
		t.Errorf("QuotesFor(tickers=nil) = %+v, want empty", quotes)
	}
}

// A board answering no securities is warned about (it has stopped being a
// live board), and the other boards' prices survive.
func TestQuotesFor_EmptyBoardIsWarned(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
	}))

	logs := &logtest.Capture{}
	c := moex.New(srv.Client(), srv.URL, logs.Logger())
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err != nil {
		t.Fatalf("QuotesFor: %v — an empty board must not fail the whole call", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("QuotesFor returned %d quotes, want 1: the boards that did answer must still be used", len(quotes))
	}

	// Three empty boards, three lines, each naming its board.
	var warned []string
	for _, r := range logs.Records() {
		if r.Message != "moex: board returned no securities at all, everything listed on it will have no price" {
			continue
		}
		if r.Level != slog.LevelWarn {
			t.Errorf("the empty board was logged at %s, want WARN: Debug is off on a production instance, "+
				"which is exactly where an un-priced board would go unnoticed", r.Level)
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "board" {
				warned = append(warned, a.Value.String())
			}
			return true
		})
	}
	sort.Strings(warned)
	want := []string{"bonds/TQCB", "bonds/TQOB", "bonds/TQRD"}
	if strings.Join(warned, ",") != strings.Join(want, ",") {
		t.Fatalf("warned about boards %v, want %v", warned, want)
	}
}
