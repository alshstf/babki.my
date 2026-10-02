package portfolio_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// A paper nobody quotes — a frozen fund — is valued at a price a person
// states, and the position says the price is theirs; a later price from the
// exchange takes over.
func TestAPriceStatedByHandValuesAPaperNobodyQuotes(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	fund := createInstrument(t, c, url, `{"type":"etf","name":"Замороженный фонд","ticker":"FXUS","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":100000,"currency":"RUB"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"100","currency":"RUB"}`, acc.ID, fund.ID))

	post := func(body string) int {
		resp, err := c.Post(url+"/api/v1/instruments/"+fund.ID+"/prices", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	position := func() map[string]any {
		var got struct {
			Positions []map[string]any `json:"positions"`
		}
		resp, err := c.Get(url + "/api/v1/accounts/" + acc.ID + "/positions")
		if err != nil {
			t.Fatal(err)
		}
		decodeJSON(t, resp, &got)
		return got.Positions[0]
	}
	if p := position(); p["market_value_minor"] != nil || p["market_value_gap"] != "no_quote" {
		t.Fatalf("before any price: %v", p)
	}

	if code := post(`{"on":"2026-09-01","price":"50"}`); code != http.StatusNoContent {
		t.Fatalf("state a price = %d", code)
	}
	if p := position(); p["market_value_minor"] != float64(50_000) || p["price_by_hand"] != true || p["price_on"] != "2026-09-01" {
		t.Errorf("with a price stated by hand: %v, want 500 ₽ by hand on 2026-09-01", p)
	}

	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{{
		InstrumentID: uuid.MustParse(fund.ID), On: mustDate(t, "2026-09-15"), Price: decimal.RequireFromString("60"), Currency: "RUB", Source: "moex",
	}}); err != nil {
		t.Fatal(err)
	}
	if p := position(); p["market_value_minor"] != float64(60_000) || p["price_by_hand"] == true {
		t.Errorf("after the exchange priced it again: %v, want 600 ₽ from the exchange", p)
	}

	for body, want := range map[string]int{
		`{"on":"2026-09-01","price":"0"}`:  http.StatusBadRequest,
		`{"on":"2099-01-01","price":"10"}`: http.StatusBadRequest,
		`{"on":"01.09.2026","price":"10"}`: http.StatusBadRequest,
	} {
		if code := post(body); code != want {
			t.Errorf("%s = %d, want %d", body, code, want)
		}
	}
	resp, _ := c.Post(url+"/api/v1/instruments/"+uuid.NewString()+"/prices", "application/json", strings.NewReader(`{"on":"2026-09-01","price":"10"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown paper = %d, want 404", resp.StatusCode)
	}
}
