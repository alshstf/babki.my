package portfolio_test

import (
	"fmt"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// A POSITION'S INCOME CAN ARRIVE IN SEVERAL CURRENCIES AT ONCE, and this file
// is what the screen's two income figures are pinned by. A dollar share bought
// on a Russian broker pays its dividend in dollars one quarter and in rubles
// the next, and the tax withheld comes in rubles either way — see
// engine_income_currency_test.go for why the engine now folds that instead of
// refusing it.
//
// The two figures answer two different questions and this file states both:
//
//   - Position.income_minor is the income denominated in the position's OWN
//     currency, and only that. It is one int64 published beside `currency`, so
//     a sum containing another currency's minor units could not be labelled
//     honestly — and a partial figure under a true label beats a complete one
//     under a false label.
//   - PositionInBase.income_minor is the WHOLE income, every payment converted
//     out of the currency it actually arrived in, at the rate of the day it
//     arrived. Nothing is missing from it, and no term is converted at a rate
//     belonging to a currency it is not in.
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

	// buy 10 @ 100.00 USD                      cost   100 000 cents
	// dividend      5 000 cents (USD)          income   5 000 cents
	// dividend    300 000 kopecks (RUB)        income 300 000 kopecks
	// tax         -39 000 kopecks (RUB)        income -39 000 kopecks
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
	// 5 000 and not 266 000: the ruble payments are not this field's to carry,
	// at par or at any rate. They are not converted into it either — this
	// object publishes no rate and converting here would invent one.
	if p.IncomeMinor != 5000 {
		t.Errorf("income_minor = %d, want 5000 — the dollar income alone, under the dollar sign this row carries", p.IncomeMinor)
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

// TestInBaseGoesNullWhenAnIncomeCurrencyHasNoRate. The rule that a value is
// never published half-struck does not weaken because the missing term is in a
// currency of its own: a base-currency income summed from only the payments
// that happened to convert is smaller than the truth and looks exactly like an
// ordinary figure on screen. The whole object goes, and the gap names the term
// that stopped it.
//
// The dollar rate IS seeded here, so the position's own currency converts
// perfectly and the only unconvertible term is the euro tax. A server that
// converted every payment from the position's currency would find a rate for
// all of them and publish a number.
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
}
