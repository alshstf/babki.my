package account_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
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
	RatesOn          *string   `json:"rates_on"`
}

func TestSummaryEndpoint(t *testing.T) {
	url, c := newAPI(t)

	mk := func(body string) string {
		resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts", body)
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
	apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+id1+"/balance", `{"as_of":"2026-07-20","amount_minor":100000}`)
	apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+id2+"/balance", `{"as_of":"2026-07-20","amount_minor":-25000}`)

	resp := apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	if len(sum.Totals) != 1 || sum.Totals[0].NetMinor != 75000 ||
		sum.Totals[0].AssetsMinor != 100000 || sum.Totals[0].LiabilitiesMinor != -25000 {
		t.Fatalf("summary = %+v", sum)
	}

	// Already in the base currency: no lookup, total equals net.
	if sum.BaseCurrency != "RUB" {
		t.Fatalf("base_currency = %q, want RUB", sum.BaseCurrency)
	}
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 75000 {
		t.Fatalf("total_in_base_minor = %v, want 75000", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want []", sum.Unconverted)
	}
	// Identity conversions resolve no rate, so rates_on is null.
	if sum.RatesOn != nil {
		t.Fatalf("rates_on = %v, want null (no cross-currency conversion happened)", *sum.RatesOn)
	}
}

// pastOn is a date safely before today, so the nearest-earlier lookup finds it.
func pastOn() time.Time {
	return time.Now().UTC().AddDate(0, -1, 0).Truncate(24 * time.Hour)
}

// Two non-base currencies converted into RUB:
//
//	USD 100.00 (10000) × 90  = 9000.00 RUB (900000)
//	EUR  50.00 (5000)  × 100 = 5000.00 RUB (500000)
//	total = 1400000
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
		resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts", body)
		if resp.StatusCode != 201 {
			t.Fatalf("create %s account: %d", currency, resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
			`{"as_of":"2026-07-20","amount_minor":`+balanceFor(currency)+`}`); resp.StatusCode != 200 {
			t.Fatalf("set %s balance: %d", currency, resp.StatusCode)
		}
	}
	mk("USD", `{"name":"US cash","type":"cash","currency":"USD"}`)
	mk("EUR", `{"name":"EU cash","type":"cash","currency":"EUR"}`)

	resp := apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
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
	// Both rates are dated on, so rates_on is that date.
	wantRatesOn := on.Format("2006-01-02")
	if sum.RatesOn == nil || *sum.RatesOn != wantRatesOn {
		t.Fatalf("rates_on = %v, want %q", sum.RatesOn, wantRatesOn)
	}
}

// balanceFor is each currency's balance, shared by the requests and the arithmetic.
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

// A currency without a rate goes to unconverted; the total is the sum of what
// converted.
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
		resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
			`{"name":"`+name+`","type":"cash","currency":"`+currency+`"}`)
		if resp.StatusCode != 201 {
			t.Fatalf("create %s account: %d", currency, resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
			`{"as_of":"2026-07-20","amount_minor":`+amountMinor+`}`); resp.StatusCode != 200 {
			t.Fatalf("set %s balance: %d", currency, resp.StatusCode)
		}
	}
	mk("US cash", "USD", "10000")  // 100.00 USD -> 9000.00 RUB (900000 minor)
	mk("KZT cash", "KZT", "50000") // no rate available at all

	resp := apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
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
	// KZT never converted, so only USD's rate date counts.
	wantRatesOn := on.Format("2006-01-02")
	if sum.RatesOn == nil || *sum.RatesOn != wantRatesOn {
		t.Fatalf("rates_on = %v, want %q (USD leg only)", sum.RatesOn, wantRatesOn)
	}
}

// With no rates at all the total is null, not 0, and every currency is
// unconverted.
func TestSummaryNoRatesAtAllYieldsNullTotal(t *testing.T) {
	url, c, _ := newAPIWithConverter(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"US cash","type":"cash","currency":"USD"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create account: %d", resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":10000}`); resp.StatusCode != 200 {
		t.Fatalf("set balance: %d", resp.StatusCode)
	}

	resp = apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
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
	// Nothing converted, so rates_on is null.
	if sum.RatesOn != nil {
		t.Fatalf("rates_on = %v, want null (nothing converted)", *sum.RatesOn)
	}
}

// base_currency follows the space's setting: an empty USD space reports USD,
// 0 and no unconverted currencies.
func TestSummaryBaseCurrencyComesFromSpace(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/space", `{"base_currency":"USD"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch space base_currency: %d", resp.StatusCode)
	}

	resp = apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
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
	// No accounts at all, so nothing to convert: rates_on must be null.
	if sum.RatesOn != nil {
		t.Fatalf("rates_on = %v, want null (empty space)", *sum.RatesOn)
	}
}

// A zero-balance currency needs no rate and never appears in unconverted: a
// new empty KZT account must not spoil the summary.
func TestSummaryZeroBalanceCurrencyIgnored(t *testing.T) {
	url, c, _ := newAPIWithConverter(t)

	// Create RUB account (base currency) with a non-zero balance
	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Base RUB","type":"cash","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create RUB account: %d", resp.StatusCode)
	}
	var rubAcc struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rubAcc)
	if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+rubAcc.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":100000}`); resp.StatusCode != 200 {
		t.Fatalf("set RUB balance: %d", resp.StatusCode)
	}

	// Create KZT account (NOT base currency, NO fx rate) with zero balance
	resp = apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Zero KZT","type":"cash","currency":"KZT"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create KZT account: %d", resp.StatusCode)
	}
	var kztAcc struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&kztAcc)
	// Note: not setting a balance for KZT account, so it has net_minor=0

	resp = apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.BaseCurrency != "RUB" {
		t.Fatalf("base_currency = %q, want RUB", sum.BaseCurrency)
	}
	// RUB only: 100000, not null.
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 100000 {
		t.Fatalf("total_in_base_minor = %v, want 100000 (RUB leg only)", sum.TotalInBaseMinor)
	}
	// unconverted must be empty: KZT has zero balance and doesn't need conversion
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want []", sum.Unconverted)
	}
	// Totals must still include both currencies
	if len(sum.Totals) != 2 {
		t.Fatalf("totals has %d currencies, want 2", len(sum.Totals))
	}
	// Only identity conversions, so rates_on is null.
	if sum.RatesOn != nil {
		t.Fatalf("rates_on = %v, want null (only identity RUB->RUB was converted)", *sum.RatesOn)
	}
}

// rates_on discloses a stale rate: a USD rate two days old surfaces as that
// date, not today.
func TestSummaryRatesOnReflectsStaleRateNotToday(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	twoDaysAgo := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour)

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: twoDaysAgo, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"US cash","type":"cash","currency":"USD"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create USD account: %d", resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":10000}`); resp.StatusCode != 200 {
		t.Fatalf("set balance: %d", resp.StatusCode)
	}

	resp = apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 900000 {
		t.Fatalf("total_in_base_minor = %v, want 900000", sum.TotalInBaseMinor)
	}
	wantRatesOn := twoDaysAgo.Format("2006-01-02")
	todayStr := time.Now().UTC().Format("2006-01-02")
	if sum.RatesOn == nil || *sum.RatesOn != wantRatesOn {
		t.Fatalf("rates_on = %v, want %q (the seeded rate's own date)", sum.RatesOn, wantRatesOn)
	}
	if sum.RatesOn != nil && *sum.RatesOn == todayStr {
		t.Fatalf("rates_on = %q, must not be today's date (%q) for a two-day-old rate", *sum.RatesOn, todayStr)
	}
}

// Conversion through the inverse rate: base USD, a RUB balance, and only a
// USD->RUB row (78.50). 785000 / 78.50 = 10000 exactly. A broken inverse would
// leave the total null.
func TestSummaryConvertsViaInverseRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	// Only USD->RUB is seeded.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("78.50"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/space", `{"base_currency":"USD"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch space base_currency: %d", resp.StatusCode)
	}

	resp = apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"RUB cash","type":"cash","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create RUB account: %d", resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	if resp = apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":785000}`); resp.StatusCode != 200 {
		t.Fatalf("set RUB balance: %d", resp.StatusCode)
	}

	resp = apitest.Do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum summaryResponse
	_ = json.NewDecoder(resp.Body).Decode(&sum)

	if sum.BaseCurrency != "USD" {
		t.Fatalf("base_currency = %q, want USD", sum.BaseCurrency)
	}
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 10000 {
		t.Fatalf("total_in_base_minor = %v, want 10000 (785000 RUB minor / 78.50 via inverse rate = 100.00 USD)", sum.TotalInBaseMinor)
	}
	if sum.Unconverted == nil || len(*sum.Unconverted) != 0 {
		t.Fatalf("unconverted = %v, want [] (inverse rate must resolve the RUB->USD conversion)", sum.Unconverted)
	}
	wantRatesOn := on.Format("2006-01-02")
	if sum.RatesOn == nil || *sum.RatesOn != wantRatesOn {
		t.Fatalf("rates_on = %v, want %q", sum.RatesOn, wantRatesOn)
	}
}
