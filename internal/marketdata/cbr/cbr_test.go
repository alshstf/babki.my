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

// serve answers every request with body and status, recording the last query.
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

	// USD: nominal 1, comma read as the decimal point.
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

	// JPY: nominal 100, so the value is divided by it.
	jpy, ok := byBase["JPY"]
	if !ok {
		t.Fatalf("no JPY rate in %+v", rates)
	}
	if want := decimal.RequireFromString("0.523410"); !jpy.Rate.Equal(want) {
		t.Errorf("JPY.Rate (Nominal=100) = %s, want %s", jpy.Rate, want)
	}

	// KZT: nominal 100 with another value shape.
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

// A parseable body under a 500, so only the status check can fail the call.
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

	// TRY's identifier has a letter suffix.
	if got, want := ids["TRY"], "R01700J"; got != want {
		t.Errorf(`ids["TRY"] = %q, want %q`, got, want)
	}

	// A currency absent from the document is absent from the map.
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

// A parseable body under a 500, as above.
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

var _ marketdata.FxHistoryProvider = (*cbr.Client)(nil)

// Parses a response captured live from XML_dynamic.asp for USD.
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

	// Slash-separated dates and the bank's identifier, never the ISO code.
	if want := "date_req1=01/12/2025&date_req2=05/12/2025&VAL_NM_RQ=R01235"; *gotQuery != want {
		t.Errorf("request query = %q, want %q", *gotQuery, want)
	}

	// Non-working days have no record and stay missing.
	want := []struct {
		on   time.Time
		rate string
	}{
		// Nominal 1 throughout.
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
		// The response has no ISO code; Base comes from the caller.
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

// The lira's nominal changes within one series, so each record's own nominal
// must be used.
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
		// Nominal 1: dividing by the earlier 10 would be ten times off.
		{time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC), "1.8455"},
	}
	// The days in between are holidays, not gaps to fill.
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

// An empty series is valid here, unlike an empty daily document.
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

// recordingTransport records the requested URL without the network, so the
// production endpoints (cbr.New(client, "")) are actually checked; an
// httptest server accepts any path.
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

// The production RatesRange URL, with literal slashes in the dates as the
// live endpoint accepts them.
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

// The production RatesOn URL.
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

// RatesRange refuses a response for another currency, reversed dates (not an
// empty series that would read as "nothing published"), and a parseable body
// under a 500.
func TestRatesRangeRefuses(t *testing.T) {
	record := func(id string) []byte {
		return []byte(`<?xml version="1.0" encoding="windows-1251"?>` +
			`<ValCurs ID="` + id + `" DateRange1="01.12.2025" DateRange2="05.12.2025" name="Foreign Currency Market Dynamic">` +
			`<Record Date="02.12.2025" Id="` + id + `"><Nominal>1</Nominal><Value>77,7027</Value></Record>` +
			`</ValCurs>`)
	}
	dec1 := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	dec5 := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		status   int
		body     []byte
		from, to time.Time
	}{
		"another currency's ID": {http.StatusOK, record("R01239"), dec1, dec5},
		"to before from":        {http.StatusOK, record("R01235"), dec5, dec1},
		"HTTP 500":              {http.StatusInternalServerError, record("R01235"), dec1, dec5},
	} {
		srv, _ := serve(t, tc.status, tc.body)
		c := cbr.New(srv.Client(), srv.URL)
		if _, err := c.RatesRange(context.Background(), "USD", "R01235", tc.from, tc.to); err == nil {
			t.Errorf("%s: RatesRange succeeded, want an error", name)
		}
	}
}

// The daily document's root has no ID, so a misdirected request fails the same
// check.
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

// The currency id is escaped: it comes from XML the bank controls.
func TestRatesRange_EscapesCurrencyID(t *testing.T) {
	rt := &recordingTransport{body: []byte(`<?xml version="1.0" encoding="windows-1251"?><ValCurs></ValCurs>`)}
	c := cbr.New(&http.Client{Transport: rt}, "https://example.invalid")

	from := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
	// Only the URL sent matters here.
	_, _ = c.RatesRange(context.Background(), "XXX", "R01235&VAL_NM_RQ=R01239", from, to)

	want := "https://example.invalid?date_req1=01/12/2025&date_req2=05/12/2025&VAL_NM_RQ=R01235%26VAL_NM_RQ%3DR01239"
	if rt.gotURL != want {
		t.Errorf("request URL = %q, want %q (currencyID must be query-escaped)", rt.gotURL, want)
	}
}

// A missing nominal is refused, naming the currency: reading it as 1 would
// inflate KZT (quoted per 100) a hundredfold everywhere downstream.
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

// The same rule on the history feed.
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

// Every request sends a User-Agent other than Go's default, which the bank
// refuses with 403 (checked 2026-08-21); without it most base-currency
// figures were missing.
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
