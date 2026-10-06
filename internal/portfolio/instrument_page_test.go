package portfolio_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/testdb"
)

type holdingsResp struct {
	Instrument struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"instrument"`
	Holdings []struct {
		AccountID string         `json:"account_id"`
		Position  map[string]any `json:"position"`
	} `json:"holdings"`
	Total map[string]any `json:"total"`
}

// One paper across the family: its position on each account whose journal
// names it — the same row each account's positions screen shows — and the two
// added up because both are kept in roubles. An account that never touched the
// paper is not listed.
func TestAPapersHoldingsAcrossTheFamily(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	a := createAccount(t, c, url, `{"name":"Первый","type":"brokerage","currency":"RUB"}`)
	b := createAccount(t, c, url, `{"name":"Второй","type":"brokerage","currency":"RUB"}`)
	other := createAccount(t, c, url, `{"name":"Без Сбера","type":"brokerage","currency":"RUB"}`)
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	gazp := createInstrument(t, c, url, `{"type":"share","name":"Газпром","ticker":"GAZP","currency":"RUB"}`)
	for _, acc := range []string{a.ID, b.ID, other.ID} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"RUB"}`, acc))
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"100","currency":"RUB"}`, a.ID, sber.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-11","quantity":"5","price":"120","currency":"RUB"}`, b.ID, sber.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-11","quantity":"7","price":"200","currency":"RUB"}`, other.ID, gazp.ID))
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{{
		InstrumentID: uuid.MustParse(sber.ID), On: mustDate(t, "2026-09-15"), Price: decimal.RequireFromString("150"), Currency: "RUB", Source: "moex",
	}}); err != nil {
		t.Fatal(err)
	}

	resp, err := c.Get(url + "/api/v1/instruments/" + sber.ID + "/holdings")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holdings = %d", resp.StatusCode)
	}
	var got holdingsResp
	apitest.Decode(t, resp, &got)

	if got.Instrument.ID != sber.ID || got.Instrument.Name != "Сбербанк" {
		t.Errorf("instrument = %+v, want Сбербанк", got.Instrument)
	}
	byAccount := map[string]map[string]any{}
	for _, h := range got.Holdings {
		byAccount[h.AccountID] = h.Position
	}
	if len(byAccount) != 2 || byAccount[other.ID] != nil {
		t.Fatalf("holdings on %d accounts (%v), want the two that bought SBER", len(byAccount), got.Holdings)
	}
	for acc, want := range map[string][2]any{a.ID: {"10", float64(100_000)}, b.ID: {"5", float64(60_000)}} {
		p := byAccount[acc]
		if p["quantity"] != want[0] || p["cost_minor"] != want[1] {
			t.Errorf("account %s: quantity %v cost %v, want %v", acc, p["quantity"], p["cost_minor"], want)
		}
		if instr, _ := p["instrument"].(map[string]any); instr["id"] != sber.ID {
			t.Errorf("account %s holds %v, want only SBER", acc, instr)
		}
	}
	want := map[string]any{
		"currency": "RUB", "quantity": "15", "cost_minor": float64(160_000),
		"market_value_minor": float64(225_000), "total_minor": float64(65_000),
	}
	for k, v := range want {
		if got.Total[k] != v {
			t.Errorf("total.%s = %v, want %v", k, got.Total[k], v)
		}
	}
}

// A total that would need a conversion is not given: positions kept in two
// currencies are listed and not added up. A figure missing on one position is
// missing from the total rather than summed without it.
func TestAPapersHoldingsAreNotAddedAcrossCurrenciesOrPastAGap(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	a := createAccount(t, c, url, `{"name":"Первый","type":"brokerage","currency":"RUB"}`)
	b := createAccount(t, c, url, `{"name":"Второй","type":"brokerage","currency":"USD"}`)
	paper := createInstrument(t, c, url, `{"type":"share","name":"Без цены","ticker":"NOPX","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"RUB"}`, a.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"100","currency":"RUB"}`, a.ID, paper.ID))

	get := func() holdingsResp {
		resp, err := c.Get(url + "/api/v1/instruments/" + paper.ID + "/holdings")
		if err != nil {
			t.Fatal(err)
		}
		var got holdingsResp
		apitest.Decode(t, resp, &got)
		return got
	}
	got := get()
	if got.Total == nil || got.Total["market_value_minor"] != nil || got.Total["total_minor"] != nil || got.Total["cost_minor"] != float64(100_000) {
		t.Errorf("total of one unpriced position = %v, want its cost and no value", got.Total)
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"USD"}`, b.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"1","price":"10","currency":"USD"}`, b.ID, paper.ID))
	if got := get(); len(got.Holdings) != 2 || got.Total != nil {
		t.Errorf("positions in two currencies: %d holdings, total %v — want both listed and no total", len(got.Holdings), got.Total)
	}
}

func TestAnUnknownPapersHoldingsAndPricesAre404(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	for _, path := range []string{"/holdings", "/prices?from=2026-01-01"} {
		resp, err := c.Get(url + "/api/v1/instruments/" + uuid.NewString() + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s of an unknown paper = %d, want 404", path, resp.StatusCode)
		}
	}
}

// The daily prices from `from` on, oldest first, whatever their source; a
// `from` in the future or past the ten-year reach is refused.
func TestAPapersDailyPrices(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	id := uuid.MustParse(sber.ID)
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{
		{InstrumentID: id, On: mustDate(t, "2026-08-31"), Price: decimal.RequireFromString("140"), Currency: "RUB", Source: "moex_history"},
		{InstrumentID: id, On: mustDate(t, "2026-09-02"), Price: decimal.RequireFromString("150.5"), Currency: "RUB", Source: "manual"},
		{InstrumentID: id, On: mustDate(t, "2026-09-01"), Price: decimal.RequireFromString("145"), Currency: "RUB", Source: "moex_history"},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := c.Get(url + "/api/v1/instruments/" + sber.ID + "/prices?from=2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	apitest.Decode(t, resp, &got)
	want := []map[string]any{
		{"on": "2026-09-01", "price": "145", "currency": "RUB", "source": "moex_history"},
		{"on": "2026-09-02", "price": "150.5", "currency": "RUB", "source": "manual"},
	}
	if len(got) != len(want) {
		t.Fatalf("prices = %v, want %v", got, want)
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("price %d %s = %v, want %v", i, k, got[i][k], v)
			}
		}
	}

	for _, from := range []string{"2099-01-01", "2000-01-01", "01.09.2026", ""} {
		resp, err := c.Get(url + "/api/v1/instruments/" + sber.ID + "/prices?from=" + from)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("from=%q = %d, want 400", from, resp.StatusCode)
		}
	}
}
