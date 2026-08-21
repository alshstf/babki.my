package cbr_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/cbr"
)

func TestName(t *testing.T) {
	c := cbr.New(nil, "")
	if got := c.Name(); got != "cbr" {
		t.Fatalf("Name() = %q, want %q", got, "cbr")
	}
}

// serve starts an httptest.Server that always responds with body and the
// given status code, and records the query string of the last request.
func serve(t *testing.T, status int, body []byte) (*httptest.Server, *string) {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotQuery
}

func TestRatesOn_ParsesFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/daily.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv, gotQuery := serve(t, http.StatusOK, fixture)

	c := cbr.New(srv.Client(), srv.URL)
	requested := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	rates, err := c.RatesOn(context.Background(), requested)
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	if *gotQuery != "date_req=28.07.2026" {
		t.Errorf("request query = %q, want %q", *gotQuery, "date_req=28.07.2026")
	}

	if len(rates) != 4 {
		t.Fatalf("len(rates) = %d, want 4: %+v", len(rates), rates)
	}

	byBase := make(map[string]marketdata.FxRate, len(rates))
	for _, r := range rates {
		byBase[r.Base] = r
	}

	wantDate := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

	// USD: Nominal=1, Value="78,5012" -> rate = 78.5012 (comma parsed, no
	// division needed).
	usd, ok := byBase["USD"]
	if !ok {
		t.Fatalf("no USD rate in %+v", rates)
	}
	if want := decimal.RequireFromString("78.5012"); !usd.Rate.Equal(want) {
		t.Errorf("USD.Rate = %s, want %s", usd.Rate, want)
	}
	if usd.Quote != "RUB" {
		t.Errorf("USD.Quote = %q, want RUB", usd.Quote)
	}
	if usd.Source != "cbr" {
		t.Errorf("USD.Source = %q, want cbr", usd.Source)
	}
	if !usd.On.Equal(wantDate) {
		t.Errorf("USD.On = %v, want %v (from response Date attribute, not request)", usd.On, wantDate)
	}

	// JPY: Nominal=100, Value="52,3410" -> rate = 52.3410 / 100 = 0.523410.
	// This is the case that would silently break if Nominal were ignored.
	jpy, ok := byBase["JPY"]
	if !ok {
		t.Fatalf("no JPY rate in %+v", rates)
	}
	if want := decimal.RequireFromString("0.523410"); !jpy.Rate.Equal(want) {
		t.Errorf("JPY.Rate (Nominal=100) = %s, want %s", jpy.Rate, want)
	}

	// KZT: Nominal=100, Value="16,3025" -> rate = 0.163025 (second
	// Nominal>1 case, different value shape than JPY).
	kzt, ok := byBase["KZT"]
	if !ok {
		t.Fatalf("no KZT rate in %+v", rates)
	}
	if want := decimal.RequireFromString("0.163025"); !kzt.Rate.Equal(want) {
		t.Errorf("KZT.Rate (Nominal=100) = %s, want %s", kzt.Rate, want)
	}

	// EUR: Nominal=1, Value="92,5678" -> rate = 92.5678.
	eur, ok := byBase["EUR"]
	if !ok {
		t.Fatalf("no EUR rate in %+v", rates)
	}
	if want := decimal.RequireFromString("92.5678"); !eur.Rate.Equal(want) {
		t.Errorf("EUR.Rate = %s, want %s", eur.Rate, want)
	}
}

// TestRatesOn_ServerError serves a perfectly parseable body under a 500 on
// purpose: with an empty body the request would fail on the XML decode no
// matter what, and the test would pass without the status ever being
// checked (confirmed by mutation testing: deleting the status check left
// this test green when the body was empty).
func TestRatesOn_ServerError(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs Date="28.07.2026" name="Foreign Currency Market">` +
		`<Valute ID="R01235"><CharCode>USD</CharCode><Nominal>1</Nominal><Value>78,5012</Value></Valute>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusInternalServerError, body)

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.RatesOn(context.Background(), time.Now())
	if err == nil {
		t.Fatal("RatesOn: want error on HTTP 500, got nil")
	}
}

func TestRatesOn_InvalidXML(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, []byte(`<ValCurs><Valute>mismatched</ValCurs>`))

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.RatesOn(context.Background(), time.Now())
	if err == nil {
		t.Fatal("RatesOn: want error on invalid XML, got nil")
	}
}

func TestRatesOn_EmptyValCurs(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs Date="28.07.2026" name="Foreign Currency Market"></ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.RatesOn(context.Background(), time.Now())
	if err == nil {
		t.Fatal("RatesOn: want error on empty ValCurs (no currencies), got nil")
	}
}

func TestCurrencyIDs_ParsesFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/daily_currency_ids.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv, _ := serve(t, http.StatusOK, fixture)

	c := cbr.New(srv.Client(), srv.URL)
	ids, err := c.CurrencyIDs(context.Background())
	if err != nil {
		t.Fatalf("CurrencyIDs: %v", err)
	}

	// USD's internal ID follows the common "R" + digits shape.
	if got, want := ids["USD"], "R01235"; got != want {
		t.Errorf(`ids["USD"] = %q, want %q`, got, want)
	}

	// TRY's internal ID has a trailing letter ("R01700J"), which is the
	// shape that would break an implementation assuming "R" + digits only.
	if got, want := ids["TRY"], "R01700J"; got != want {
		t.Errorf(`ids["TRY"] = %q, want %q`, got, want)
	}

	// A currency absent from the document must be absent from the map, not
	// present with a zero value and not an error.
	if id, ok := ids["GBP"]; ok {
		t.Errorf(`ids["GBP"] = %q, want absent (cbr.ru does not quote it in this fixture)`, id)
	}

	if len(ids) != 2 {
		t.Errorf("len(ids) = %d, want 2: %+v", len(ids), ids)
	}
}

func TestCurrencyIDs_EmptyValCurs(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs Date="28.07.2026" name="Foreign Currency Market"></ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.CurrencyIDs(context.Background())
	if err == nil {
		t.Fatal("CurrencyIDs: want error on empty ValCurs (no currencies), got nil")
	}
}

// TestCurrencyIDs_ServerError serves a perfectly parseable body under a 500
// on purpose, for the same reason as TestRatesOn_ServerError: an empty body
// would fail on the XML decode regardless of whether the status is checked.
func TestCurrencyIDs_ServerError(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs Date="28.07.2026" name="Foreign Currency Market">` +
		`<Valute ID="R01235"><CharCode>USD</CharCode><Nominal>1</Nominal><Value>78,5012</Value></Valute>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusInternalServerError, body)

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.CurrencyIDs(context.Background())
	if err == nil {
		t.Fatal("CurrencyIDs: want error on HTTP 500, got nil")
	}
}

// *cbr.Client must satisfy the history-capable provider interface, not just
// the daily one: the backfill job depends on the interface, not on this type.
var _ marketdata.FxHistoryProvider = (*cbr.Client)(nil)

// TestRatesRange_ParsesFixture works from a response captured live from
// cbr.ru's XML_dynamic.asp for USD (only the <?xml?> declaration was added to
// the captured body, which a copy-paste of the document cannot carry).
func TestRatesRange_ParsesFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/dynamic_usd.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv, gotQuery := serve(t, http.StatusOK, fixture)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	rates, err := c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err != nil {
		t.Fatalf("RatesRange: %v", err)
	}

	// The dynamic endpoint takes slash-separated dates in date_req1/date_req2
	// and identifies the currency by the bank's internal code, never by ISO.
	if want := "date_req1=01/12/2025&date_req2=05/12/2025&VAL_NM_RQ=R01235"; *gotQuery != want {
		t.Errorf("request query = %q, want %q", *gotQuery, want)
	}

	// Four <Record> elements in, four rates out, in document order. The
	// requested range starts on 01.12.2025 but the series starts on
	// 02.12.2025: cbr.ru publishes nothing for non-working days, and those
	// days must stay missing rather than be invented here.
	want := []struct {
		on   time.Time
		rate string
	}{
		// Nominal is 1 on every record of this series, so each rate is just
		// the Value with its comma read as a decimal point: 77,7027 / 1.
		{time.Date(2025, 12, 2, 0, 0, 0, 0, time.UTC), "77.7027"},
		{time.Date(2025, 12, 3, 0, 0, 0, 0, time.UTC), "77.4631"},
		{time.Date(2025, 12, 4, 0, 0, 0, 0, time.UTC), "77.9556"},
		{time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC), "76.9708"},
	}
	if len(rates) != len(want) {
		t.Fatalf("len(rates) = %d, want %d: %+v", len(rates), len(want), rates)
	}
	for i, w := range want {
		got := rates[i]
		if !got.On.Equal(w.on) {
			t.Errorf("rates[%d].On = %v, want %v (from the record's Date attribute)", i, got.On, w.on)
		}
		if wantRate := decimal.RequireFromString(w.rate); !got.Rate.Equal(wantRate) {
			t.Errorf("rates[%d].Rate = %s, want %s", i, got.Rate, wantRate)
		}
		// The response carries no ISO code at all, so Base can only come from
		// the caller-supplied code.
		if got.Base != "USD" {
			t.Errorf("rates[%d].Base = %q, want USD", i, got.Base)
		}
		if got.Quote != "RUB" {
			t.Errorf("rates[%d].Quote = %q, want RUB", i, got.Quote)
		}
		if got.Source != "cbr" {
			t.Errorf("rates[%d].Source = %q, want cbr", i, got.Source)
		}
	}
}

// TestRatesRange_NominalVariesWithinSeries is the discriminating case for
// per-record nominals: cbr.ru quotes the Turkish lira per 10 units and has
// changed that multiplier over time, so a series can carry more than one
// Nominal. An implementation that reads Nominal once per document — whichever
// record it takes it from — gets at least one of these three rates wrong.
func TestRatesRange_NominalVariesWithinSeries(t *testing.T) {
	fixture, err := os.ReadFile("testdata/dynamic_try_nominal_change.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv, _ := serve(t, http.StatusOK, fixture)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	rates, err := c.RatesRange(context.Background(), "TRY", "R01700J", from, to)
	if err != nil {
		t.Fatalf("RatesRange: %v", err)
	}

	want := []struct {
		on   time.Time
		rate string
	}{
		// Nominal=10: 18,2377 / 10 = 1.82377.
		{time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC), "1.82377"},
		// Nominal=10: 18,3210 / 10 = 1.83210.
		{time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), "1.83210"},
		// Nominal=1: 1,8455 / 1 = 1.8455. Dividing this one by the 10 the
		// earlier records carry would yield 0.18455 — an order of magnitude
		// off, and the whole point of this test.
		{time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC), "1.8455"},
	}
	// Three records for an eight-day range: the days in between are holidays
	// with nothing published. They are not gaps to fill.
	if len(rates) != len(want) {
		t.Fatalf("len(rates) = %d, want %d: %+v", len(rates), len(want), rates)
	}
	for i, w := range want {
		got := rates[i]
		if !got.On.Equal(w.on) {
			t.Errorf("rates[%d].On = %v, want %v", i, got.On, w.on)
		}
		if wantRate := decimal.RequireFromString(w.rate); !got.Rate.Equal(wantRate) {
			t.Errorf("rates[%d].Rate = %s, want %s (each record divides by its own Nominal)", i, got.Rate, wantRate)
		}
	}
}

// TestRatesRange_EmptySeries pins the difference from the daily document: an
// empty <ValCurs> there means a broken response and is an error, but here it
// legitimately means "this currency was not quoted in this range".
func TestRatesRange_EmptySeries(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01235" DateRange1="01.01.2014" DateRange2="05.01.2014" name="Foreign Currency Market Dynamic"></ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2014, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2014, 1, 5, 0, 0, 0, 0, time.UTC)
	rates, err := c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err != nil {
		t.Fatalf("RatesRange: want no error on an empty series, got %v", err)
	}
	if len(rates) != 0 {
		t.Fatalf("len(rates) = %d, want 0: %+v", len(rates), rates)
	}
}

// TestRatesRange_ServerError serves a perfectly parseable body under a 500 on
// purpose: with an empty body the request would fail on the XML decode no
// matter what, and the test would pass without the status ever being checked.
func TestRatesRange_ServerError(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01235" DateRange1="01.12.2025" DateRange2="05.12.2025" name="Foreign Currency Market Dynamic">` +
		`<Record Date="02.12.2025" Id="R01235"><Nominal>1</Nominal><Value>77,7027</Value></Record>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusInternalServerError, body)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	_, err := c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err == nil {
		t.Fatal("RatesRange: want error on HTTP 500, got nil")
	}
}

// recordingTransport is an http.RoundTripper that records the full URL of
// the last request it was asked to make and returns a canned response,
// without ever touching the network. Every other test in this file points
// the client at an httptest.Server via a non-empty baseURL, which proves the
// request shape but never proves that the *production* construction —
// cbr.New(client, ""), exactly as cmd/babki/root.go builds it — actually
// reaches the real cbr.ru endpoints. A reviewer once repointed
// DefaultDynamicURL at the daily endpoint here and the rest of the suite
// (all built on httptest.Server, which does not care what path it is asked
// for) stayed green.
type recordingTransport struct {
	gotURL string
	body   []byte
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.gotURL = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(rt.body)),
		Header:     make(http.Header),
	}, nil
}

// TestRatesRange_ProductionURL pins the exact request URL RatesRange sends
// in production. The literal slashes in date_req1/date_req2 (not %2F) are
// deliberate and already verified against the live endpoint; this test
// freezes that.
func TestRatesRange_ProductionURL(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01235" DateRange1="01.12.2025" DateRange2="05.12.2025" name="Foreign Currency Market Dynamic"></ValCurs>`)
	rt := &recordingTransport{body: body}
	c := cbr.New(&http.Client{Transport: rt}, "")

	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	if _, err := c.RatesRange(context.Background(), "USD", "R01235", from, to); err != nil {
		t.Fatalf("RatesRange: %v", err)
	}

	want := "https://www.cbr.ru/scripts/XML_dynamic.asp?date_req1=01/12/2025&date_req2=05/12/2025&VAL_NM_RQ=R01235"
	if rt.gotURL != want {
		t.Errorf("request URL = %q, want %q", rt.gotURL, want)
	}
}

// TestRatesOn_ProductionURL is TestRatesRange_ProductionURL's counterpart
// for the daily endpoint, so the pair together pin both URLs a production
// client (cbr.New(client, "")) actually calls.
func TestRatesOn_ProductionURL(t *testing.T) {
	body, err := os.ReadFile("testdata/daily.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rt := &recordingTransport{body: body}
	c := cbr.New(&http.Client{Transport: rt}, "")

	on := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	if _, err := c.RatesOn(context.Background(), on); err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	want := "https://www.cbr.ru/scripts/XML_daily.asp?date_req=28.07.2026"
	if rt.gotURL != want {
		t.Errorf("request URL = %q, want %q", rt.gotURL, want)
	}
}

// TestRatesRange_IDMismatch is the case a reviewer demonstrated live: the
// dynamic endpoint's root <ValCurs ID="..."> is the only thing in the
// response that says which currency the series belongs to, and nothing
// forced it to match what was requested. Without checking it, a series for
// a different currency (or the daily document, whose root has no ID
// attribute at all) would be accepted and shipped labeled as the requested
// one.
func TestRatesRange_IDMismatch(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01239" DateRange1="01.12.2025" DateRange2="05.12.2025" name="Foreign Currency Market Dynamic">` +
		`<Record Date="02.12.2025" Id="R01239"><Nominal>1</Nominal><Value>91,2345</Value></Record>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	_, err := c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err == nil {
		t.Fatal("RatesRange: want error when response ID (R01239) does not match requested currency (R01235), got nil")
	}
}

// TestRatesRange_WrongEndpointResponse is the other half of the same check:
// if RatesRange were ever misdirected at the daily document (as happened in
// review — see TestRatesRange_ProductionURL), that document's root <ValCurs>
// carries no ID attribute at all, so it must fail the same comparison rather
// than being silently decoded into zero records.
func TestRatesRange_WrongEndpointResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/daily.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv, _ := serve(t, http.StatusOK, fixture)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	_, err = c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err == nil {
		t.Fatal("RatesRange: want error when the response has no matching ID attribute (e.g. the daily document), got nil")
	}
}

// TestRatesRange_EscapesCurrencyID guards against a currencyID that would
// otherwise inject extra query parameters into the request. Real cbr.ru
// internal ids are always URL-safe (e.g. "R01235", "R01700J"), but the id
// comes from CurrencyIDs, which itself reads it out of XML the bank
// controls, so it must be escaped defensively rather than trusted.
func TestRatesRange_EscapesCurrencyID(t *testing.T) {
	rt := &recordingTransport{body: []byte(`<?xml version="1.0" encoding="windows-1251"?><ValCurs></ValCurs>`)}
	c := cbr.New(&http.Client{Transport: rt}, "https://example.invalid")

	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	// The response's empty ID will not match this id, so RatesRange returns
	// an error (per TestRatesRange_IDMismatch above) — irrelevant here,
	// since only the request URL that was actually sent is being checked.
	_, _ = c.RatesRange(context.Background(), "XXX", "R01235&VAL_NM_RQ=R01239", from, to)

	want := "https://example.invalid?date_req1=01/12/2025&date_req2=05/12/2025&VAL_NM_RQ=R01235%26VAL_NM_RQ%3DR01239"
	if rt.gotURL != want {
		t.Errorf("request URL = %q, want %q (currencyID must be query-escaped)", rt.gotURL, want)
	}
}

// TestRatesRange_DateOrder is the other silent-zero case: from and to
// reversed produces an empty series from cbr.ru (or, as here, would never
// even need to reach the server to know it is nonsense), and an empty slice
// with a nil error is indistinguishable from "legitimately nothing
// published in this range" (see TestRatesRange_EmptySeries). It must be a
// loud error instead.
func TestRatesRange_DateOrder(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01235" DateRange1="01.12.2025" DateRange2="05.12.2025" name="Foreign Currency Market Dynamic">` +
		`<Record Date="02.12.2025" Id="R01235"><Nominal>1</Nominal><Value>77,7027</Value></Record>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC) // to before from
	_, err := c.RatesRange(context.Background(), "USD", "R01235", from, to)
	if err == nil {
		t.Fatal("RatesRange: want error when to is before from, got nil")
	}
}

// A <Nominal> the feed did not send used to be read as zero and replaced with
// 1. For the currencies the bank quotes per 1 unit that substitution is
// invisible; for KZT, quoted per 100, it multiplies the rate by exactly a
// hundred — and KZT is the currency of a real brokerage account here. The
// inflated rate would reach fx_rates and from there balances, valuations, cost
// basis and realized profit, looking like an ordinary number the whole way.
//
// So the missing element is refused rather than defaulted, and the refusal
// names the currency: a rate nobody can tell apart from a real one is worse
// than no rate, which the app already draws honestly as a gap.
func TestRatesOn_MissingNominalIsRefusedRatherThanAssumedToBeOne(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs Date="28.07.2026" name="Foreign Currency Market">` +
		`<Valute ID="R01235"><CharCode>USD</CharCode><Nominal>1</Nominal><Value>78,5012</Value></Valute>` +
		`<Valute ID="R01335"><CharCode>KZT</CharCode><Value>16,3025</Value></Valute>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	rates, err := c.RatesOn(context.Background(), time.Now())
	if err == nil {
		t.Fatalf("RatesOn accepted a record with no <Nominal> and returned %d rates; "+
			"KZT would have been published a hundred times too high", len(rates))
	}
	if !strings.Contains(err.Error(), "KZT") {
		t.Errorf("error = %q, want it to name KZT — otherwise nobody can tell which record the feed broke", err)
	}
	if rates != nil {
		t.Errorf("RatesOn returned %d rates alongside the error; a partial answer here is "+
			"indistinguishable from a complete one", len(rates))
	}
}

// The same rule on the history feed, which carries its own Nominal per record
// because the bank re-scales how many units it quotes over the years.
func TestRatesRange_MissingNominalIsRefused(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
		`<ValCurs ID="R01335" DateRange1="01.07.2026" DateRange2="02.07.2026" name="Foreign Currency Market">` +
		`<Record Date="01.07.2026" Id="R01335"><Nominal>100</Nominal><Value>16,3025</Value></Record>` +
		`<Record Date="02.07.2026" Id="R01335"><Value>16,4111</Value></Record>` +
		`</ValCurs>`)
	srv, _ := serve(t, http.StatusOK, body)

	c := cbr.New(srv.Client(), srv.URL)
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rates, err := c.RatesRange(context.Background(), "R01335", "KZT", from, from.AddDate(0, 0, 1))
	if err == nil {
		t.Fatalf("RatesRange accepted a record with no <Nominal> and returned %d rates", len(rates))
	}
	if !strings.Contains(err.Error(), "KZT") {
		t.Errorf("error = %q, want it to name KZT", err)
	}
}

// TestEveryRequestNamesThisProgram is the fix for the outage that made every
// base-currency figure in this program unavailable at once.
//
// The Bank of Russia answers Go's default agent — "Go-http-client/2.0", which
// is what net/http sends when nothing sets the header — with 403, on both of
// its endpoints (checked live on 2026-08-21). Nothing in this client set one,
// so no rate for any historical day had ever been fetched: the table held the
// last eleven days against a journal starting in 2020, and every figure that
// needed a rate for the day a purchase actually happened published nothing with
// `no_rate` beside it. Five of the owner's six accounts showed no total at all.
//
// The assertion is on the header being SENT AND NOT GO'S OWN, rather than on
// its exact wording: what the feed refuses is the default, and a test pinned to
// one spelling would fail over a version bump that changes nothing.
func TestEveryRequestNamesThisProgram(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(*cbr.Client) error
	}{
		{"the daily table", func(cl *cbr.Client) error {
			_, err := cl.RatesOn(context.Background(), time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
			return err
		}},
		{"one currency's history", func(cl *cbr.Client) error {
			_, err := cl.RatesRange(context.Background(), "USD", "R01235",
				time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC))
			return err
		}},
		{"the currency index", func(cl *cbr.Client) error {
			_, err := cl.CurrencyIDs(context.Background())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("User-Agent")
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="windows-1251"?><ValCurs/>`)
			}))
			defer srv.Close()

			_ = c.call(cbr.New(srv.Client(), srv.URL))

			if got == "" {
				t.Fatalf("no User-Agent sent — Go fills in its own, and the Bank of Russia answers that one 403")
			}
			if strings.HasPrefix(got, "Go-http-client") {
				t.Errorf("User-Agent = %q, which is Go's default and the one the feed refuses", got)
			}
		})
	}
}
