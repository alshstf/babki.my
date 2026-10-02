package moex

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// A security's closing prices come page by page, as the exchange serves them:
// only the boards this program prices, the official close where there is one
// and the last trade where there is not, and no day without either. A fund's
// history runs on across its move from TQTF to TQBR.
func TestHistoryWalksThePagesAndKeepsThePricedBoards(t *testing.T) {
	pages := map[string]string{
		"0": `{"history":{"columns":["BOARDID","TRADEDATE","CLOSE","LEGALCLOSEPRICE","CURRENCYID"],"data":[
			["TQTF","2026-06-18",5.83,5.85,"SUR"],
			["SMAL","2026-06-18",5.9,5.9,"SUR"],
			["TQTF","2026-06-19",5.78,null,"SUR"]
		]},"history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[0,5,3]]}}`,
		"3": `{"history":{"columns":["BOARDID","TRADEDATE","CLOSE","LEGALCLOSEPRICE","CURRENCYID"],"data":[
			["TQBR","2026-06-22",5.52,5.57,"SUR"],
			["TQBR","2026-06-23",null,0,"SUR"]
		]},"history.cursor":{"columns":["INDEX","TOTAL","PAGESIZE"],"data":[[3,5,3]]}}`,
	}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/iss/history/engines/stock/markets/shares/securities/TMOS.json" {
			http.NotFound(w, r)
			return
		}
		start := r.URL.Query().Get("start")
		asked = append(asked, start)
		_, _ = fmt.Fprint(w, pages[start])
	}))
	defer srv.Close()

	got, err := New(srv.Client(), srv.URL, nil).History(t.Context(), "shares", "TMOS",
		time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ day, price string }{{"2026-06-18", "5.85"}, {"2026-06-19", "5.78"}, {"2026-06-22", "5.57"}}
	if len(got) != len(want) || len(asked) != 2 {
		t.Fatalf("got %+v from pages %v, want %v from two pages", got, asked, want)
	}
	for i, w := range want {
		if got[i].Day.Format(time.DateOnly) != w.day || !got[i].Price.Equal(decimal.RequireFromString(w.price)) || got[i].Currency != "RUB" {
			t.Errorf("day %d = %+v, want %s at %s RUB", i, got[i], w.day, w.price)
		}
	}
}
