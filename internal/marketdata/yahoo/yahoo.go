// Package yahoo reads foreign shares' prices on their home exchanges from
// Yahoo Finance (decision Р-11): a share is found by its ISIN, and its daily
// closes are read by the symbol found. The API is public but unofficial and
// needs no key; a change on its side shows as a failed job, never a figure.
package yahoo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

const (
	DefaultSearchURL = "https://query2.finance.yahoo.com/v1/finance/search"
	DefaultChartURL  = "https://query1.finance.yahoo.com/v8/finance/chart"
)

// userAgent names this program; Yahoo refuses requests that send none.
const userAgent = "Mozilla/5.0 (compatible; babki.my/1.0; +https://github.com/alshstf/babki.my)"

const maxBody = 8 << 20

type Client struct {
	http      *http.Client
	searchURL string
	chartURL  string
}

// New builds a client; an empty baseURL is Yahoo's own hosts (a test server
// answers both paths under one base), a nil client http.DefaultClient.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		return &Client{http: client, searchURL: DefaultSearchURL, chartURL: DefaultChartURL}
	}
	base := strings.TrimRight(baseURL, "/")
	return &Client{http: client, searchURL: base + "/v1/finance/search", chartURL: base + "/v8/finance/chart"}
}

func (c *Client) Name() string { return "yahoo" }

type wireSearch struct {
	Quotes []struct {
		Symbol    string `json:"symbol"`
		QuoteType string `json:"quoteType"`
	} `json:"quotes"`
}

// SymbolFor is the symbol of the first share the search finds for isin.
func (c *Client) SymbolFor(ctx context.Context, isin string) (string, bool, error) {
	q := url.Values{"q": {isin}, "quotesCount": {"5"}, "newsCount": {"0"}}
	var resp wireSearch
	if err := c.get(ctx, c.searchURL+"?"+q.Encode(), &resp); err != nil {
		return "", false, err
	}
	for _, r := range resp.Quotes {
		if r.QuoteType == "EQUITY" && r.Symbol != "" {
			return r.Symbol, true, nil
		}
	}
	return "", false, nil
}

type wireChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency             string `json:"currency"`
				ExchangeTimezoneName string `json:"exchangeTimezoneName"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Close []*float64 `json:"close"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// subunits are the currencies Yahoo quotes in hundredths: pence, agorot,
// cents of the rand.
var subunits = map[string]string{"GBp": "GBP", "GBX": "GBP", "ILA": "ILS", "ZAc": "ZAR"}

// Closes is the symbol's daily closes from from to to, oldest first, each
// dated by the exchange's own calendar day. A day with no close is left out.
func (c *Client) Closes(ctx context.Context, symbol string, from, to time.Time) ([]marketdata.DayPrice, error) {
	q := url.Values{
		"interval": {"1d"},
		"period1":  {strconv.FormatInt(from.Unix(), 10)},
		"period2":  {strconv.FormatInt(to.AddDate(0, 0, 1).Unix(), 10)},
	}
	var resp wireChart
	if err := c.get(ctx, c.chartURL+"/"+url.PathEscape(symbol)+"?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	if resp.Chart.Error != nil {
		return nil, fmt.Errorf("yahoo: %s: %s: %s", symbol, resp.Chart.Error.Code, resp.Chart.Error.Description)
	}
	if len(resp.Chart.Result) == 0 {
		return nil, nil
	}
	r := resp.Chart.Result[0]
	currency, scale := r.Meta.Currency, int32(0)
	if whole, ok := subunits[currency]; ok {
		currency, scale = whole, -2
	}
	if len(currency) != 3 || strings.ToUpper(currency) != currency {
		return nil, fmt.Errorf("yahoo: %s: currency %q is not a currency code", symbol, r.Meta.Currency)
	}
	loc, err := time.LoadLocation(r.Meta.ExchangeTimezoneName)
	if err != nil {
		return nil, fmt.Errorf("yahoo: %s: exchange time zone %q: %w", symbol, r.Meta.ExchangeTimezoneName, err)
	}
	if len(r.Indicators.Quote) == 0 {
		return nil, nil
	}
	closes := r.Indicators.Quote[0].Close
	out := make([]marketdata.DayPrice, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		if i >= len(closes) || closes[i] == nil || *closes[i] <= 0 {
			continue
		}
		local := time.Unix(ts, 0).In(loc)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		// A float close carries binary noise past any exchange's tick.
		price := decimal.NewFromFloat(*closes[i]).Round(4).Shift(scale)
		out = append(out, marketdata.DayPrice{Day: day, Price: price, Currency: currency})
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("yahoo: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("yahoo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("yahoo: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("yahoo: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("yahoo: decode: %w", err)
	}
	return nil
}
