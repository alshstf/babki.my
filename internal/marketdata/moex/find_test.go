package moex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// The exchange's search answers with every paper whose name, ticker or ISIN
// holds the query — indices, delisted issues, other boards. Only an exact
// ticker or ISIN, traded on a board this program prices, is taken; a bond's
// original face value and its currency come from its description.
func TestASecurityIsFoundOnlyByAnExactCodeOnAPricedBoard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/iss/securities.json":
			_, _ = w.Write([]byte(`{"securities":{"columns":["secid","shortname","name","isin","group","primary_boardid","is_traded"],"data":[
				["FIXGAZP","Фиксинг","Фиксинг Газпром",null,"stock_index","INPF",1],
				["GAZP-OLD","Газпром","Газпром ПАО ао","RU0007661625","stock_shares","EQBR",0],
				["GAZP","ГАЗПРОМ ао","Газпром ПАО ао","RU0007661625","stock_shares","TQBR",1],
				["TMOS","TMOS ETF","БПИФ Тинькофф iMOEX","RU000A101X76","stock_ppif","TQBR",1],
				["SU26238RMFS4","ОФЗ 26238","ОФЗ-ПД 26238","RU000A1038V6","stock_bonds","TQOB",1]
			]}}`))
		case strings.HasPrefix(r.URL.Path, "/iss/securities/SU26238RMFS4.json"):
			_, _ = w.Write([]byte(`{"description":{"columns":["name","value"],"data":[
				["SECID","SU26238RMFS4"],["FACEVALUE","1000"],["INITIALFACEVALUE","1000"],["FACEUNIT","SUR"]
			]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.Client(), srv.URL, nil)

	for code, want := range map[string]Security{
		"gazp":         {SecID: "GAZP", ISIN: "RU0007661625", Name: "ГАЗПРОМ ао", Kind: "share", Currency: "RUB"},
		"RU0007661625": {SecID: "GAZP", ISIN: "RU0007661625", Name: "ГАЗПРОМ ао", Kind: "share", Currency: "RUB"},
		"TMOS":         {SecID: "TMOS", ISIN: "RU000A101X76", Name: "TMOS ETF", Kind: "etf", Currency: "RUB"},
		"RU000A1038V6": {
			SecID: "SU26238RMFS4", ISIN: "RU000A1038V6", Name: "ОФЗ 26238", Kind: "bond", Currency: "RUB",
			FaceValue: decimal.RequireFromString("1000"), FaceCurrency: "RUB",
		},
	} {
		got, ok, err := c.FindSecurity(t.Context(), code)
		if err != nil || !ok {
			t.Fatalf("%s: found %v, %v", code, ok, err)
		}
		if got.SecID != want.SecID || got.ISIN != want.ISIN || got.Kind != want.Kind || got.Currency != want.Currency ||
			got.Name != want.Name || !got.FaceValue.Equal(want.FaceValue) || got.FaceCurrency != want.FaceCurrency {
			t.Errorf("%s = %+v, want %+v", code, got, want)
		}
	}
	for _, code := range []string{"FIXGAZP", "GAZP-OLD", "GAZ", ""} {
		if got, ok, err := c.FindSecurity(t.Context(), code); ok || err != nil {
			t.Errorf("%q found %+v (%v), want nothing", code, got, err)
		}
	}
}

// A paper the exchange no longer trades is still in its reference: a
// receipt of a company that moved to Russia (TCS Group, replaced by shares of
// МКПАО «ТКС Холдинг» on 27.02.2024). Found by its exact ISIN, under its full
// name, as a share; no currency, since no board trades it.
func TestAPaperNoLongerTradedIsRememberedByItsIsin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/iss/securities.json" && r.URL.Query().Get("q") == "US87238U2033":
			_, _ = w.Write([]byte(`{"securities":{"columns":["secid","shortname","name","isin","group","primary_boardid","is_traded"],"data":[
				["TCS-ME","TCS-гдр","ГДР TCS Group Holding ORD SHS","US87238U2033","stock_dr","RPMA",0]
			]}}`))
		case r.URL.Path == "/iss/securities.json" && r.URL.Query().Get("q") == "XS0088543193":
			_, _ = w.Write([]byte(`{"securities":{"columns":["secid","shortname","name","isin","group","primary_boardid","is_traded"],"data":[
				["XS0088543193","RUS-28","ГОВОЗ РФ МК-0-СМ-119 (XS)","XS0088543193","stock_bonds","TQCB",1]
			]}}`))
		case strings.HasPrefix(r.URL.Path, "/iss/securities/XS0088543193.json"):
			_, _ = w.Write([]byte(`{"description":{"columns":["name","value"],"data":[
				["FACEVALUE","1000"],["INITIALFACEVALUE","1000"],["FACEUNIT","USD"]
			]}}`))
		case r.URL.Path == "/iss/securities.json":
			_, _ = w.Write([]byte(`{"securities":{"columns":["secid","shortname","name","isin","group","primary_boardid","is_traded"],"data":[
				["IMOEX","Индекс МосБиржи","Индекс МосБиржи",null,"stock_index","SNDX",1]
			]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.Client(), srv.URL, nil)

	got, ok, err := c.RememberedByISIN(t.Context(), " us87238u2033 ")
	if err != nil || !ok {
		t.Fatalf("found %v, %v; want the receipt the exchange still remembers", ok, err)
	}
	want := Security{SecID: "TCS-ME", ISIN: "US87238U2033", Name: "ГДР TCS Group Holding ORD SHS", Kind: "share"}
	if got.SecID != want.SecID || got.ISIN != want.ISIN || got.Name != want.Name || got.Kind != want.Kind || got.Currency != "" {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// Not traded on a priced board: the ordinary finder still answers nothing.
	if _, ok, err := c.FindSecurity(t.Context(), "US87238U2033"); ok || err != nil {
		t.Errorf("FindSecurity found a receipt no board trades (%v)", err)
	}

	bond, ok, err := c.RememberedByISIN(t.Context(), "XS0088543193")
	if err != nil || !ok || bond.Kind != "bond" || !bond.FaceValue.Equal(decimal.NewFromInt(1000)) || bond.FaceCurrency != "USD" {
		t.Errorf("bond = %+v (%v, %v), want a bond of 1000 USD face", bond, ok, err)
	}

	if got, ok, err := c.RememberedByISIN(t.Context(), "RU0009029540"); ok || err != nil {
		t.Errorf("an ISIN the exchange does not know found %+v (%v)", got, err)
	}
}
