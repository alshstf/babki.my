package portfolio_test

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// Income arriving in several currencies, and the screen's three figures for
// it: income_minor is only the position's own currency; income_by_currency is
// the whole income unconverted, by currency; in_base.income_minor is the whole
// income converted, each payment from its own currency at its own day.
func TestIncomeArrivingInSeveralCurrencies(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	// The space's base currency is RUB (setupAPI's /api/v1/setup call).
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	// 	buy 10 @ 100.00 USD            cost   100 000 cents
	// 	dividend  5 000 cents (USD)
	// 	dividend 300 000 kopecks (RUB)
	// 	tax      −39 000 kopecks (RUB)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-05","amount_minor":5000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-06","amount_minor":300000,"currency":"RUB"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"tax",
		"occurred_on":"2026-07-06","amount_minor":-39000,"currency":"RUB"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", got.Positions)
	}
	p := got.Positions[0]

	if p.Currency != "USD" {
		t.Fatalf("currency = %q, want USD — the currency the shares were paid for", p.Currency)
	}
	// 5 000, not 266 000: rouble payments are not this field's, at any rate.
	if p.IncomeMinor != 5000 {
		t.Errorf("income_minor = %d, want 5000 — the dollar income alone, under the dollar sign this row carries", p.IncomeMinor)
	}
	// RUB before USD (by code); the rouble entry is net of the rouble tax; the USD
	// entry equals income_minor.
	wantIncome := []currencyIncome{
		{Currency: "RUB", IncomeMinor: 261000},
		{Currency: "USD", IncomeMinor: 5000},
	}
	if !slices.Equal(p.IncomeByCurrency, wantIncome) {
		t.Errorf("income_by_currency = %+v, want %+v (300 000 − 39 000 kopecks, and 5 000 cents)",
			p.IncomeByCurrency, wantIncome)
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil (gap %v), want the converted object", p.InBaseGap)
	}
	if p.InBase.CostMinor != 9000000 {
		t.Errorf("in_base.cost_minor = %d, want 9000000 (100 000 ¢ at 90)", p.InBase.CostMinor)
	}
	// 5 000 ¢ at 90 = 450 000 kopecks, plus 300 000 kopecks and minus 39 000
	// kopecks that are already rubles and need no rate at all.
	if p.InBase.IncomeMinor == 23940000 {
		t.Fatalf("in_base.income_minor = %d — every payment was converted at the DOLLAR rate, rubles included", p.InBase.IncomeMinor)
	}
	if p.InBase.IncomeMinor != 711000 {
		t.Errorf("in_base.income_minor = %d, want 711000 (450 000 + 300 000 − 39 000)", p.InBase.IncomeMinor)
	}
}

// A payment in a currency without a rate nulls the whole in_base, naming the
// term; the USD rate is seeded, so converting everything from USD would wrongly
// publish a number.
func TestInBaseGoesNullWhenAnIncomeCurrencyHasNoRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"tax",
		"occurred_on":"2026-07-06","amount_minor":-1000,"currency":"EUR"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", got.Positions)
	}
	p := got.Positions[0]

	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the euro tax has no rate and the income sum is a term short", p.InBase)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_income_date" {
		t.Errorf("in_base_gap = %v, want no_rate_income_date", p.InBaseGap)
	}
	// The position's own figures are untouched by the gap, as always.
	if p.CostMinor != 100000 {
		t.Errorf("cost_minor = %d, want 100000", p.CostMinor)
	}
	if p.IncomeMinor != 0 {
		t.Errorf("income_minor = %d, want 0 — the only payment was in euros, and this field is the dollar one", p.IncomeMinor)
	}
	// income_minor's zero is not the whole answer: the list carries the euro
	// tax.
	wantIncome := []currencyIncome{{Currency: "EUR", IncomeMinor: -1000}}
	if !slices.Equal(p.IncomeByCurrency, wantIncome) {
		t.Errorf("income_by_currency = %+v, want %+v — the euro tax, which income_minor's 0 says nothing about",
			p.IncomeByCurrency, wantIncome)
	}
}

// An empty list (never paid) is published as [], distinct from a zero entry.
// The raw JSON is checked, since null and [] decode alike.
func TestIncomeByCurrencyIsEmptyWhenNothingWasPaid(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var raw struct {
		Positions []struct {
			IncomeMinor      int64           `json:"income_minor"`
			IncomeByCurrency json.RawMessage `json:"income_by_currency"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(raw.Positions) != 1 {
		t.Fatalf("positions = %d, want exactly 1", len(raw.Positions))
	}
	if got := string(raw.Positions[0].IncomeByCurrency); got != "[]" {
		t.Errorf("income_by_currency = %s, want [] — an empty array, since a null would leave a reader to guess", got)
	}
	if raw.Positions[0].IncomeMinor != 0 {
		t.Errorf("income_minor = %d, want 0", raw.Positions[0].IncomeMinor)
	}
}
