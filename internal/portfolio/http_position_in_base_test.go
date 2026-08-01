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
	"babki.my/babki/internal/platform/testdb"
)

// fxSeedOn is the date the fx-rate fixtures in this file are seeded on when a
// single rate has to cover every date a test touches: a lot's acquisition
// date, an income operation's date, and "today" alike. It sits safely EARLIER
// than the 2026-07-xx operation dates the fixtures use, so Store.FxRateOn's
// nearest-earlier-date lookup finds it from all three.
//
// pastOn() (now minus one month) cannot serve that role: it is only ever
// earlier than "today", and the moment the wall clock drifts past 2026-08-01
// it lands AFTER the fixtures' own operation dates — a lot bought on
// 2026-07-01 would then resolve no rate at all and in_base would go null for
// a reason that has nothing to do with what the test is checking. Tests that
// deliberately want two DIFFERENT rates seed their own explicit dates.
func fxSeedOn(t *testing.T) time.Time {
	t.Helper()
	return mustDate(t, "2026-01-01")
}

// TestPositionInBaseConvertsAllFourValues covers the shape of the whole
// object: a position in a non-base currency (USD) with a resolvable fx rate
// must get in_base filled in with all four values — cost_minor,
// market_value_minor, unrealized_pnl_minor, income_minor — expressed in the
// space's base currency (RUB, the default), plus the currency and rate_on.
//
// Exactly ONE rate is seeded here, early enough to cover every date in the
// fixture, so the historical rates behind cost_minor/income_minor and today's
// rate behind market_value_minor all coincide. That is deliberate: this test
// pins the plumbing (which source figure feeds which in_base field, and that
// none of them is dropped), while the semantics — that each lot and each
// income operation is valued at ITS OWN date's rate — are pinned by the
// two-rate tests below, which are built so that the two answers differ.
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

	on := fxSeedOn(t)
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

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
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
// in_base uses many rates (one per lot date, one per income date, today's for
// the valuation) but always ONE currency pair — the position's own currency
// into the base currency — so it may only carry the market valuation when that
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

// Dates shared by the historical-basis fixtures below. Two fx rates are
// seeded, on earlyRateOn and lateRateOn; every operation date resolves to one
// of them through Store.FxRateOn's nearest-earlier-date lookup — earlyBuyOn
// falls after the first and before the second, lateBuyOn after the second —
// and so does "today", whenever the test actually runs, since nothing newer
// than lateRateOn is ever seeded. That last part is what makes today's rate
// equal to the late rate and lets the arithmetic below be written down as
// fixed numbers.
const (
	earlyRateOn = "2026-02-01"
	lateRateOn  = "2026-07-01"
	earlyBuyOn  = "2026-03-10"
	lateBuyOn   = "2026-07-10"
)

// twoRateAPI wires the fixture the historical-basis tests share: an
// RUB-based space (setupAPI's default) whose fx table holds exactly two
// USD->RUB rates, early on earlyRateOn and late on lateRateOn. The caller
// creates its own USD account and instruments.
func twoRateAPI(t *testing.T, quotes quoteStoreLike, early, late string) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString(early), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString(late), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}
	return url, c
}

// TestPositionInBaseCostUsesEachLotsOwnRate is the core of this change: the
// base-currency cost basis is the sum of the FIFO lots still held, each
// converted at the fx rate of the day THAT lot was bought — not the position's
// total cost converted at today's rate.
//
// The two are different questions. "Total cost at today's rate" answers what
// the basis would have cost if everything had been bought this morning, which
// nobody asked; it also cancels the currency's move straight out of the
// profit, leaving base-currency profit as nothing but position-currency profit
// times a rate. The historical basis answers what the position actually cost
// in rubles, and lets the ruble's own move show up in the profit.
//
// Manual arithmetic (every step exact — no rounding to disentangle from
// marketdata.Converter's own rounding tests):
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	buy 10 @ 100.00 USD on 2026-03-10 -> lot 1: cost 100_000 minor USD
//	  valued at the 2026-02-01 rate (the newest one on or before 2026-03-10):
//	  100_000 * 60 =  6_000_000
//	buy 10 @ 200.00 USD on 2026-07-10 -> lot 2: cost 200_000 minor USD
//	  valued at the 2026-07-01 rate: 200_000 * 90 = 18_000_000
//
//	cost_minor (USD)     = 100_000 + 200_000        =    300_000
//	in_base.cost_minor   =  6_000_000 + 18_000_000  = 24_000_000
//
// The number the old semantics produced is deliberately different:
// 300_000 * 90 (today's rate) = 27_000_000. Both figures are asserted, the
// wrong one by name, so a regression to "whole position at today's rate"
// cannot pass this test quietly.
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
	// rate_on names the rate behind the "what is it worth now" figure, which
	// is today's — the historical rates behind the basis have no single date
	// to report (see the handler's positionInBase).
	if p.InBase.RateOn != lateRateOn {
		t.Errorf("in_base.rate_on = %q, want %q (today's rate, i.e. the newest row in the table)", p.InBase.RateOn, lateRateOn)
	}
}

// transferOn is the day the in-kind transfer below happens: after both
// purchases and after lateRateOn, so the transfer's own date resolves to the
// LATE rate while the older of the two purchases resolves to the early one.
// That gap is what makes "valued at the purchase dates" and "valued at the
// transfer date" two visibly different numbers.
const transferOn = "2026-07-20"

func createTransfer(t *testing.T, c *http.Client, url, body string) {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create transfer = %d: %s", resp.StatusCode, b)
	}
}

// TestPositionInBaseTransferredLotsKeepTheirPurchaseDates is what the whole
// transfer-breakdown work exists for: a position moved between two of the
// family's own accounts must be worth, in rubles, exactly what it was worth
// before it moved. Moving shares from one broker to another is not a purchase,
// so it cannot change the basis — yet a transfer that carried only a single
// summed cost re-dated everything to the day of the move, and the destination
// then valued the whole position at the fx rate of that day.
//
// The fixture is TestPositionInBaseCostUsesEachLotsOwnRate's, reached through a
// transfer: the two lots are bought in the SOURCE account on dates with
// different rates, and the whole position is then transferred to a second
// account on a third date. What the destination reports must equal what the
// source would have reported.
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	source: buy 10 @ $100.00 on 2026-03-10 -> lot 1: 100_000 minor USD
//	        buy 10 @ $200.00 on 2026-07-10 -> lot 2: 200_000 minor USD
//	transfer all 20 units to the second account on 2026-07-20
//
//	destination cost_minor (USD) = 300_000 (the basis follows the shares)
//	in_base.cost_minor           = 100_000*60 + 200_000*90 = 24_000_000
//	  (the rates of the two PURCHASE days, in the account that now holds them)
//	the transfer-date answer     = 300_000 * 90            = 27_000_000
//
// Both figures are asserted, the wrong one by name: 27_000_000 is what a
// destination that collapses the arrival into one lot dated 2026-07-20
// produces, and it is 3_000_000 rubles of profit invented out of the dollar's
// rise between the first purchase and the day the shares changed brokers.
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

	// The source really did give everything up: without this, a transfer that
	// silently moved nothing would leave the assertions below meaningless.
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

// TestPositionInBaseUnrealizedPnlIncludesCurrencyRevaluation is the second
// discriminating test: base-currency profit is the current valuation in rubles
// minus the HISTORICAL ruble basis, so it carries the currency's own move.
// It is therefore not the position-currency profit times any rate, and this
// test is built so those two numbers cannot be confused for one another.
//
//	same fx table and lots as TestPositionInBaseCostUsesEachLotsOwnRate:
//	  cost_minor (USD)   =    300_000, in_base.cost_minor = 24_000_000
//	quote 250.00 USD, quantity 20
//	  market_value_minor (USD)  = 250.00 * 20 = 5_000.00 major = 500_000 minor
//	  in_base.market_value_minor = 500_000 * 90 (today) = 45_000_000
//
//	unrealized_pnl_minor (USD)         =    500_000 -    300_000 =    200_000
//	in_base.unrealized_pnl_minor       = 45_000_000 - 24_000_000 = 21_000_000
//	the old semantics (USD profit at today's rate) = 200_000 * 90 = 18_000_000
//
// 21_000_000 is the honest ruble profit: 18_000_000 of it is the position
// working, and the remaining 3_000_000 is the dollar having risen from 60 to
// 90 on the first lot (100_000 * (90-60) = 3_000_000) between the day it was
// bought and today.
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

// TestPositionInBaseProfitInPositionCurrencyLossInBase pins the consequence
// the owner asked for explicitly: one position can be in profit measured in
// its own currency and at a LOSS measured in the base currency, at the same
// moment. The two numbers answer different questions — "did the instrument
// go up in dollars" and "did the holding grow my rubles" — and when the ruble
// strengthens hard enough, the honest answers disagree in sign. That is the
// intended behavior, not a bug to be smoothed over: a version that keeps the
// signs in step would be hiding the currency loss from its owner.
//
//	fx USD->RUB: 100 from 2026-02-01, 50 from 2026-07-01 (ruble doubles)
//	buy 10 @ 100.00 USD on 2026-03-10 -> cost 100_000 minor USD
//	  in_base.cost_minor = 100_000 * 100 = 10_000_000
//	quote 110.00 USD, quantity 10 -> market_value_minor (USD) = 110_000
//	  in_base.market_value_minor = 110_000 * 50 = 5_500_000
//
//	unrealized_pnl_minor (USD)   =    110_000 -    100_000 =  +10_000  (profit)
//	in_base.unrealized_pnl_minor =  5_500_000 - 10_000_000 = -4_500_000 (loss)
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
	// The point of the whole test, stated as its own assertion so the failure
	// message says what broke rather than just which number moved.
	if *p.UnrealizedPnlMinor <= 0 || *p.InBase.UnrealizedPnlMinor >= 0 {
		t.Errorf("unrealized_pnl_minor = %d (USD) and in_base.unrealized_pnl_minor = %d (RUB): want opposite signs — a position can be in profit in its own currency and at a loss in the base currency, and both answers must be published as they are",
			*p.UnrealizedPnlMinor, *p.InBase.UnrealizedPnlMinor)
	}
}

// TestPositionInBaseIncomeUsesEachOperationsOwnRate covers income the same way
// the lots are covered: each income operation is converted at the rate of the
// day it occurred, and the results summed — the position's income total has
// already lost the dates, so it can never be converted as one number.
//
// The fixture puts all three types the engine folds into income_minor
// (dividend, coupon, tax — see portfolio/engine.go) on ONE instrument, so a
// filter here that drifts from the engine's own list fails this test instead
// of silently publishing a base income smaller than the position's own. A
// coupon on a share is not realistic, but both the journal's validation and
// the engine accept it, and realism is not what this test is about.
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	dividend  +10_000 on 2026-03-10 -> 10_000 * 60 =   600_000
//	coupon    +20_000 on 2026-07-10 -> 20_000 * 90 = 1_800_000
//	tax        -5_000 on 2026-07-10 -> -5_000 * 90 =  -450_000
//
//	income_minor (USD)     = 10_000 + 20_000 - 5_000 =    25_000
//	in_base.income_minor   = 600_000 + 1_800_000 - 450_000 = 1_950_000
//	the old semantics (total at today's rate) = 25_000 * 90 =  2_250_000
func TestPositionInBaseIncomeUsesEachOperationsOwnRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := twoRateAPI(t, quotes, "60", "90")

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	// The buy is on the late date so the basis needs only one rate: this test
	// is about income, and the lot arithmetic is pinned elsewhere.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon",
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

// TestPositionInBaseNullWhenALotHasNoRate covers the honesty rule at the lot
// level: a single lot older than anything in the fx table leaves the whole
// in_base null, not a basis silently summed from the lots that did convert.
// A partial basis is an invented number — smaller than the truth, indistinguishable
// from a real one on screen, and it would drag unrealized_pnl_minor up with it.
//
// The fixture is built so that a naive implementation cannot slip through: the
// rate table starts on 2026-07-01, so the 2026-03-10 lot has no rate on or
// before its date, while the 2026-07-10 lot AND today both resolve fine.
// Converting the position's total at today's rate — or skipping the lot that
// failed — would produce a plausible 200_000 * 90 = 18_000_000 or
// 300_000 * 90 = 27_000_000 instead of null.
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

// TestPositionInBaseNullWhenAnIncomeOperationHasNoRate is the same honesty
// rule on the income side: the lots and today's rate all resolve, only the
// dividend's own date predates the rate table, and that alone must null the
// whole object rather than publish an income total quietly missing one payment.
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

// historicalFailingConverter resolves rates on or after `since` normally and
// fails every EARLIER lookup with a genuine, non-ErrNoRate error — a dropped
// connection while reading an old row, say. It exists to reach the failure
// path that failingConverter cannot: with `since` set just before today, the
// request gets past today's rate (which supplies rate_on and the market
// valuation) and only breaks on a LOT's historical rate, which is where the
// per-lot conversion added by this change does its work.
type historicalFailingConverter struct {
	rate   decimal.Decimal
	rateOn time.Time
	since  time.Time
	err    error
}

// Convert is never reached by the test using this double — a share's
// valuation is already in the position's own currency, so toAPI does no fx
// conversion at all — and so it fails unconditionally rather than inventing a
// plausible number for a path nobody exercises.
func (c historicalFailingConverter) Convert(_ context.Context, _ int64, _, _ string, _ time.Time) (int64, error) {
	return 0, c.err
}

func (c historicalFailingConverter) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	if on.Before(c.since) {
		return decimal.Decimal{}, time.Time{}, c.err
	}
	return c.rate, c.rateOn, nil
}

// TestPositionInBaseHistoricalRateErrorFailsRequest extends the distinction
// TestPositionsRealRateErrorFailsRequest draws — a genuine failure must fail
// the request, never be served as in_base: null — to the historical lookups
// this change introduces. Without it, wrapping the per-lot rate resolution in
// a "treat any error as no rate" shortcut would silently turn a database
// outage into an ordinary, expected-looking "no rate" marker on screen, and no
// other test would notice: today's rate still resolves here, so the request
// otherwise looks perfectly healthy.
func TestPositionInBaseHistoricalRateErrorFailsRequest(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := historicalFailingConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		// Yesterday: today's lookup (time.Now().UTC()) succeeds, every
		// operation date in this fixture fails.
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

// onlyPosition fetches an account's positions and returns the single one it
// must contain, failing the test on anything else. The in_base tests all use
// one-position accounts, and this keeps their bodies about the arithmetic.
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

// TestPositionInBaseCostRoundsOnceForTheWholeBasis pins where the rounding
// happens: the lots are multiplied by their rates as decimals and only the
// TOTAL is rounded, once. Rounding each lot first and adding the results is
// the tempting shape (it reads like "convert each lot"), and it is off by a
// minor unit here — a drift that grows with the number of lots and would
// leave in_base.cost_minor not quite equal to any sum a person could
// reproduce by hand.
//
//	fx USD->RUB = 90.5, one rate for every date, so this test says nothing
//	  about which date's rate is used (that is pinned above) — only about
//	  rounding
//	two buys of $123.45 -> two lots of 12_345 minor USD each
//	  12_345 * 90.5 = 1_117_222.5 exactly, per lot
//
//	rounding once, at the end:  1_117_222.5 + 1_117_222.5 = 2_234_445.0 -> 2_234_445
//	rounding per lot (wrong):   1_117_223   + 1_117_223   =              = 2_234_446
//
// income_minor goes through the same helper (see the handler's sumInBase), so
// this covers the rounding contract for both figures.
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

// TestPositionInBaseClosedPositionHasZeroBasis covers the position the whole
// historical-basis machinery has the least to say about: a fully closed one.
// It has no lots left, so the basis is a sum over nothing — and a sum over
// nothing must be a published zero, not a null in_base and not a refusal.
// The row stays in the list as history (see handleList), and in base-currency
// mode it has to read as history rather than as a broken row.
//
// The income is what makes the test worth having: a closed position can still
// carry dividends collected while it was open, and those keep being valued at
// the rate of the day they were paid even though nothing is held any more.
// The lots are gone; the payments' dates are not.
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	buy      10 @ $100.00 on 2026-03-10 -> cost 100_000 minor USD
//	dividend    +$50.00   on 2026-03-10 -> income 5_000 minor USD
//	sell     10 @ $120.00 on 2026-07-10 -> realized +20_000, no lots left
//	quote $130.00, quantity 0 -> market_value_minor (USD) = 0
//
//	in_base.cost_minor         = (no lots)          =       0
//	in_base.market_value_minor = 0 * 90             =       0
//	in_base.unrealized_pnl     = 0 - 0              =       0
//	in_base.income_minor       = 5_000 * 60         = 300_000
//	  (at today's rate it would be 5_000 * 90 = 450_000 — the wrong answer,
//	   asserted by name below)
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
