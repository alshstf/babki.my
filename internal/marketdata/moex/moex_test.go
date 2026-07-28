package moex_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/moex"
)

const (
	sharesPath = "/iss/engines/stock/markets/shares/boards/TQBR/securities.json"
	bondsPath  = "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json"
)

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
	srv, gotQueries := serve(t, map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	})

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
	if gotQueries[sharesPath] != wantQuery {
		t.Errorf("shares request query = %q, want %q", gotQueries[sharesPath], wantQuery)
	}
	if gotQueries[bondsPath] != wantQuery {
		t.Errorf("bonds request query = %q, want %q", gotQueries[bondsPath], wantQuery)
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
	srv, _ := serve(t, map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	})

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
	srv, _ := serve(t, map[string]route{
		sharesPath: {status: http.StatusOK, body: body},
		bondsPath:  {status: http.StatusOK, body: body},
	})

	c := moex.New(srv.Client(), srv.URL)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
	if err == nil {
		t.Fatal("QuotesFor: want error when PREVPRICE column is missing, got nil")
	}
}

func TestQuotesFor_ServerError(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, map[string]route{
		sharesPath: {status: http.StatusInternalServerError, body: nil},
		bondsPath:  {status: http.StatusOK, body: shares},
	})

	c := moex.New(srv.Client(), srv.URL)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
	if err == nil {
		t.Fatal("QuotesFor: want error on HTTP 500, got nil")
	}
}

func TestQuotesFor_InvalidJSON(t *testing.T) {
	shares := readFixture(t, "shares.json")
	srv, _ := serve(t, map[string]route{
		sharesPath: {status: http.StatusOK, body: []byte(`{"securities":`)},
		bondsPath:  {status: http.StatusOK, body: shares},
	})

	c := moex.New(srv.Client(), srv.URL)
	_, err := c.QuotesFor(context.Background(), []string{"SBER"}, time.Now())
	if err == nil {
		t.Fatal("QuotesFor: want error on invalid JSON, got nil")
	}
}

func TestQuotesFor_NoTickersRequested(t *testing.T) {
	shares := readFixture(t, "shares.json")
	bonds := readFixture(t, "bonds.json")
	srv, _ := serve(t, map[string]route{
		sharesPath: {status: http.StatusOK, body: shares},
		bondsPath:  {status: http.StatusOK, body: bonds},
	})

	c := moex.New(srv.Client(), srv.URL)
	quotes, err := c.QuotesFor(context.Background(), nil, time.Now())
	if err != nil {
		t.Fatalf("QuotesFor: %v", err)
	}
	if len(quotes) != 0 {
		t.Errorf("QuotesFor(tickers=nil) = %+v, want empty", quotes)
	}
}
