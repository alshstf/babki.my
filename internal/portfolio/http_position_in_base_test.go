package portfolio_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/testdb"
)

// fxSeedOn is a date before every fixture date, so one seeded rate covers lot
// dates, income dates and today alike. (pastOn() would fall after the July
// fixtures once the clock passes August.)
func fxSeedOn(t *testing.T) time.Time {
	t.Helper()
	return mustDate(t, "2026-01-01")
}

// A non-base position with a rate gets the four held-side figures in the base
// currency. One rate covers every date, so this pins the plumbing; the
// per-date semantics are pinned by the two-rate tests below.
//
//	buy 10 @ 100.00 USD          cost   100_000
//	dividend                     income   5_000
//	quote 120.00 × 10            value  120_000, unrealized 20_000
//	USD->RUB 90: cost 9_000_000, income 450_000, value 10_800_000,
//	             unrealized 1_800_000 — all distinct.
func TestPositionInBaseConvertsHeldSideValues(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	on := fxSeedOn(t)
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	// The space's base is RUB; the account is USD.
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-05","amount_minor":5000,"currency":"USD"}`, acc.ID, share.ID))

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

	// The native figures first.
	if p.CostMinor != 100000 {
		t.Fatalf("cost_minor = %d, want 100000", p.CostMinor)
	}
	if p.IncomeMinor != 5000 {
		t.Fatalf("income_minor = %d, want 5000", p.IncomeMinor)
	}
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 120000 {
		t.Fatalf("market_value_minor = %v, want 120000", p.MarketValueMinor)
	}
	if p.UnrealizedPnlMinor == nil || *p.UnrealizedPnlMinor != 20000 {
		t.Fatalf("unrealized_pnl_minor = %v, want 20000", p.UnrealizedPnlMinor)
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.CostMinor != 9000000 {
		t.Errorf("in_base.cost_minor = %d, want 9000000", p.InBase.CostMinor)
	}
	if p.InBase.IncomeMinor != 450000 {
		t.Errorf("in_base.income_minor = %d, want 450000", p.InBase.IncomeMinor)
	}
	if p.InBase.MarketValueMinor == nil || *p.InBase.MarketValueMinor != 10800000 {
		t.Errorf("in_base.market_value_minor = %v, want 10800000", p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor == nil || *p.InBase.UnrealizedPnlMinor != 1800000 {
		t.Errorf("in_base.unrealized_pnl_minor = %v, want 1800000", p.InBase.UnrealizedPnlMinor)
	}
	if p.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB", p.InBase.Currency)
	}
	wantRateOn := on.Format("2006-01-02")
	if p.InBase.RateOn == nil || *p.InBase.RateOn != wantRateOn {
		t.Errorf("in_base.rate_on = %v, want %q", p.InBase.RateOn, wantRateOn)
	}
}

// A position in the base currency has no in_base.
func TestPositionInBaseNullWhenAlreadyBaseCurrency(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, share.ID))

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
	if got.Positions[0].InBase != nil {
		t.Fatalf("in_base = %+v, want null (position already in base currency)", got.Positions[0].InBase)
	}
}

// Without a rate in_base is null as a whole, and the request still succeeds.
func TestPositionInBaseNullWhenNoRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// No GBP rate at all.

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"GBP"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"GBP"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"GBP"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d, want 200 (missing rate must not fail the request): %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", got.Positions)
	}
	if got.Positions[0].InBase != nil {
		t.Fatalf("in_base = %+v, want null (no fx rate for GBP->RUB)", got.Positions[0].InBase)
	}
}

// Without a quote in_base still carries cost and income; its valuation and
// unrealized profit are null, as the position's own are.
func TestPositionInBaseNullMarketValueAndUnrealizedPnlWithoutQuote(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	// No quotes seeded for any instrument.
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"USD"}`)

	// 	buy 5 @ 10.00 USD -> cost 5_000;  dividend 200
	// 	  cost 5_000 × 90 = 450_000; income 200 × 90 = 18_000
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"10",
		"amount_minor":-5000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-05","amount_minor":200,"currency":"USD"}`, acc.ID, share.ID))

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

	if p.MarketValueMinor != nil || p.UnrealizedPnlMinor != nil {
		t.Fatalf("top-level market_value_minor/unrealized_pnl_minor = %v/%v, want both null (no quote)",
			p.MarketValueMinor, p.UnrealizedPnlMinor)
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a non-null object (cost_minor/income_minor are still convertible)")
	}
	if p.InBase.CostMinor != 450000 {
		t.Errorf("in_base.cost_minor = %d, want 450000", p.InBase.CostMinor)
	}
	if p.InBase.IncomeMinor != 18000 {
		t.Errorf("in_base.income_minor = %d, want 18000", p.InBase.IncomeMinor)
	}
	if p.InBase.MarketValueMinor != nil {
		t.Errorf("in_base.market_value_minor = %v, want null (no quote)", p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor != nil {
		t.Errorf("in_base.unrealized_pnl_minor = %v, want null (no quote)", p.InBase.UnrealizedPnlMinor)
	}
	if p.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB", p.InBase.Currency)
	}
}

// Without a valuation rate_on is null (#52): it dates the valuation and
// nothing else. The basis and income stand.
//
//	USD->RUB 90; buy 5 @ $10.00 -> 450_000; dividend $2.00 -> 18_000; no quote
func TestPositionInBaseRateOnNullWithoutAValuation(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	// No quotes seeded for any instrument — the point of the fixture.
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"10",
		"amount_minor":-5000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-05","amount_minor":200,"currency":"USD"}`, acc.ID, share.ID))

	p := onlyPosition(t, c, url, acc.ID)

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: the basis and the income are convertible whether or not a quote exists")
	}
	if p.InBase.MarketValueMinor != nil {
		t.Fatalf("in_base.market_value_minor = %d, want null — the fixture seeds no quote, so this test is not testing what it means to", *p.InBase.MarketValueMinor)
	}
	if p.InBase.RateOn != nil {
		t.Errorf("in_base.rate_on = %q, want null: the contract defines it as the date of the rate behind market_value_minor, and there is no market_value_minor here — a date published here names the day of a value the object does not contain", *p.InBase.RateOn)
	}
	// The basis and income are unaffected.
	if p.InBase.CostMinor != 450000 {
		t.Errorf("in_base.cost_minor = %d, want 450000 (5000 * 90) — a null rate_on must take nothing else with it", p.InBase.CostMinor)
	}
	if p.InBase.IncomeMinor != 18000 {
		t.Errorf("in_base.income_minor = %d, want 18000 (200 * 90) — a null rate_on must take nothing else with it", p.InBase.IncomeMinor)
	}
}

// Today's rate is needed only by the valuation: without a quote, in_base is
// published even when today has no rate (#52). oneDateConverter fails only
// today's date, a hole no real converter has.
//
//	buy 10 @ $100.00 on 2026-03-10; every date at 90 except today; no quote
//	in_base.cost_minor = 9_000_000, rate_on = null
func TestPositionInBasePublishedWithoutTodaysRateWhenThereIsNoQuote(t *testing.T) {
	pool := testdb.New(t)
	// No quotes seeded: the position has nothing for today's rate to value.
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, oneDateConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		on:     time.Now().UTC().Format("2006-01-02"),
		err:    fmt.Errorf("%w: USD -> RUB today", marketdata.ErrNoRate),
	})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: the only rate missing is today's, and this position has no valuation for today's rate to convert — refusing the basis over it is silence about something the server knows")
	}
	if p.InBase.CostMinor != 9000000 {
		t.Errorf("in_base.cost_minor = %d, want 9000000 (100000 * 90, at the rate of the purchase day)", p.InBase.CostMinor)
	}
	if p.InBase.RateOn != nil {
		t.Errorf("in_base.rate_on = %q, want null: there is no valuation here, and no rate for today was resolved to name", *p.InBase.RateOn)
	}
}

// A valuation that never reached the position's currency (no EUR rate) has no
// base figure: value and unrealized are null while cost and income convert.
// Multiplying the EUR amount by the USD rate would be a silently wrong number.
//
//	USD position; bond face 1 000 EUR at par; only USD->RUB = 90
//	  valuation 100_000 EUR, published raw; cost 9_000_000
func TestPositionInBaseNullMarketValueWhenValuationInForeignCurrency(t *testing.T) {
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
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Еврооблигация","ticker":"EUB","currency":"USD","face_value_minor":100000,"face_currency":"EUR"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID))

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

	// The valuation really is published raw in EUR.
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 100000 {
		t.Fatalf("market_value_minor = %v, want 100000 (raw, unconverted)", p.MarketValueMinor)
	}
	if p.MarketValueCurrency == nil || *p.MarketValueCurrency != "EUR" {
		t.Fatalf("market_value_currency = %v, want EUR (face_currency, no EUR->USD rate)", p.MarketValueCurrency)
	}
	if p.UnrealizedPnlMinor != nil {
		t.Fatalf("unrealized_pnl_minor = %v, want null (valuation currency differs)", p.UnrealizedPnlMinor)
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a non-null object (cost_minor/income_minor are still convertible)")
	}
	if p.InBase.CostMinor != 9000000 {
		t.Errorf("in_base.cost_minor = %d, want 9000000 (100000 USD minor * 90)", p.InBase.CostMinor)
	}
	if p.InBase.IncomeMinor != 0 {
		t.Errorf("in_base.income_minor = %d, want 0 (no income operations)", p.InBase.IncomeMinor)
	}
	if p.InBase.MarketValueMinor != nil {
		t.Errorf("in_base.market_value_minor = %v, want null: the valuation is in EUR, and %d RUB is that EUR amount times the USD rate — a silently wrong number",
			*p.InBase.MarketValueMinor, *p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor != nil {
		t.Errorf("in_base.unrealized_pnl_minor = %s, want null (derived from a valuation that cannot be converted)", formatMinor(p.InBase.UnrealizedPnlMinor))
	}
	// rate_on goes null with the valuation.
	if p.InBase.RateOn != nil {
		t.Errorf("in_base.rate_on = %q, want null: market_value_minor is null, so no rate stands behind anything in this object", *p.InBase.RateOn)
	}
	if p.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB", p.InBase.Currency)
	}
}

// A valuation in a third currency converts once, from its own currency, to
// the base (#39); rate_on names the EUR row. A plain share on the same account
// is unaffected.
//
//	USD->RUB 90 (2026-01-01), EUR->RUB 100 (2026-02-01); EUR->USD bridges
//	bond: face 1 000 EUR at par, bought for 900 USD
//	  source 100_000 EUR; native 111_111 USD
//	  in_base value 10_000_000 (the chain gave 9_999_990), cost 8_100_000,
//	  unrealized 1_900_000, rate_on 2026-02-01
//	share: 10 @ 100 USD, quoted 110: value 9_900_000, cost 9_000_000,
//	  rate_on 2026-01-01
func TestPositionInBaseValuationIsConvertedOnceFromItsOwnCurrency(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: mustDate(t, "2026-02-01"), Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Еврооблигация","ticker":"EUB","currency":"USD","face_value_minor":100000,"face_currency":"EUR"}`)
	share := createInstrument(t, c, url,
		`{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("110.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"1","price":"900",
		"amount_minor":-90000,"currency":"USD"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	byID := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		byID[p.Instrument.Id] = p
	}

	bondPos, ok := byID[bond.ID]
	if !ok {
		t.Fatalf("no position for the bond: %+v", got.Positions)
	}
	// The valuation is in a third currency and did reach USD.
	if bondPos.MarketValueSourceCurrency == nil || *bondPos.MarketValueSourceCurrency != "EUR" {
		t.Fatalf("bond market_value_source_currency = %s, want EUR (the face currency the valuation came out in)", formatText(bondPos.MarketValueSourceCurrency))
	}
	if bondPos.MarketValueSourceMinor == nil || *bondPos.MarketValueSourceMinor != 100000 {
		t.Fatalf("bond market_value_source_minor = %s, want 100000 (1 000,00 EUR of face, quoted at par)", formatMinor(bondPos.MarketValueSourceMinor))
	}
	// The native figure is still the USD conversion.
	if bondPos.MarketValueMinor == nil || *bondPos.MarketValueMinor != 111111 {
		t.Errorf("bond market_value_minor = %s, want 111111 (100000 EUR at the bridged EUR->USD rate) — the position-currency figure must not move",
			formatMinor(bondPos.MarketValueMinor))
	}
	if bondPos.InBase == nil {
		t.Fatalf("bond in_base = nil, want the object: every rate this position needs is seeded")
	}
	if bondPos.InBase.CostMinor != 8100000 {
		t.Errorf("bond in_base.cost_minor = %d, want 8100000 (90000 USD at 90)", bondPos.InBase.CostMinor)
	}
	if bondPos.InBase.MarketValueMinor == nil || *bondPos.InBase.MarketValueMinor != 10000000 {
		t.Errorf("bond in_base.market_value_minor = %s, want 10000000 (100000 EUR at EUR->RUB 100, one multiplication) — 9999990 is the same money sent through the dollar first and rounded on the way",
			formatMinor(bondPos.InBase.MarketValueMinor))
	}
	if bondPos.InBase.UnrealizedPnlMinor == nil || *bondPos.InBase.UnrealizedPnlMinor != 1900000 {
		t.Errorf("bond in_base.unrealized_pnl_minor = %s, want 1900000 (10000000 - 8100000) — it must be the difference of the two figures published beside it",
			formatMinor(bondPos.InBase.UnrealizedPnlMinor))
	}
	if bondPos.InBase.RateOn == nil || *bondPos.InBase.RateOn != "2026-02-01" {
		t.Errorf("bond in_base.rate_on = %s, want 2026-02-01 (the EUR->RUB row, the rate actually behind the figure) — 2026-01-01 is the USD->RUB row, which no longer has anything to do with this valuation",
			formatText(bondPos.InBase.RateOn))
	}

	sharePos, ok := byID[share.ID]
	if !ok {
		t.Fatalf("no position for the share: %+v", got.Positions)
	}
	if sharePos.MarketValueSourceCurrency != nil || sharePos.MarketValueSourceMinor != nil {
		t.Fatalf("share market_value_source_currency/_minor = %s/%s, want both null: the quote is already in the position's currency, so nothing was converted",
			formatText(sharePos.MarketValueSourceCurrency), formatMinor(sharePos.MarketValueSourceMinor))
	}
	if sharePos.InBase == nil {
		t.Fatalf("share in_base = nil, want the object")
	}
	if sharePos.InBase.MarketValueMinor == nil || *sharePos.InBase.MarketValueMinor != 9900000 {
		t.Errorf("share in_base.market_value_minor = %s, want 9900000 (110000 USD at 90) — a position with no source valuation converts exactly as it always did",
			formatMinor(sharePos.InBase.MarketValueMinor))
	}
	if sharePos.InBase.CostMinor != 9000000 {
		t.Errorf("share in_base.cost_minor = %d, want 9000000 (100000 USD at 90)", sharePos.InBase.CostMinor)
	}
	if sharePos.InBase.RateOn == nil || *sharePos.InBase.RateOn != "2026-01-01" {
		t.Errorf("share in_base.rate_on = %s, want 2026-01-01 (the USD->RUB row): the two rows of this one account name different rate dates because their valuations really are struck at different rates",
			formatText(sharePos.InBase.RateOn))
	}
}

// A valuation already in the base currency (a rouble OFZ in a dollar account)
// is published as the original roubles, with a null rate_on: no rate was used.
//
//	USD->RUB 90; bond face 1 000 RUB at par, bought for 1 000 USD
//	  source 100_000 RUB; native round(100_000/90) = 1_111 USD
//	  in_base value 100_000 exactly (the round trip gave 99_990);
//	  cost 9_000_000
func TestPositionInBaseValuationAlreadyInBaseCurrencyIsNotConverted(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"ОФЗ","ticker":"OFZ","currency":"USD","face_value_minor":100000,"face_currency":"RUB"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-01","quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueSourceCurrency == nil || *p.MarketValueSourceCurrency != "RUB" {
		t.Fatalf("market_value_source_currency = %s, want RUB (the face currency, which here IS the base currency)", formatText(p.MarketValueSourceCurrency))
	}
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 1111 {
		t.Fatalf("market_value_minor = %s, want 1111 (100000 RUB at RUB->USD 1/90)", formatMinor(p.MarketValueMinor))
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object")
	}
	if p.InBase.MarketValueMinor == nil || *p.InBase.MarketValueMinor != 100000 {
		t.Errorf("in_base.market_value_minor = %s, want 100000 — the valuation was already in rubles, so it is the answer; 99990 is that answer sent to the dollar and back",
			formatMinor(p.InBase.MarketValueMinor))
	}
	if p.InBase.UnrealizedPnlMinor == nil || *p.InBase.UnrealizedPnlMinor != -8900000 {
		t.Errorf("in_base.unrealized_pnl_minor = %s, want -8900000 (100000 - 9000000)", formatMinor(p.InBase.UnrealizedPnlMinor))
	}
	if p.InBase.RateOn != nil {
		t.Errorf("in_base.rate_on = %q, want null: no rate was applied to this valuation, and a date here would name one that had no part in the figure", *p.InBase.RateOn)
	}
}

// Dates for the historical-basis fixtures: rates on earlyRateOn and
// lateRateOn; earlyBuyOn falls between them, lateBuyOn and today after the
// second, so today's rate is the late one.
const (
	earlyRateOn = "2026-02-01"
	lateRateOn  = "2026-07-01"
	earlyBuyOn  = "2026-03-10"
	lateBuyOn   = "2026-07-10"
)

// datedRate is one USD->RUB row: its day and rate.
type datedRate struct{ on, rate string }

// fxRateAPI wires a RUB space whose fx table holds exactly the given USD->RUB
// rows; today resolves to the newest.
func fxRateAPI(t *testing.T, quotes quoteStoreLike, rates ...datedRate) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	rows := make([]marketdata.FxRate, 0, len(rates))
	for _, r := range rates {
		rows = append(rows, marketdata.FxRate{
			Base: "USD", Quote: "RUB", On: mustDate(t, r.on),
			Rate: decimal.RequireFromString(r.rate), Source: "test",
		})
	}
	if err := mdStore.UpsertFxRates(t.Context(), rows); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}
	return url, c
}

// twoRateAPI is fxRateAPI with an early and a late rate.
func twoRateAPI(t *testing.T, quotes quoteStoreLike, early, late string) (string, *http.Client) {
	t.Helper()
	return fxRateAPI(t, quotes, datedRate{earlyRateOn, early}, datedRate{lateRateOn, late})
}

// The base basis sums the held lots, each at its purchase day's rate — not the
// total at today's rate, which would cancel the currency's move out of the
// profit.
//
//	USD->RUB 60 from 2026-02-01, 90 from 2026-07-01
//	buy 10 @ 100 on 2026-03-10 -> 100_000 × 60 =  6_000_000
//	buy 10 @ 200 on 2026-07-10 -> 200_000 × 90 = 18_000_000
//	in_base.cost_minor = 24_000_000 (today's rate would give 27_000_000)
func TestPositionInBaseCostUsesEachLotsOwnRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)

	// Pin the fixture the arithmetic rests on.
	if p.CostMinor != 300000 {
		t.Fatalf("cost_minor = %d, want 300000 (100000 + 200000, in USD)", p.CostMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.CostMinor == 27000000 {
		t.Fatalf("in_base.cost_minor = 27000000 — that is cost_minor times TODAY's rate (300000 * 90), the semantics this change replaces; each lot must be valued at the rate of its own purchase date")
	}
	if p.InBase.CostMinor != 24000000 {
		t.Errorf("in_base.cost_minor = %d, want 24000000 (100000*60 bought %s + 200000*90 bought %s)",
			p.InBase.CostMinor, earlyBuyOn, lateBuyOn)
	}
	// No quote, so no valuation and no rate_on: the basis uses one rate per
	// purchase day.
	if p.InBase.RateOn != nil {
		t.Errorf("in_base.rate_on = %q, want null: this position has no quote, so the object holds no market valuation for a single rate to be behind", *p.InBase.RateOn)
	}
}

// transferOn falls after lateRateOn, so the transfer day's rate differs from
// the earlier purchase's.
const transferOn = "2026-07-20"

func createTransfer(t *testing.T, c *http.Client, url, body string) {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create transfer = %d: %s", resp.StatusCode, b)
	}
}

// Transferred lots keep their purchase dates: the destination's base basis
// equals what the source's was.
//
//	USD->RUB 60 from 2026-02-01, 90 from 2026-07-01
//	source buys 10 @ 100 (2026-03-10) and 10 @ 200 (2026-07-10), transfers
//	all on 2026-07-20
//	destination in_base.cost_minor = 24_000_000; re-dating to the transfer
//	day would give 27_000_000
func TestPositionInBaseTransferredLotsKeepTheirPurchaseDates(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, from.ID, share.ID, lateBuyOn))

	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"20","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))

	// The source gave everything up.
	src := onlyPosition(t, c, url, from.ID)
	if src.Quantity != "0" || src.CostMinor != 0 {
		t.Fatalf("source position after transferring everything = {qty %q cost %d}, want {\"0\" 0}",
			src.Quantity, src.CostMinor)
	}

	p := onlyPosition(t, c, url, to.ID)
	if p.Quantity != "20" {
		t.Fatalf("destination quantity = %q, want \"20\"", p.Quantity)
	}
	if p.CostMinor != 300000 {
		t.Fatalf("destination cost_minor = %d, want 300000 (100000 + 200000, in USD — a transfer moves the basis, it does not change it)", p.CostMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.CostMinor == 27000000 {
		t.Fatalf("in_base.cost_minor = 27000000 — that is the whole arrival valued at the rate of the TRANSFER day %s (300000 * 90); moving shares between the family's own accounts must not reprice them, so each lot keeps the rate of the day it was bought",
			transferOn)
	}
	if p.InBase.CostMinor != 24000000 {
		t.Errorf("in_base.cost_minor = %d, want 24000000 (100000*60 bought %s + 200000*90 bought %s)",
			p.InBase.CostMinor, earlyBuyOn, lateBuyOn)
	}
}

// A lot with no acquisition date (a transfer from before breakdowns) nulls
// the whole in_base. Neither the transfer-day figure (27_000_000) nor zero may
// appear.
func TestPositionInBaseNullWhenALotHasNoAcquisitionDate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("60"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, from.ID, share.ID, lateBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"20","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))

	// Make the transfer a pre-breakdown one: basis kept, dates gone.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	p := onlyPosition(t, c, url, to.ID)
	// The native figures are unaffected.
	if p.Quantity != "20" || p.CostMinor != 300000 {
		t.Fatalf("destination position = {qty %q cost %d}, want {\"20\" 300000} — an unknown acquisition date must not change the basis",
			p.Quantity, p.CostMinor)
	}
	if p.InBase == nil {
		return
	}
	if p.InBase.CostMinor == 27000000 {
		t.Fatalf("in_base.cost_minor = 27000000 — the arrival was valued at the rate of the TRANSFER day %s (300000 * 90). Nobody recorded that day as a purchase date; the lot's date is unknown and no rate answers for it",
			transferOn)
	}
	if p.InBase.CostMinor == 0 {
		t.Fatalf("in_base.cost_minor = 0 — the undatable lot was skipped and the rest summed; a basis missing a lot reads like a real, smaller basis")
	}
	t.Fatalf("in_base = %+v, want null: one lot does not know when it was acquired, so the whole object is unpublishable", *p.InBase)
}

// An undated lot beside a dated one still nulls in_base: skipping it would
// print the dated lot's cost alone, a plausible wrong basis.
//
//	transfer of 20 (breakdown dropped): undated lot, cost 300_000
//	own buy 3 @ $40 on 2026-07-25: dated lot, cost 12_000 (rate 90)
//	"skip" would give 1_080_000; "transfer day" 28_080_000
func TestPositionInBaseNullWhenOneOfSeveralLotsHasNoAcquisitionDate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("60"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, from.ID, share.ID, lateBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"20","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))

	// Drop the breakdown, as above.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	// A dated lot of the account's own beside the undated one.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-25","quantity":"3","price":"40",
		"amount_minor":-12000,"currency":"USD"}`, to.ID, share.ID))

	p := onlyPosition(t, c, url, to.ID)
	// The native figures are unaffected.
	if p.Quantity != "23" || p.CostMinor != 312000 {
		t.Fatalf("destination position = {qty %q cost %d}, want {\"23\" 312000} — an unknown acquisition date must not change the basis",
			p.Quantity, p.CostMinor)
	}
	if p.InBase == nil {
		return
	}
	if p.InBase.CostMinor == 1_080_000 {
		t.Fatalf("in_base.cost_minor = 1080000 — that is only the DATED lot converted (12000 * 90); the undated lot was silently skipped instead of nulling the whole object, and a partial basis reads exactly like a smaller real one")
	}
	if p.InBase.CostMinor == 28_080_000 {
		t.Fatalf("in_base.cost_minor = 28080000 — that prices the undated lot as if it were bought on the TRANSFER day (300000 * 90) and adds the real dated lot on top; nobody recorded a purchase date for that first lot")
	}
	t.Fatalf("in_base = %+v, want null: one of the two lots held here does not know when it was acquired, so the whole object is unpublishable", *p.InBase)
}

// Base unrealized profit is value in roubles minus the historical rouble
// basis, so it carries the currency's move.
//
//	cost 300_000 USD -> 24_000_000; quote 250 × 20 = 500_000 -> 45_000_000
//	unrealized USD 200_000; base 21_000_000 (USD profit × today = 18_000_000)
//	the extra 3_000_000 is the first lot's 60 -> 90
func TestPositionInBaseUnrealizedPnlIncludesCurrencyRevaluation(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("250.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)

	// Pin the position-currency figures the arithmetic rests on.
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 500000 {
		t.Fatalf("market_value_minor = %v, want 500000 (USD)", p.MarketValueMinor)
	}
	if p.UnrealizedPnlMinor == nil || *p.UnrealizedPnlMinor != 200000 {
		t.Fatalf("unrealized_pnl_minor = %v, want 200000 (USD)", p.UnrealizedPnlMinor)
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.MarketValueMinor == nil || *p.InBase.MarketValueMinor != 45000000 {
		t.Errorf("in_base.market_value_minor = %v, want 45000000 (500000 * 90, today's rate)", p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor == nil {
		t.Fatalf("in_base.unrealized_pnl_minor = nil, want 21000000")
	}
	if *p.InBase.UnrealizedPnlMinor == 18000000 {
		t.Fatalf("in_base.unrealized_pnl_minor = 18000000 — that is the USD profit times today's rate (200000 * 90), which is base-currency profit with the currency's own move cancelled out of it; it must be the ruble valuation minus the ruble basis")
	}
	if *p.InBase.UnrealizedPnlMinor != 21000000 {
		t.Errorf("in_base.unrealized_pnl_minor = %d, want 21000000 (45000000 - 24000000)", *p.InBase.UnrealizedPnlMinor)
	}
}

// A position can profit in its own currency and lose in the base one when the
// rouble strengthens; the signs must not be kept in step.
//
//	USD->RUB 100 then 50
//	buy 10 @ 100 -> cost 100_000 -> 10_000_000
//	quote 110 -> value 110_000 -> 5_500_000
//	unrealized +10_000 USD, −4_500_000 RUB
func TestPositionInBaseProfitInPositionCurrencyLossInBase(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "100", "50")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("110.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)

	if p.UnrealizedPnlMinor == nil || *p.UnrealizedPnlMinor != 10000 {
		t.Fatalf("unrealized_pnl_minor = %v, want +10000 (a profit in USD)", p.UnrealizedPnlMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.CostMinor != 10000000 {
		t.Errorf("in_base.cost_minor = %d, want 10000000 (100000 * 100, the rate on the day it was bought)", p.InBase.CostMinor)
	}
	if p.InBase.MarketValueMinor == nil || *p.InBase.MarketValueMinor != 5500000 {
		t.Errorf("in_base.market_value_minor = %v, want 5500000 (110000 * 50, today's rate)", p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor == nil {
		t.Fatalf("in_base.unrealized_pnl_minor = nil, want -4500000")
	}
	if *p.InBase.UnrealizedPnlMinor != -4500000 {
		t.Errorf("in_base.unrealized_pnl_minor = %d, want -4500000 (5500000 - 10000000)", *p.InBase.UnrealizedPnlMinor)
	}
	// The point of the test, as its own assertion.
	if *p.UnrealizedPnlMinor <= 0 || *p.InBase.UnrealizedPnlMinor >= 0 {
		t.Errorf("unrealized_pnl_minor = %d (USD) and in_base.unrealized_pnl_minor = %d (RUB): want opposite signs — a position can be in profit in its own currency and at a loss in the base currency, and both answers must be published as they are",
			*p.UnrealizedPnlMinor, *p.InBase.UnrealizedPnlMinor)
	}
}

// Each income operation converts at its own day's rate: two dividends and a
// tax on one share.
//
//	USD->RUB 60 from 2026-02-01, 90 from 2026-07-01
//	dividend +10_000 (03-10) -> 600_000
//	dividend +20_000 (07-10) -> 1_800_000
//	tax       −5_000 (07-10) -> −450_000
//	income 25_000 USD; base 1_950_000 (today's rate would give 2_250_000)
func TestPositionInBaseIncomeUsesEachOperationsOwnRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	// The buy is on the late date so the basis needs one rate.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":20000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"tax",
		"occurred_on":%q,"amount_minor":-5000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)

	if p.IncomeMinor != 25000 {
		t.Fatalf("income_minor = %d, want 25000 (10000 + 20000 - 5000, in USD)", p.IncomeMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.IncomeMinor == 2250000 {
		t.Fatalf("in_base.income_minor = 2250000 — that is income_minor times TODAY's rate (25000 * 90); each income operation must be valued at the rate of the day it occurred")
	}
	if p.InBase.IncomeMinor != 1950000 {
		t.Errorf("in_base.income_minor = %d, want 1950000 (600000 + 1800000 - 450000)", p.InBase.IncomeMinor)
	}
}

// One lot older than the rate table nulls the whole in_base, rather than a
// basis summed from the others (18_000_000) or the total at today's rate
// (27_000_000).
func TestPositionInBaseNullWhenALotHasNoRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the lot bought on %s has no fx rate on or before its date, and a basis missing one of its lots is a made-up number",
			*p.InBase, earlyBuyOn)
	}
}

// An income operation older than the rate table nulls the whole in_base too.
func TestPositionInBaseNullWhenAnIncomeOperationHasNoRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the dividend paid on %s has no fx rate on or before its date, and an income total missing one payment is a made-up number",
			*p.InBase, earlyBuyOn)
	}
}

// historicalFailingConverter answers dates from since on normally and fails
// earlier ones with a real error, so a request gets past today's rate and
// breaks on a lot's historical rate.
type historicalFailingConverter struct {
	rate   decimal.Decimal
	rateOn time.Time
	since  time.Time
	err    error
}

func (c historicalFailingConverter) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	if on.Before(c.since) {
		return decimal.Decimal{}, time.Time{}, c.err
	}
	return c.rate, c.rateOn, nil
}

// RatesOn answers from this double's own Rate (see ratesFromRate).
func (c historicalFailingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratetest.BatchFrom(ctx, c, queries)
}

// A real failure on a historical lookup fails the request, never shows as a
// null in_base.
func TestPositionInBaseHistoricalRateErrorFailsRequest(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := historicalFailingConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		// Yesterday: today succeeds, every operation date fails.
		since: time.Now().UTC().AddDate(0, 0, -1),
		err:   errors.New("connection reset by peer"),
	}
	url, c := setupAPI(t, pool, quotes, conv)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions with a failing HISTORICAL rate lookup = %d, want 500 — a real outage must not be served as a 200 with in_base: null: %s",
			resp.StatusCode, b)
	}
}

// onlyPosition fetches an account's positions and returns the single one.
func onlyPosition(t *testing.T, c *http.Client, url, accountID string) positionResp {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d, want 200: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", got.Positions)
	}
	return got.Positions[0]
}

// realizedTotalOf fetches the account's realized total.
func realizedTotalOf(t *testing.T, c *http.Client, url, accountID string) realizedTotalResp {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d, want 200: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	return got.RealizedTotal
}

// Lot terms are multiplied as decimals and rounded once for the total.
//
//	USD->RUB 90.5; two lots of 12_345: 1_117_222.5 each
//	once: 2_234_445; per lot: 2_234_446
//
// Income uses the same sum, so this covers both.
func TestPositionInBaseCostRoundsOnceForTheWholeBasis(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90.5"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	for range 2 {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"1","price":"123.45",
			"amount_minor":-12345,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	}

	p := onlyPosition(t, c, url, acc.ID)
	if p.CostMinor != 24690 {
		t.Fatalf("cost_minor = %d, want 24690 (two lots of 12345, in USD)", p.CostMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.CostMinor == 2234446 {
		t.Fatalf("in_base.cost_minor = 2234446 — that is each lot rounded before summing (1117223 twice); the basis must be summed as decimals and rounded once, giving 2234445")
	}
	if p.InBase.CostMinor != 2234445 {
		t.Errorf("in_base.cost_minor = %d, want 2234445 (round(12345*90.5 + 12345*90.5))", p.InBase.CostMinor)
	}
}

// A closed position has a zero base basis, not a null; its dividends keep
// their payment day's rate.
//
//	buy 10 @ 100 (03-10), dividend +50 (03-10), sell 10 @ 120 (07-10)
//	cost 0, value 0, unrealized 0, income 5_000 × 60 = 300_000
//	(today's rate would give 450_000)
func TestPositionInBaseClosedPositionHasZeroBasis(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("130.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":5000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.Quantity != "0" {
		t.Fatalf("quantity = %q, want %q (the position is fully closed)", p.Quantity, "0")
	}
	if p.CostMinor != 0 {
		t.Fatalf("cost_minor = %d, want 0 (nothing held)", p.CostMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object: a closed position has an empty basis, which is a zero, not a reason to refuse")
	}
	if p.InBase.CostMinor != 0 {
		t.Errorf("in_base.cost_minor = %d, want 0 (a sum over no lots)", p.InBase.CostMinor)
	}
	if p.InBase.MarketValueMinor == nil || *p.InBase.MarketValueMinor != 0 {
		t.Errorf("in_base.market_value_minor = %v, want 0 (quantity 0 at any price)", p.InBase.MarketValueMinor)
	}
	if p.InBase.UnrealizedPnlMinor == nil || *p.InBase.UnrealizedPnlMinor != 0 {
		t.Errorf("in_base.unrealized_pnl_minor = %v, want 0 (nothing held, nothing unrealized)", p.InBase.UnrealizedPnlMinor)
	}
	if p.InBase.IncomeMinor == 450000 {
		t.Fatalf("in_base.income_minor = 450000 — that is the dividend at TODAY's rate; a payment keeps the rate of the day it was paid even after the position is closed")
	}
	if p.InBase.IncomeMinor != 300000 {
		t.Errorf("in_base.income_minor = %d, want 300000 (5000 * 60, the rate on the day the dividend was paid)", p.InBase.IncomeMinor)
	}
}
