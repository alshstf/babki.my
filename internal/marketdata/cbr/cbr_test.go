package cbr_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestRatesOn_ServerError(t *testing.T) {
	srv, _ := serve(t, http.StatusInternalServerError, nil)

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

func TestCurrencyIDs_ServerError(t *testing.T) {
	srv, _ := serve(t, http.StatusInternalServerError, nil)

	c := cbr.New(srv.Client(), srv.URL)
	_, err := c.CurrencyIDs(context.Background())
	if err == nil {
		t.Fatal("CurrencyIDs: want error on HTTP 500, got nil")
	}
}
