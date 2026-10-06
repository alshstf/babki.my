package finex_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"babki.my/babki/internal/marketdata/finex"
)

// serve answers the manager's API from recorded responses (2026-10-06).
func serve(t *testing.T, routes map[string]string) *finex.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Errorf("fixture %s: %v", file, err)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return finex.New(srv.Client(), srv.URL+"/v1")
}

func TestFundsAreKeyedByISIN(t *testing.T) {
	c := serve(t, map[string]string{"/v1/fonds/": "fonds.json"})
	funds, err := c.Funds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if funds["IE00BD3QJ757"] != "FXIT" || funds["IE00BQ1Y6480"] != "FXRL" || len(funds) != 2 {
		t.Errorf("funds = %v, want FXIT and FXRL by ISIN", funds)
	}
}

// The history is the value per unit in the fund's currency, from the day asked.
func TestNAVHistoryStartsAtFromInTheFundsCurrency(t *testing.T) {
	c := serve(t, map[string]string{"/v1/fonds/FXIT/": "fxit.json", "/v1/fonds/FXIT/history/": "fxit_history.json"})
	days, err := c.NAVHistory(context.Background(), "FXIT", time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[1].Price.String() != "329.8645" || days[1].Currency != "USD" ||
		!days[1].Day.Equal(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("history = %+v, want 27 and 28 August, the last 329.8645 USD", days)
	}
}

func TestAFailedRequestIsAnError(t *testing.T) {
	c := serve(t, map[string]string{})
	if _, err := c.Funds(context.Background()); err == nil {
		t.Error("a 404 answered as an empty list")
	}
}
