package coingecko

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func server(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			_, _ = w.Write([]byte(`{"coins":[
				{"id":"bitget-wrapped-btc","symbol":"BGBTC","market_cap_rank":290},
				{"id":"bitcoin-on-x","symbol":"btc","market_cap_rank":null},
				{"id":"bitcoin","symbol":"BTC","market_cap_rank":1},
				{"id":"wrapped-bitcoin-lookalike","symbol":"BTC","market_cap_rank":3100}]}`))
		case "/coins/bitcoin/market_chart/range":
			if r.URL.Query().Get("vs_currency") != "usd" {
				t.Errorf("asked in %q, want usd", r.URL.Query().Get("vs_currency"))
			}
			// Two days' ends at midnight UTC, a repeat, and «now».
			_, _ = w.Write([]byte(`{"prices":[[1791158400000,86489.73],[1791244800000,85770.87],[1791244800000,1],[1791323880000,85587.1]]}`))
		case "/simple/price":
			_, _ = w.Write([]byte(`{"bitcoin":{"usd":85587,"last_updated_at":1791323880},"dead-coin":{"usd":0,"last_updated_at":1791323880}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL)
}

// A ticker names the largest coin spelled exactly so, not a wrapped lookalike.
func TestATickerNamesTheLargestCoinOfThatSymbol(t *testing.T) {
	c := server(t)
	if coin, ok, err := c.CoinFor(t.Context(), "btc"); err != nil || !ok || coin != "bitcoin" {
		t.Errorf("BTC = %q (%v, %v), want bitcoin", coin, ok, err)
	}
	if coin, ok, err := c.CoinFor(t.Context(), "XYZ"); err != nil || ok {
		t.Errorf("an unknown ticker found %q (%v)", coin, err)
	}
}

// A price dated midnight UTC is the day before's end; «now» is not a day's.
func TestHistoryFilesEachMidnightPriceUnderTheDayItEnded(t *testing.T) {
	c := server(t)
	got, err := c.History(t.Context(), "bitcoin", "USD", time.Unix(0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		day   string
		price string
	}{{"2026-10-04", "86489.73"}, {"2026-10-05", "85770.87"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %d days", got, len(want))
	}
	for i, w := range want {
		if got[i].Day.Format(time.DateOnly) != w.day || !got[i].Price.Equal(decimal.RequireFromString(w.price)) || got[i].Currency != "USD" {
			t.Errorf("day %d = %+v, want %s at %s USD", i, got[i], w.day, w.price)
		}
	}
}

// Today's price comes with its day; a coin priced at nothing is left out.
func TestCurrentPricesAreDatedByWhenTheyWereStruck(t *testing.T) {
	c := server(t)
	got, err := c.Current(t.Context(), []string{"bitcoin", "dead-coin"}, "USD")
	if err != nil {
		t.Fatal(err)
	}
	btc, ok := got["bitcoin"]
	if !ok || btc.Day.Format(time.DateOnly) != "2026-10-06" || !btc.Price.Equal(decimal.NewFromInt(85587)) {
		t.Errorf("bitcoin = %+v, want 85587 on 2026-10-06, the UTC day it was struck", btc)
	}
	if _, ok := got["dead-coin"]; ok {
		t.Error("a coin priced at nothing was kept")
	}
}
