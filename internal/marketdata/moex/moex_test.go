package moex_test

import (
	"context"
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
)

// The exact ISS path of every board QuotesFor is expected to query. These
// are spelled out literally rather than derived from the provider so that a
// board silently disappearing from (or appearing in) the provider's list
// shows up here as a failure naming the board.
const (
	sharesPath = "/iss/engines/stock/markets/shares/boards/TQBR/securities.json"
	bondsPath  = "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json"
	corpPath   = "/iss/engines/stock/markets/bonds/boards/TQCB/securities.json"
	corpDPath  = "/iss/engines/stock/markets/bonds/boards/TQRD/securities.json"
)

// wantBoardPaths is every path QuotesFor must request, exactly once each.
var wantBoardPaths = []string{sharesPath, bondsPath, corpPath, corpDPath}

// emptyBoard is a well-formed securities response with no rows: the columns
// the provider asks for are present, so it parses cleanly and contributes
// nothing. Used to stub boards a test does not care about.
var emptyBoard = []byte(`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[]}}`)

// allBoards fills in every board QuotesFor queries, so a test only has to
// name the boards it actually cares about; the rest serve emptyBoard. It
// exists so that adding a board to the provider does not require editing
// every test — only the tests that assert on board contents.
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
	c := moex.New(nil, "")
	if got := c.Name(); got != "moex" {
		t.Fatalf("Name() = %q, want %q", got, "moex")
	}
}

// route is one path's canned response: status and body (nil body panics the
// handler if hit unexpectedly, which is intentional — it flags board paths
// the test forgot to stub).
type route struct {
	status int
	body   []byte
}

// serve starts an httptest.Server that dispatches by exact URL path to
// routes, and records the raw query string seen for each path.
func serve(t *testing.T, routes map[string]route) (*httptest.Server, map[string]string) {
	t.Helper()
	gotQueries := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
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

	c := moex.New(srv.Client(), srv.URL)
	on := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

	// Request a mix of: a plain share (SBER), a share with a null price
	// (GAZP, must be silently dropped), a share whose price stresses
	// decimal precision (LKOH), a bond needing SUR->RUB mapping
	// (SU26238RMFS4), a bond whose currency is not SUR and must pass
	// through unchanged (RU000A105EX7), and a ticker present on neither
	// board (NOPE, must be silently absent — not an error).
	tickers := []string{"SBER", "GAZP", "LKOH", "SU26238RMFS4", "RU000A105EX7", "NOPE"}
	quotes, err := c.QuotesFor(context.Background(), tickers, on)
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	wantQuery := "iss.meta=off&securities.columns=SECID,PREVPRICE,CURRENCYID"
	for _, p := range wantBoardPaths {
		if gotQueries[p] != wantQuery {
			t.Errorf("request query for %s = %q, want %q", p, gotQueries[p], wantQuery)
		}
	}

	// GAZP (null price) and NOPE (absent from both boards) must not
	// appear; every other requested ticker should.
	if len(quotes) != 4 {
		t.Fatalf("len(quotes) = %d, want 4: %+v", len(quotes), quotes)
	}

	byTicker := make(map[string]marketdata.TickerQuote, len(quotes))
	for _, q := range quotes {
		byTicker[q.Ticker] = q
		if !q.On.Equal(on) {
			t.Errorf("%s.On = %v, want %v", q.Ticker, q.On, on)
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

	// LKOH's fixture price, 1234.567890123456789, has more significant
	// digits than a float64 can represent exactly. If the response were
	// decoded through float64 (e.g. json.Unmarshal into interface{} without
	// UseNumber, or decimal.NewFromFloat) this would silently round to
	// 1234.567890123457 or similar — a bug this assertion catches by
	// requiring an exact string match, not just numeric closeness.
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

	usdBond, ok := byTicker["RU000A105EX7"]
	if !ok {
		t.Fatalf("no RU000A105EX7 quote in %+v", quotes)
	}
	if usdBond.Currency != "USD" {
		t.Errorf("RU000A105EX7.Currency = %q, want USD unchanged (only SUR maps)", usdBond.Currency)
	}
}

func TestQuotesFor_FiltersToRequestedTickers(t *testing.T) {
	shares := readFixture(t, "shares.json")
	bonds := readFixture(t, "bonds.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
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
	// PREVPRICE column is absent entirely — this must be a hard error, not
	// a silently-empty result, since the caller has no way to distinguish
	// "no prices today" from "we can't even find the price column".
	body := []byte(`{"securities":{"columns":["SECID","CURRENCYID"],"data":[["SBER","SUR"]]}}`)
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: body},
	}))

	c := moex.New(srv.Client(), srv.URL)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
	if err == nil {
		t.Fatal("QuotesFor: want error when PREVPRICE column is missing, got nil")
	}
}

// TestQuotesFor_OneBoardFailingFailsTheWholeCall pins the documented
// failure policy: a board that errors aborts QuotesFor entirely, and the
// quotes already gathered from boards that succeeded are NOT returned.
//
// The board that fails here is deliberately not the first one — TQBR has
// already yielded a usable SBER quote by the time TQCB returns 500. A
// provider that returned that quote alongside a nil error would look, to
// quotesWorker and then to the position screen, exactly like "TQCB simply
// has no prices for your bonds today", which is the wrong cause. The whole
// point of failing is that the caller can tell a breakage from an absence.
func TestQuotesFor_OneBoardFailingFailsTheWholeCall(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		corpPath:   {status: http.StatusInternalServerError, body: emptyBoard},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "RU000A0JSGV0"}, time.Now())
	if err == nil {
		t.Fatal("QuotesFor: want error when a board returns HTTP 500, got nil")
	}
	if quotes != nil {
		t.Errorf("QuotesFor returned %+v alongside the error; a partial result must never be published", quotes)
	}
	// The error has to name the board that broke, or an operator reading the
	// job log cannot tell which board to go look at.
	if !strings.Contains(err.Error(), "TQCB") {
		t.Errorf("error %q does not name the failing board TQCB", err)
	}
}

// TestQuotesFor_QueriesEveryBoard asserts the exact set of boards requested.
// It is the guard for a board being dropped from (or quietly added to) the
// provider's list: the failure message names the individual board, since
// "an ETF is never priced" is invisible until someone notices the board is
// not being asked at all.
func TestQuotesFor_QueriesEveryBoard(t *testing.T) {
	srv, gotQueries := serve(t, allBoards(nil))

	c := moex.New(srv.Client(), srv.URL)
	if _, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now()); err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	// Only the missing direction is checked here. The converse — a board
	// requested that this test does not list — is already caught by serve,
	// which fails on any path it has no route for and names that path; a
	// second check here would be unreachable, since serve never records an
	// unrouted path in gotQueries.
	for _, p := range wantBoardPaths {
		if _, ok := gotQueries[p]; !ok {
			t.Errorf("board %s is in the expected set but was never requested", p)
		}
	}
}

// TestQuotesFor_CorporateBondsAreQuoted covers the gap this change closes on
// the bond side: TQOB carries government bonds (OFZ) only, so a corporate
// bond was previously asked about on no board at all and could never be
// priced. Both corporate boards are checked in one test because the claim
// is the same for each: the bond gets a price, quoted — like every
// bonds-market board — as a percentage of face value.
func TestQuotesFor_CorporateBondsAreQuoted(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		corpPath:  {status: http.StatusOK, body: readFixture(t, "corp_bonds.json")},
		corpDPath: {status: http.StatusOK, body: readFixture(t, "corp_bonds_d.json")},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(),
		[]string{"RU000A0JSGV0", "RU000A0JWRV9", "RU000A105SZ2"}, time.Now())
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

// TestQuotesFor_ETFIsQuotedOnTQBR pins a fact that is easy to get wrong in
// the other direction: exchange-traded funds are NOT on a fund-specific
// board. ISS reports the dedicated ETF board TQTF as not traded, with no
// securities on it, and reports TQBR as the primary traded board for funds
// like TMOS — so TQBR is what has to carry them, and a change that narrowed
// TQBR to "shares only" would silently stop pricing every fund.
func TestQuotesFor_ETFIsQuotedOnTQBR(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: readFixture(t, "shares.json")},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), []string{"TMOS"}, time.Now())
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

// TestQuotesFor_TickerOnTwoBoardsTakesTheFirst pins the merge rule: boards
// is a precedence list, and the earliest board reporting a ticker wins.
//
// Without a rule the same ticker yields two TickerQuotes, and what reaches
// the database then depends on which upsert lands last — a coin flip
// between two different prices, in two different currencies, presented as
// fact. Here TQBR and TQCB both report COLLIDE at prices that cannot be
// confused with one another.
func TestQuotesFor_TickerOnTwoBoardsTakesTheFirst(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[["COLLIDE",111.11,"SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[["COLLIDE",222.22,"USD"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), []string{"COLLIDE"}, time.Now())
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

// TestQuotesFor_NullPriceDoesNotClaimPrecedence guards the corner the
// precedence rule must not swallow: a null PREVPRICE is "no trade
// recorded", not a value. An earlier board reporting null must therefore
// leave the ticker open for a later board that has a real price, otherwise
// adding a board could take a priced instrument and un-price it.
func TestQuotesFor_NullPriceDoesNotClaimPrecedence(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[["COLLIDE",null,"SUR"]]}}`)},
		corpPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","CURRENCYID"],"data":[["COLLIDE",222.22,"SUR"]]}}`)},
	}))

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), []string{"COLLIDE"}, time.Now())
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

func TestQuotesFor_InvalidJSON(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(`{"securities":`)},
	}))

	c := moex.New(srv.Client(), srv.URL)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
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

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), nil, time.Now())
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 0 {
		t.Errorf("QuotesFor(tickers=nil) = %+v, want empty", quotes)
	}
}
