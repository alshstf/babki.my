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
			_, _ = w.Write([]byte(`{"securities":{"columns":["secid","shortname","isin","group","primary_boardid","is_traded"],"data":[
				["FIXGAZP","Фиксинг",null,"stock_index","INPF",1],
				["GAZP-OLD","Газпром","RU0007661625","stock_shares","EQBR",0],
				["GAZP","ГАЗПРОМ ао","RU0007661625","stock_shares","TQBR",1],
				["TMOS","TMOS ETF","RU000A101X76","stock_ppif","TQBR",1],
				["SU26238RMFS4","ОФЗ 26238","RU000A1038V6","stock_bonds","TQOB",1]
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
