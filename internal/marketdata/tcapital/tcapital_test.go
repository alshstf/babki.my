package tcapital

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func page(t *testing.T) (*Client, *int) {
	t.Helper()
	body, err := os.ReadFile("testdata/statistics.html")
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL), &asked
}

// The company's closed funds are read from its page, by ISIN; an exchange-
// traded fund has a market price and is left to it.
func TestTheClosedFundsAreReadFromTheCompanysPage(t *testing.T) {
	c, asked := page(t)
	funds, err := c.Funds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(funds) != 1 || funds["RU000A1071G8"] != "TECH2" {
		t.Errorf("funds = %v, want TECH2 alone", funds)
	}

	got, err := c.NAVHistory(t.Context(), "TECH2", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Day.Format(time.DateOnly) != "2026-09-30" ||
		!got[0].Price.Equal(decimal.RequireFromString("0.0198")) || got[0].Currency != "RUB" {
		t.Errorf("TECH2 from September = %+v, want the unit value of 0.0198 RUB on 2026-09-30", got)
	}
	if *asked != 1 {
		t.Errorf("the page was read %d times, want once for the run", *asked)
	}
}

// A page that no longer carries the data is a failure, not an empty answer.
func TestAPageWithoutTheDataIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>maintenance</body></html>"))
	}))
	t.Cleanup(srv.Close)
	if _, err := New(srv.Client(), srv.URL).Funds(t.Context()); err == nil {
		t.Error("a page with no data read as no funds")
	}
}
