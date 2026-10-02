package table_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata/moex"
)

// exchangeStub is the Moscow Exchange as far as these tests need it: Gazprom
// by ticker, an OFZ by ISIN, a ticker that names the catalog's Sberbank by
// another code, and an outage for the one code that asks for it.
type exchangeStub struct{}

func (exchangeStub) FindSecurity(_ context.Context, code string) (moex.Security, bool, error) {
	switch code {
	case "GAZP":
		return moex.Security{SecID: "GAZP", ISIN: "RU0007661625", Name: "ГАЗПРОМ ао", Kind: "share", Currency: "RUB"}, true, nil
	case "RU000A1038V6":
		return moex.Security{
			SecID: "SU26238RMFS4", ISIN: "RU000A1038V6", Name: "ОФЗ 26238", Kind: "bond", Currency: "RUB",
			FaceValue: decimal.RequireFromString("1000"), FaceCurrency: "RUB",
		}, true, nil
	case "SBERP-ALIAS":
		return moex.Security{SecID: "SBER", ISIN: "RU0009029540", Name: "Сбербанк", Kind: "share", Currency: "RUB"}, true, nil
	case "DOWN":
		return moex.Security{}, false, errors.New("connection refused")
	}
	return moex.Security{}, false, nil
}

// Papers a table names and the catalog lacks are filed from the exchange, so
// the rows naming them read on the next preview. A paper already in the
// catalog — under the code given or under its ISIN — is not filed twice.
func TestUnknownPapersAreFiledFromTheExchange(t *testing.T) {
	url, c := newAPI(t)
	var acc struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Счёт","type":"brokerage","currency":"RUB"}`, 201, &acc)
	call(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, 201, nil)

	var got struct {
		Added []struct {
			Code   string `json:"code"`
			Ticker string `json:"ticker"`
			Name   string `json:"name"`
		} `json:"added"`
		Known    []string `json:"known"`
		NotFound []string `json:"not_found"`
	}
	call(t, c, "POST", url+"/api/v1/imports/papers",
		`{"codes":["gazp","RU000A1038V6","SBER","SBERP-ALIAS","NOSUCH","GAZP"]}`, 200, &got)
	if len(got.Added) != 2 || got.Added[0].Ticker != "GAZP" || got.Added[1].Ticker != "SU26238RMFS4" {
		t.Errorf("added = %+v, want GAZP and the OFZ", got.Added)
	}
	if len(got.Known) != 2 || len(got.NotFound) != 1 || got.NotFound[0] != "NOSUCH" {
		t.Errorf("known = %v, not found = %v; want SBER twice known and NOSUCH not found", got.Known, got.NotFound)
	}

	var bond struct {
		Instruments []struct {
			Type           string  `json:"type"`
			FaceValueMinor *int64  `json:"face_value_minor"`
			FaceCurrency   *string `json:"face_currency"`
		} `json:"instruments"`
	}
	call(t, c, "GET", url+"/api/v1/instruments?query=RU000A1038V6", "", 200, &bond)
	if len(bond.Instruments) != 1 || bond.Instruments[0].Type != "bond" || bond.Instruments[0].FaceValueMinor == nil ||
		*bond.Instruments[0].FaceValueMinor != 100_000 || *bond.Instruments[0].FaceCurrency != "RUB" {
		t.Errorf("the OFZ was filed as %+v, want a bond with a 1 000 ₽ face", bond.Instruments)
	}

	csv := "Дата;Операция;Бумага;Количество;Цена;Сумма\n" +
		"01.07.2026;Пополнение;;;;10 000\n" +
		"02.07.2026;Покупка;GAZP;10;150;\n"
	body, _ := json.Marshal(map[string]string{"content": csv})
	var p preview
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview", string(body), 200, &p)
	if p.Rows[1].Verdict != "new" {
		t.Errorf("the GAZP row after filing = %s, want new", p.Rows[1].Verdict)
	}

	call(t, c, "POST", url+"/api/v1/imports/papers", `{"codes":["DOWN"]}`, 502, nil)
}
