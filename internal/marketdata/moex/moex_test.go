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
var emptyBoard = []byte(`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[]}}`)

// fixtureDate is the PREVDATE every row of every testdata fixture carries: the
// session those prices belong to. ISS reports one and the same PREVDATE for
// every row of a board (checked on 2026-08-03: all 502 TQBR rows, all 62 TQOB,
// 3019 of 3021 TQCB and all 47 TQRD read 2026-07-31), so the fixtures do the
// same. It is deliberately a date no test passes in and no clock produces, so
// an assertion on it cannot be satisfied by "today" or by anything a caller
// supplied.
var fixtureDate = time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)

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

	c := moex.New(srv.Client(), srv.URL, nil)

	// Request a mix of: a plain share (SBER), a share with a null price
	// (GAZP, must be silently dropped), a share whose price stresses
	// decimal precision (LKOH), a bond needing SUR->RUB mapping
	// (SU26238RMFS4), a bond whose currency is not SUR and must pass
	// through unchanged (RU000A105EX7), and a ticker present on neither
	// board (NOPE, must be silently absent — not an error).
	tickers := []string{"SBER", "GAZP", "LKOH", "SU26238RMFS4", "RU000A105EX7", "NOPE"}
	quotes, err := c.QuotesFor(context.Background(), tickers)
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}

	// PREVDATE is part of the pinned column set, not an incidental addition:
	// without it every quote would have to be dated by our own clock, which
	// is the whole of #90. Asking for a column ISS does not send costs
	// nothing, but NOT asking for this one costs the quote its day.
	wantQuery := "iss.meta=off&iss.only=securities&securities.columns=SECID,ISIN,PREVPRICE,PREVDATE,CURRENCYID"
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
	// THE ISIN COMES THROUGH, because it is what the price is matched to a
	// catalog row by (see marketdata.refreshQuotesWorker). A ticker names a
	// LISTING: two exchanges hand the same one to unrelated companies, and two
	// inside one currency zone hand it to them in the same currency, so ticker
	// and currency together settle nothing. The exchange has been sending this
	// field all along; this program simply did not ask for the column.
	if sber.ISIN != "RU0009029540" {
		t.Errorf("SBER.ISIN = %q, want RU0009029540", sber.ISIN)
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
	// ITS SECID IS NOT ITS ISIN, and this bond is in the fixture partly to say
	// so: the exchange calls it SU26238RMFS4 and its ISIN is RU000A1038V6
	// (checked against iss.moex.com). A reader that took the security id for
	// the identifier would be right about every corporate bond on this exchange
	// — their SECIDs really are ISINs — and wrong about every federal one.
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
	// PREVPRICE column is absent entirely — this must be a hard error, not
	// a silently-empty result, since the caller has no way to distinguish
	// "no prices today" from "we can't even find the price column".
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

// TestQuotesFor_DateTravelsWithTheRow pins where a quote's date comes from:
// the PREVDATE cell of the very row the price came from, and nowhere else.
//
// The two boards here report different PREVDATEs. That is not a claim about
// ISS — on 2026-08-03 all four boards read 2026-07-31, one date per board —
// it is how the test tells apart the three ways a date could be produced. A
// provider that used the clock, or the caller's argument, or one board-level
// date for the whole call, gives both quotes the SAME date and fails here;
// only reading each row's own cell gives two different ones.
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
		// Midnight UTC, because that is what pgx reads a Postgres DATE back
		// as: a date built in any other zone would not compare equal to the
		// same day coming out of the quotes table.
		if h, m, s := q.On.Clock(); h != 0 || m != 0 || s != 0 || q.On.Location() != time.UTC {
			t.Errorf("%s.On = %v, want midnight UTC", q.Ticker, q.On)
		}
	}
}

// TestQuotesFor_MissingPrevdateColumn is the mirror of the PREVPRICE case
// above: if ISS stops sending the column the dates come from, that must be an
// error and not a silent fallback. Anything else — today's date, the previous
// quote's date, the zero time — is a date this process made up, which is the
// defect #90 exists for.
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

// unreadableDateMsg is the line a price with no readable date must leave
// behind. Spelled out here so that demoting or rewording it is a test failure
// rather than a silent loss of the only trace such a price leaves.
const unreadableDateMsg = "moex: price came without a readable date, dropping it (this instrument keeps whatever earlier quote it already has)"

// TestQuotesFor_PriceWithUnreadableDateIsDroppedAndWarned covers the decision
// this task had to make: a row ISS priced but did not date.
//
// ISS ships unreadable dates today. Checked on 2026-08-03: 2 of TQCB's 3021
// rows carry PREVDATE "0000-00-00" — RU000A10EH19 and RU000A10FT14, both of
// which ISS's own description block gives an ISSUEDATE of 2026-08-03, i.e.
// they started trading that morning and have no previous session at all. Both
// also carry a null PREVPRICE, so today the two conditions coincide; nothing
// in ISS's contract says they always will, and the price is what this code
// would otherwise publish under an invented date.
//
// The row is dropped, and the ticker is simply absent from the result — which
// the QuoteProvider contract already defines as "no price available". Storing
// it is not open to us: on_date is half of the quotes primary key, so every
// way of storing this price starts by inventing its day. Failing the whole
// call is worse: one malformed row out of three thousand would un-price every
// instrument the owner holds, and River would retry into the same poison for
// as long as ISS kept publishing it.
//
// The null-priced row here is the one ISS actually ships, and it must stay
// silent: it loses nothing, and a line per never-traded instrument would be
// noise that buries the one line that matters.
func TestQuotesFor_PriceWithUnreadableDateIsDroppedAndWarned(t *testing.T) {
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(
			`{"securities":{"columns":["SECID","PREVPRICE","PREVDATE","CURRENCYID"],"data":[` +
				`["SBER",305.55,"2026-07-24","SUR"],` +
				`["NODATE",111.11,"0000-00-00","SUR"],` +
				`["NEVERTRADED",null,"0000-00-00","SUR"]]}}`)},
	}))

	var records []slog.Record
	c := moex.New(srv.Client(), srv.URL, slog.New(&recordingHandler{records: &records}))
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
	for _, r := range records {
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
		// The raw cell has to be in the line: "0000-00-00" (a security with no
		// previous session) and a changed date format are the same failure to
		// this code and completely different failures to whoever reads the log.
		if attrs["prevdate"] != "0000-00-00" {
			t.Errorf("warning carried prevdate=%q, want the raw cell %q", attrs["prevdate"], "0000-00-00")
		}
	}
	if len(warned) != 1 || warned[0] != "NODATE" {
		t.Fatalf("warned about %v, want exactly [NODATE]: the priced row must be reported and the "+
			"null-priced one must not — it loses nothing", warned)
	}
}

// TestQuotesFor_WhatCountsAsAnUnreadableDate pins the boundary of "readable",
// one form per case, because each of these is a different way for the column
// to stop meaning what it means today and each must cost the price rather
// than produce a day.
//
// "0001-01-01" is the one that needs saying out loud: it is a perfectly valid
// date and parses without complaint, and it is also Go's zero time — the value
// a forgotten assignment leaves behind. Accepted, it would be stored as a
// quote from year 1 and would make "this price has a day" indistinguishable
// from "this price has none", which is the distinction the whole change rests
// on.
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

			c := moex.New(srv.Client(), srv.URL, slog.New(&recordingHandler{records: &[]slog.Record{}}))
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

// TestQuotesFor_UnreadableDateDoesNotClaimPrecedence is the same corner
// TestQuotesFor_NullPriceDoesNotClaimPrecedence guards, for the other reason a
// row can be unusable. A row that cannot be published must not consume the
// ticker's one slot, or adding a board could take a priced instrument and
// un-price it.
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

	c := moex.New(srv.Client(), srv.URL, nil)
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER", "RU000A0JSGV0"})
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

	c := moex.New(srv.Client(), srv.URL, nil)
	if _, err := c.QuotesFor(context.Background(), []string{"SBER"}); err != nil {
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

// TestQuotesFor_TMOSRowRecordsETFOnTQBRDecision does not prove that ISS
// puts exchange-traded funds on TQBR rather than the dedicated (and
// currently empty) TQTF board — no offline test can pin a fact about a
// live third-party API, and a live-network test here would be worse: slow,
// flaky, and dependent on TMOS still trading whenever CI happens to run.
// See the shares/TQBR entry in the boards doc comment for the live-checked
// evidence the decision actually rests on.
//
// What this test does is record that decision and guard the fixture it
// depends on: the only edit that reddens this test alone is deleting the
// TMOS row from testdata/shares.json. Every code mutation that would break
// the underlying claim (e.g. filtering out fund tickers, or mishandling a
// row that happens to be an ETF) also breaks four or more other tests,
// starting with TestQuotesFor_ParsesFixture, which already pins the same
// parsing behaviour via SBER.
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

// TestQuotesFor_NullPriceDoesNotClaimPrecedence guards the corner the
// precedence rule must not swallow: a null PREVPRICE is "no trade
// recorded", not a value. An earlier board reporting null must therefore
// leave the ticker open for a later board that has a real price, otherwise
// adding a board could take a priced instrument and un-price it.
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

// recordingHandler captures records so a test can assert on the LEVEL and the
// attributes of a log line rather than on a substring of rendered text — a
// substring match cannot tell a Warn from a Debug, and this repository has
// already shipped one test that passed for exactly that wrong reason.
type recordingHandler struct{ records *[]slog.Record }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestQuotesFor_EmptyBoardIsWarned covers the case where ISS answers 200 with
// no securities at all. That is not a quiet day: securities.json lists what is
// LISTED, not what has traded, so every board queried here carries hundreds or
// thousands of rows on any day of the week. Zero rows means the path has
// stopped naming a live board — ISS answers exactly this way for a board that
// was renamed or retired, and seven such paths were found while choosing this
// board list.
//
// Without the warning, every instrument on that board simply has no price, and
// the screen reports that as "no quote" — a statement about the instrument,
// when the truth is a statement about our URL.
//
// The other boards' prices must survive: the response was valid, and failing
// the call would throw away three boards of correct data over the fourth.
func TestQuotesFor_EmptyBoardIsWarned(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, allBoards(map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
	}))

	var records []slog.Record
	c := moex.New(srv.Client(), srv.URL, slog.New(&recordingHandler{records: &records}))
	quotes, err := c.QuotesFor(context.Background(), []string{"SBER"})
	if err != nil {
		t.Fatalf("QuotesFor: %v — an empty board must not fail the whole call", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("QuotesFor returned %d quotes, want 1: the boards that did answer must still be used", len(quotes))
	}

	// Three of the four boards served emptyBoard, so exactly three lines, each
	// naming its own board. Counting them is what catches a warning emitted
	// once per call instead of once per board.
	var warned []string
	for _, r := range records {
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
