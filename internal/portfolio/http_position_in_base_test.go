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

// TestPositionInBaseConvertsAllFourValues covers the brief's main case: a
// position in a non-base currency (USD) with a resolvable fx rate must get
// in_base filled in with all four values — cost_minor, market_value_minor,
// unrealized_pnl_minor, income_minor — each converted into the space's base
// currency (RUB, the default) using today's rate, plus the currency and
// rate_on it was converted with.
//
// Manual arithmetic, chosen so every step is exact (no rounding ambiguity to
// separate from marketdata.Converter's own rounding tests, which already
// cover that):
//
//	buy 10 @ 100.00 USD, fee 0 -> amount_minor -100_000 (cents)
//	  cost_minor (position currency, USD) = 100_000 (= $1,000.00)
//	dividend amount_minor 5_000 (cents), tagged to the instrument
//	  income_minor (USD) = 5_000 (= $50.00)
//	quote price 120.00 USD, quantity 10
//	  market_value_minor (USD) = 120.00 * 10 = 1_200.00 major = 120_000 minor
//	unrealized_pnl_minor (USD) = 120_000 - 100_000 = 20_000
//
//	fx rate USD->RUB = 90 (RUB per 1 USD)
//	  cost_minor_base           = 100_000 * 90 =  9_000_000
//	  income_minor_base         =   5_000 * 90 =    450_000
//	  market_value_minor_base   = 120_000 * 90 = 10_800_000
//	  unrealized_pnl_minor_base =  20_000 * 90 =  1_800_000
//
// All four converted figures are distinct, so a bug that mixed up which
// source value feeds which in_base field (e.g. applying cost_minor's result
// to income_minor too) would be caught by comparing each field to its own
// expected number.
func TestPositionInBaseConvertsAllFourValues(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	on := pastOn()
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	// Space's base currency defaults to RUB (see setupAPI's /api/v1/setup
	// call); the account below is USD, so it differs and must be converted.
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

	// Sanity-check the source (position-currency) figures the arithmetic
	// above is built on, before checking their conversions.
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
	if p.InBase.RateOn != wantRateOn {
		t.Errorf("in_base.rate_on = %q, want %q", p.InBase.RateOn, wantRateOn)
	}
}

// TestPositionInBaseNullWhenAlreadyBaseCurrency covers: a position already
// denominated in the space's base currency (RUB, the default) has nothing to
// convert, so in_base must be null even though the position has cost and
// income.
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

// TestPositionInBaseNullWhenNoRate covers: a position in a non-base currency
// with NO resolvable fx rate must come back with in_base = null as a whole
// — not partially populated with, say, cost_minor converted at some
// fallback rate — and the request as a whole must still succeed (200).
// Mirrors account's TestListBalanceInBaseNullWhenNoRate and toAPI's
// marketdata.ErrNoRate handling for market value conversion.
func TestPositionInBaseNullWhenNoRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// Deliberately no mdStore.UpsertFxRates call: no GBP->RUB rate exists
	// (no direct, inverse, or RUB-bridge leg — GBP has no rate at all here).

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

// TestPositionInBaseNullMarketValueAndUnrealizedPnlWithoutQuote covers: a
// position in a non-base currency WITH a resolvable fx rate, but no quote
// for its instrument, must still get a non-null in_base (cost_minor and
// income_minor are always computable, quote or not) — but
// market_value_minor and unrealized_pnl_minor inside it must be null,
// mirroring the top-level position's own null market_value_minor/
// unrealized_pnl_minor, since there is nothing to convert.
func TestPositionInBaseNullMarketValueAndUnrealizedPnlWithoutQuote(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	// No quotes seeded for any instrument.
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	on := pastOn()
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"USD"}`)

	// buy 5 @ 10.00 USD, fee 0 -> cost_minor (USD) = 5_000
	// dividend amount_minor 200 (cents) -> income_minor (USD) = 200
	//   cost_minor_base   = 5_000 * 90 = 450_000
	//   income_minor_base =   200 * 90 =  18_000
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

// TestPositionInBaseNullMarketValueWhenValuationInForeignCurrency covers the
// case where the position's market valuation is NOT denominated in the
// position's own currency, because the conversion into it failed for want of
// an fx rate (toAPI's marketdata.ErrNoRate fallback, which publishes the raw
// valuation as-is in market_value_currency).
//
// in_base converts everything at ONE rate — the position's own currency into
// the base currency — so it may only carry the market valuation when that
// valuation is actually in the position's currency. Otherwise the amount
// would be multiplied by a rate belonging to a different currency pair and
// published, unlabeled, as a base-currency figure: a silently wrong number,
// which is worse than no number at all. market_value_minor must therefore be
// null, and unrealized_pnl_minor with it (it is derived from the valuation),
// while cost_minor/income_minor — genuinely in the position's currency —
// still convert normally.
//
// Fixture (the reviewer's reproduction):
//
//	base currency RUB (space default); account and position in USD
//	bond face_value_minor 100_000, face_currency EUR, quote price 100.00
//	  market valuation = 100_000 * 100.00/100 * 1 = 100_000 minor EUR
//	  no EUR->USD rate exists -> market_value_currency stays EUR (raw)
//	fx_rates holds only USD->RUB = 90 (no EUR leg at all)
//	  in_base.cost_minor = 100_000 * 90 = 9_000_000 RUB
//
// Without the currency check, in_base.market_value_minor would come out as
// 100_000 * 90 = 9_000_000 RUB — a EUR amount multiplied by the USD rate,
// numerically identical to the converted cost, so the row would also read as
// "profit exactly zero". The real value is 1_000.00 EUR, and no EUR->RUB
// rate exists to express it in RUB at all.
func TestPositionInBaseNullMarketValueWhenValuationInForeignCurrency(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	on := pastOn()
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
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

	// Pin the fixture the assertions below rest on: the valuation really is
	// published raw, in a currency other than the position's own.
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
		t.Errorf("in_base.unrealized_pnl_minor = %v, want null (derived from a valuation that cannot be converted)", p.InBase.UnrealizedPnlMinor)
	}
	if p.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB", p.InBase.Currency)
	}
}
