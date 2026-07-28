package account_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// summaryResponse mirrors apitypes.Summary for decoding in tests.
type summaryResponse struct {
	Totals []struct {
		Currency         string `json:"currency"`
		AssetsMinor      int64  `json:"assets_minor"`
		LiabilitiesMinor int64  `json:"liabilities_minor"`
		NetMinor         int64  `json:"net_minor"`
	} `json:"totals"`
	BaseCurrency     string    `json:"base_currency"`
	TotalInBaseMinor *int64    `json:"total_in_base_minor"`
	Unconverted      *[]string `json:"unconverted"`
}

func TestSummaryEndpoint(t *testing.T) {
	url, c := newAPI(t)

	mk := func(body string) string {
		resp := do(t, c, "POST", url+"/api/v1/accounts", body)
		if resp.StatusCode != 201 {
			t.Fatalf("create: %d", resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		return a.ID
	}
	id1 := mk(`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	id2 := mk(`{"name":"Кредитка","type":"credit_card","currency":"RUB"}`)
	do(t, c, "PUT", url+"/api/v1/accounts/"+id1+"/balance", `{"as_of":"2026-07-20","amount_minor":100000}`)
	do(t, c, "PUT", url+"/api/v1/accounts/"+id2+"/balance", `{"as_of":"2026-07-20","amount_minor":-25000}`)

	resp := do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	if len(sum.Totals) != 1 || sum.Totals[0].NetMinor != 75000 ||
		sum.Totals[0].AssetsMinor != 100000 || sum.Totals[0].LiabilitiesMinor != -25000 {
		t.Fatalf("summary = %+v", sum)
	}

	// Single currency, already the space's default base currency (RUB) — no
	// fx lookup needed, so it must convert trivially: total == net, nothing
	// unconverted.
	if sum.BaseCurrency != "RUB" {
		t.Fatalf("base_currency = %q, want RUB", sum.BaseCurrency)
	}
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 75000 {
		t.Fatalf("total_in_base_minor = %v, want 75000", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want []", sum.Unconverted)
	}
}

// pastOn returns a date safely before "today" so Store.FxRateOn's
// nearest-earlier-date lookup always finds it regardless of when the test
// runs, without hardcoding a specific calendar date.
func pastOn() time.Time {
	return time.Now().UTC().AddDate(0, -1, 0).Truncate(24 * time.Hour)
}

// TestSummaryTotalInBaseCurrencyTwoCurrencies converts two non-base
// currencies into RUB and checks the sum against a manually computed
// expectation.
//
// Manual arithmetic (rates match converter_test.go's fixtures for an
// easy cross-check):
//
//	USD account: 100.00 USD (amount_minor 10000) * 90 RUB/USD  = 9000.00 RUB (900000 minor)
//	EUR account:  50.00 EUR (amount_minor  5000) * 100 RUB/EUR = 5000.00 RUB (500000 minor)
//	total_in_base_minor = 900000 + 500000 = 1400000 (14000.00 RUB)
func TestSummaryTotalInBaseCurrencyTwoCurrencies(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	mk := func(currency, body string) {
		resp := do(t, c, "POST", url+"/api/v1/accounts", body)
		if resp.StatusCode != 201 {
			t.Fatalf("create %s account: %d", currency, resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		if resp = do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
			`{"as_of":"2026-07-20","amount_minor":`+balanceFor(currency)+`}`); resp.StatusCode != 200 {
			t.Fatalf("set %s balance: %d", currency, resp.StatusCode)
		}
	}
	mk("USD", `{"name":"US cash","type":"cash","currency":"USD"}`)
	mk("EUR", `{"name":"EU cash","type":"cash","currency":"EUR"}`)

	resp := do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.BaseCurrency != "RUB" {
		t.Fatalf("base_currency = %q, want RUB", sum.BaseCurrency)
	}
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 1400000 {
		t.Fatalf("total_in_base_minor = %v, want 1400000", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want []", sum.Unconverted)
	}
}

// balanceFor returns the amount_minor used for each currency's account in
// TestSummaryTotalInBaseCurrencyTwoCurrencies, kept in one place so the
// values in the request bodies and the manual arithmetic in the doc comment
// can't silently drift apart.
func balanceFor(currency string) string {
	switch currency {
	case "USD":
		return "10000" // 100.00 USD
	case "EUR":
		return "5000" // 50.00 EUR
	default:
		return "0"
	}
}

// TestSummaryPartialConversionReportsUnconverted covers the "some currencies
// have a rate, one doesn't" case: total_in_base_minor must still be a
// number — the sum of whatever did convert — and the currency lacking a
// rate must show up in unconverted rather than failing the whole request.
func TestSummaryPartialConversionReportsUnconverted(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	// Only USD has a rate; KZT has none.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	mk := func(name, currency, amountMinor string) {
		resp := do(t, c, "POST", url+"/api/v1/accounts",
			`{"name":"`+name+`","type":"cash","currency":"`+currency+`"}`)
		if resp.StatusCode != 201 {
			t.Fatalf("create %s account: %d", currency, resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		if resp = do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
			`{"as_of":"2026-07-20","amount_minor":`+amountMinor+`}`); resp.StatusCode != 200 {
			t.Fatalf("set %s balance: %d", currency, resp.StatusCode)
		}
	}
	mk("US cash", "USD", "10000")  // 100.00 USD -> 9000.00 RUB (900000 minor)
	mk("KZT cash", "KZT", "50000") // no rate available at all

	resp := do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 900000 {
		t.Fatalf("total_in_base_minor = %v, want 900000 (USD leg only)", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 1 || (*sum.Unconverted)[0] != "KZT" {
		t.Fatalf("unconverted = %v, want [KZT]", sum.Unconverted)
	}
}

// TestSummaryNoRatesAtAllYieldsNullTotal covers the total absence of fx
// data: every currency lacks a rate, so total_in_base_minor must be null
// (not 0 — 0 would misleadingly claim a known zero net worth) and every
// currency present in totals must show up in unconverted.
func TestSummaryNoRatesAtAllYieldsNullTotal(t *testing.T) {
	url, c, _ := newAPIWithConverter(t)

	resp := do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"US cash","type":"cash","currency":"USD"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create account: %d", resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if resp = do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":10000}`); resp.StatusCode != 200 {
		t.Fatalf("set balance: %d", resp.StatusCode)
	}

	resp = do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.TotalInBaseMinor != nil {
		t.Fatalf("total_in_base_minor = %v, want null", *sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 1 || (*sum.Unconverted)[0] != "USD" {
		t.Fatalf("unconverted = %v, want [USD]", sum.Unconverted)
	}
}

// TestSummaryBaseCurrencyComesFromSpace proves base_currency in the response
// reflects the space's configured base currency rather than a hardcoded
// "RUB": after changing the space's base currency to USD, an empty space
// (no accounts at all) must report base_currency=USD, total_in_base_minor=0
// (there's nothing to fail to convert), and unconverted=[].
func TestSummaryBaseCurrencyComesFromSpace(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "PATCH", url+"/api/v1/space", `{"base_currency":"USD"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch space base_currency: %d", resp.StatusCode)
	}

	resp = do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.BaseCurrency != "USD" {
		t.Fatalf("base_currency = %q, want USD", sum.BaseCurrency)
	}
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 0 {
		t.Fatalf("total_in_base_minor = %v, want 0 (no accounts at all)", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want []", sum.Unconverted)
	}
}
