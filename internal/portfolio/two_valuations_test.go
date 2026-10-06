package portfolio_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// twoValuations is a space with four holdings, each priced a different way
// today (decision Р-11): a FinEx fund at a dealer's price and with a published
// NAV in dollars; a frozen fund whose last market price is from 2022; a foreign
// share trading here in roubles with a home-exchange close in dollars; and a
// paper only a person has priced. A dollar is 90 roubles.
type twoValuations struct {
	url     string
	c       *http.Client
	account string
	space   uuid.UUID
	papers  map[string]uuid.UUID
	h       *portfolio.Service
	fam     *family.Store
	pool    *pgxpool.Pool
}

func setupTwoValuations(t *testing.T) twoValuations {
	t.Helper()
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, md, conv)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: today.AddDate(-5, 0, 0), Rate: decimal.NewFromInt(90), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	papers := map[string]uuid.UUID{}
	for name, body := range map[string]string{
		"fxit":  `{"type":"etf","name":"FinEx ИТ","ticker":"FXIT","isin":"IE00BD3QJ757","currency":"RUB"}`,
		"tech":  `{"type":"etf","name":"Технологии","ticker":"TECH","isin":"RU000A101X68","currency":"RUB"}`,
		"msft":  `{"type":"share","name":"Microsoft","ticker":"MSFT-RM","isin":"US5949181045","currency":"RUB"}`,
		"house": `{"type":"share","name":"Без котировок","ticker":"HOUSE","currency":"RUB"}`,
	} {
		papers[name] = uuid.MustParse(createInstrument(t, c, url, body).ID)
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":"2021-06-01","quantity":"10","price":"100","amount_minor":-100000,"currency":"RUB"}`, acc.ID, papers[name]))
	}
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{
		{InstrumentID: papers["fxit"], On: today, Price: decimal.NewFromInt(65), Currency: "RUB", Source: "tinvest"},
		{InstrumentID: papers["tech"], On: time.Date(2022, 2, 25, 0, 0, 0, 0, time.UTC), Price: decimal.RequireFromString("8.58"), Currency: "RUB", Source: "tinvest"},
		{InstrumentID: papers["msft"], On: today, Price: decimal.NewFromInt(18025), Currency: "RUB", Source: "tinvest"},
		{InstrumentID: papers["house"], On: today.AddDate(0, -2, 0), Price: decimal.NewFromInt(500), Currency: "RUB", Source: portfolio.ManualPriceSource},
	}); err != nil {
		t.Fatal(err)
	}
	if err := md.UpsertReferencePrices(t.Context(), []marketdata.ReferencePrice{
		{InstrumentID: papers["fxit"], Kind: marketdata.ReferenceNAV, On: today.AddDate(0, 0, -40), Price: decimal.NewFromInt(2), Currency: "USD", Source: "finex"},
		{InstrumentID: papers["msft"], Kind: marketdata.ReferenceForeign, On: today.AddDate(0, 0, -1), Price: decimal.NewFromInt(525), Currency: "USD", Source: "yahoo"},
	}); err != nil {
		t.Fatal(err)
	}
	var space uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&space); err != nil {
		t.Fatal(err)
	}
	fam := family.NewStore(pool)
	h := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, fam)
	return twoValuations{url: url, c: c, account: acc.ID, space: space, papers: papers, h: h, fam: fam, pool: pool}
}

func (f twoValuations) positions(t *testing.T) map[uuid.UUID]apitypes.Position {
	t.Helper()
	resp, err := f.c.Get(f.url + "/api/v1/accounts/" + f.account + "/positions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body apitypes.PositionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]apitypes.Position{}
	for _, p := range body.Positions {
		out[p.Instrument.Id] = p
	}
	return out
}

func value(n interface{ Get() (int64, error) }) string {
	v, err := n.Get()
	if err != nil {
		return "null"
	}
	return fmt.Sprint(v)
}

// The full valuation takes a fund's NAV and a foreign share's home-exchange
// close before any market price, and a person's price or a frozen fund's last
// market price after them; the liquid one takes only a market price no older
// than a month.
func TestAHoldingIsValuedTwice(t *testing.T) {
	f := setupTwoValuations(t)
	got := f.positions(t)

	for name, want := range map[string]struct {
		source, full, liquid, lastTraded string
	}{
		// 10 × 2 $ × 90; 10 × 65 ₽.
		"fxit": {"nav", "180000", "65000", ""},
		// 10 × 8,58 ₽ from 2022: kept in the full worth, nothing in the liquid one.
		"tech": {"market", "8580", "null", "2022-02-25"},
		// 10 × 525 $ × 90; 10 × 18 025 ₽.
		"msft": {"foreign", "47250000", "18025000", ""},
		// A person's price is not a market one.
		"house": {"manual", "500000", "null", ""},
	} {
		p := got[f.papers[name]]
		source, _ := p.PriceSource.Get()
		lastTraded, _ := p.LastTradedOn.Get()
		if string(source) != want.source || value(p.MarketValueMinor) != want.full ||
			value(p.LiquidValueMinor) != want.liquid || lastTraded != want.lastTraded {
			t.Errorf("%s: source %q, full %s, liquid %s, last traded %q; want %q, %s, %s, %q", name,
				source, value(p.MarketValueMinor), value(p.LiquidValueMinor), lastTraded,
				want.source, want.full, want.liquid, want.lastTraded)
		}
	}

	v, err := f.h.ValueFromJournal(t.Context(), f.space, uuid.MustParse(f.account))
	if err != nil {
		t.Fatal(err)
	}
	// Cash: 0 − 4 × 1 000 ₽ = −4 000 ₽ (−400 000 kopecks).
	if v.Minor != 65000+18025000-400000 || v.FullMinor != 180000+8580+47250000+500000-400000 ||
		v.Unpriced != 2 || v.NotTraded != 2 || v.FullUnpriced != 0 {
		t.Errorf("worth = %d liquid (%d unpriced, %d not traded), %d full (%d unpriced)",
			v.Minor, v.Unpriced, v.NotTraded, v.FullMinor, v.FullUnpriced)
	}
}

// A space that counts only what sells has a full worth equal to the liquid one.
func TestTheFullValuationCanBeTheLiquidOne(t *testing.T) {
	f := setupTwoValuations(t)
	liquid := string(family.FullValuationLiquid)
	if err := f.fam.UpdateSpaceSettings(t.Context(), f.space, nil, nil, &liquid); err != nil {
		t.Fatal(err)
	}
	got := f.positions(t)
	for name, want := range map[string]string{"fxit": "65000", "tech": "null", "msft": "18025000", "house": "null"} {
		p := got[f.papers[name]]
		if value(p.MarketValueMinor) != want || value(p.LiquidValueMinor) != want {
			t.Errorf("%s: full %s, liquid %s; want both %s", name, value(p.MarketValueMinor), value(p.LiquidValueMinor), want)
		}
	}
}

// A space that takes NAV but not home-exchange prices values the foreign share
// at its market price here.
func TestTheFullValuationCanLeaveForeignExchangesOut(t *testing.T) {
	f := setupTwoValuations(t)
	nav := string(family.FullValuationNAV)
	if err := f.fam.UpdateSpaceSettings(t.Context(), f.space, nil, nil, &nav); err != nil {
		t.Fatal(err)
	}
	got := f.positions(t)
	if p := got[f.papers["msft"]]; value(p.MarketValueMinor) != "18025000" {
		t.Errorf("Microsoft full = %s, want its market 18025000", value(p.MarketValueMinor))
	}
	if p := got[f.papers["fxit"]]; value(p.MarketValueMinor) != "180000" {
		t.Errorf("FinEx full = %s, want its NAV 180000", value(p.MarketValueMinor))
	}
}

// On an account whose broker trades on foreign exchanges, a foreign share's
// home-exchange close is what it sells for, so it counts in the liquid worth
// when no market price is known here (decision Р-20). On a Russian broker's
// account the same close stays in the full worth only.
func TestAForeignBrokersAccountSellsAtTheHomeExchangesClose(t *testing.T) {
	f := setupTwoValuations(t)
	nvda := uuid.MustParse(createInstrument(t, f.c, f.url,
		`{"type":"share","name":"NVIDIA","ticker":"NVDA","isin":"US67066G1040","currency":"USD"}`).ID)
	createOperation(t, f.c, f.url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2024-06-03","quantity":"2","price":"120","amount_minor":-24000,"currency":"USD"}`, f.account, nvda))
	md := marketdata.NewStore(f.pool)
	if err := md.UpsertReferencePrices(t.Context(), []marketdata.ReferencePrice{{
		InstrumentID: nvda, Kind: marketdata.ReferenceForeign, On: time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1),
		Price: decimal.NewFromInt(180), Currency: "USD", Source: "yahoo",
	}}); err != nil {
		t.Fatal(err)
	}

	p := f.positions(t)[nvda]
	if value(p.LiquidValueMinor) != "null" || value(p.MarketValueMinor) != "36000" {
		t.Fatalf("on a Russian broker's account: liquid %s, full %s; want null and 2 × 180 $", value(p.LiquidValueMinor), value(p.MarketValueMinor))
	}

	resp := apitest.Do(t, f.c, http.MethodPatch, f.url+"/api/v1/accounts/"+f.account, `{"trades_abroad":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch the account: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	p = f.positions(t)[nvda]
	source, _ := p.PriceSource.Get()
	if value(p.LiquidValueMinor) != "36000" || value(p.MarketValueMinor) != "36000" || source != apitypes.PriceSourceForeign {
		t.Errorf("on a foreign broker's account: liquid %s, full %s, source %q; want 36000 both, from the home exchange",
			value(p.LiquidValueMinor), value(p.MarketValueMinor), source)
	}
	// Microsoft has a market price here, which stays first.
	if m := f.positions(t)[f.papers["msft"]]; value(m.LiquidValueMinor) != "18025000" {
		t.Errorf("Microsoft liquid = %s, want its market price here, 18025000", value(m.LiquidValueMinor))
	}
}
