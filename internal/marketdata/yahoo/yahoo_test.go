package yahoo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/marketdata/yahoo"
)

// serve answers search and chart from recorded responses (2026-10-06), or
// from body when one is given for the chart.
func serve(t *testing.T, chart string) *yahoo.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var file string
		switch {
		case r.URL.Path == "/v1/finance/search" && r.URL.Query().Get("q") == "US5949181045":
			file = "search_msft.json"
		case r.URL.Path == "/v1/finance/search" && r.URL.Query().Get("q") == "TSM":
			// TSMC's ADR, by its ticker (recorded 2026-10-06, trimmed).
			_, _ = w.Write([]byte(`{"quotes":[{"symbol":"TSM","quoteType":"EQUITY"},{"symbol":"TSMC34.SA","quoteType":"EQUITY"}]}`))
			return
		case r.URL.Path == "/v1/finance/search" && r.URL.Query().Get("q") == "T":
			_, _ = w.Write([]byte(`{"quotes":[{"symbol":"TT","quoteType":"EQUITY"}]}`))
			return
		case r.URL.Path == "/v1/finance/search":
			_, _ = w.Write([]byte(`{"quotes":[]}`))
			return
		case strings.HasPrefix(r.URL.Path, "/v8/finance/chart/"):
			if chart != "" {
				_, _ = w.Write([]byte(chart))
				return
			}
			file = "chart_msft.json"
		default:
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
	return yahoo.New(srv.Client(), srv.URL)
}

func TestASharesSymbolIsFoundByItsISIN(t *testing.T) {
	c := serve(t, "")
	if s, ok, err := c.SymbolFor(context.Background(), "US5949181045", "MSFT-RM"); err != nil || !ok || s != "MSFT" {
		t.Errorf("SymbolFor = %q, %v, %v; want MSFT", s, ok, err)
	}
	if _, ok, err := c.SymbolFor(context.Background(), "US8740391003", ""); err != nil || ok {
		t.Errorf("an ISIN the search does not know, and no ticker: ok = %v, err = %v; want not found", ok, err)
	}
	// A receipt the search does not find by ISIN is found by its ticker, the
	// Moscow listing's «-RM» dropped; a symbol not spelled as the ticker is not
	// taken.
	if s, ok, err := c.SymbolFor(context.Background(), "US8740391003", "TSM-RM"); err != nil || !ok || s != "TSM" {
		t.Errorf("TSMC by its ticker = %q, %v, %v; want TSM", s, ok, err)
	}
	if s, ok, err := c.SymbolFor(context.Background(), "US00206R1023", "T"); err != nil || ok {
		t.Errorf("a ticker matching no symbol exactly = %q, %v, %v; want not found", s, ok, err)
	}
}

// Closes are dated by the exchange's own day, not UTC's, and rounded past the
// float noise.
func TestClosesAreDatedByTheExchangesDay(t *testing.T) {
	c := serve(t, "")
	days, err := c.Closes(context.Background(), "MSFT", time.Date(2025, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2025, 10, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 5 || !days[0].Day.Equal(time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC)) ||
		days[0].Price.String() != "514.6" || days[0].Currency != "USD" {
		t.Errorf("closes = %+v, want five days from 29 September, the first 514.6 USD", days)
	}
}

// A price in pence is a price in pounds a hundred times smaller; a day with
// no close is left out.
func TestPenceBecomePoundsAndEmptyDaysAreSkipped(t *testing.T) {
	c := serve(t, `{"chart":{"result":[{"meta":{"currency":"GBp","exchangeTimezoneName":"Europe/London"},
		"timestamp":[1759219200,1759305600],"indicators":{"quote":[{"close":[1234.5,null]}]}}],"error":null}}`)
	days, err := c.Closes(context.Background(), "BP.L", time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC), time.Date(2025, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Currency != "GBP" || days[0].Price.String() != "12.345" {
		t.Errorf("closes = %+v, want one day at 12.345 GBP", days)
	}
}

func TestAChartErrorIsAnError(t *testing.T) {
	c := serve(t, `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`)
	if _, err := c.Closes(context.Background(), "GONE", time.Now().AddDate(0, 0, -5), time.Now()); err == nil {
		t.Error("a chart error answered as no closes")
	}
}

// Dividends are read from the chart's events: so much per share on the
// ex-date, the exchange's day, in whole units of the currency.
func TestDividendsAreReadFromTheChartsEvents(t *testing.T) {
	// Coca-Cola, 2024 (recorded 2026-10-06), and a London share in pence.
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			"dollars", `{"chart":{"result":[{"meta":{"currency":"USD","exchangeTimezoneName":"America/New_York"},
			"events":{"dividends":{"1717214400":{"amount":0.485,"date":1718371800},"1709269200":{"amount":0.485,"date":1710423000}}}}],"error":null}}`,
			"2024-03-14 0.485 USD, 2024-06-14 0.485 USD",
		},
		{
			"pence", `{"chart":{"result":[{"meta":{"currency":"GBp","exchangeTimezoneName":"Europe/London"},
			"events":{"dividends":{"1":{"amount":7.5,"date":1710403200}}}}],"error":null}}`,
			"2024-03-14 0.075 GBP",
		},
		{"none", `{"chart":{"result":[{"meta":{"currency":"USD","exchangeTimezoneName":"America/New_York"}}],"error":null}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serve(t, tc.body).Dividends(context.Background(), "KO",
				time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for _, d := range got {
				parts = append(parts, d.RecordDate.Format(time.DateOnly)+" "+d.PerShare.String()+" "+d.Currency)
				if d.Source != "yahoo" {
					t.Errorf("source = %q, want yahoo", d.Source)
				}
			}
			if s := strings.Join(parts, ", "); s != tc.want {
				t.Errorf("dividends = %q, want %q", s, tc.want)
			}
		})
	}
}

// Splits are read from the chart's events, dated by the exchange's day.
func TestSplitsAreReadFromTheChartsEvents(t *testing.T) {
	// Amazon, June 2022 (recorded 2026-10-06).
	body := `{"chart":{"result":[{"meta":{"currency":"USD","exchangeTimezoneName":"America/New_York"},
		"events":{"splits":{"1654056000":{"date":1654522200,"numerator":20.0,"denominator":1.0,"splitRatio":"20:1"}}}}],"error":null}}`
	got, err := serve(t, body).Splits(context.Background(), "AMZN",
		time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].On.Format(time.DateOnly) != "2022-06-06" || got[0].Numerator.String() != "20" || got[0].Denominator.String() != "1" {
		t.Errorf("splits = %+v, want 20:1 on 2022-06-06", got)
	}
}
