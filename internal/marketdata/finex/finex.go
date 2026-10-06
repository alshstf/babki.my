// Package finex reads the net asset value per unit of FinEx funds from the
// manager's public API (decision Р-11): the funds' list with their ISINs, and
// each fund's daily value since launch. The funds have not traded on an
// exchange since 2022; this is what their assets are worth, not a price they
// sell at.
package finex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the API the calculator on finex-etf.ru/calc/nav reads.
const DefaultBaseURL = "https://api.finex-etf.ru/v1"

const userAgent = "babki.my/1.0 (+https://github.com/alshstf/babki.my)"

// maxBody bounds a response: a fund's whole history is about 200 KB.
const maxBody = 8 << 20

type Client struct {
	http    *http.Client
	baseURL string
}

// New builds a client; an empty baseURL is DefaultBaseURL, a nil client
// http.DefaultClient.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{http: client, baseURL: strings.TrimRight(baseURL, "/")}
}

func (c *Client) Name() string { return "finex" }

type wireFund struct {
	Ticker       string `json:"ticker"`
	ISIN         string `json:"isin"`
	CurrencyFond string `json:"currency_fond"`
}

// Funds is each fund's ticker by ISIN. The ticker is what its history is kept
// under.
func (c *Client) Funds(ctx context.Context) (map[string]string, error) {
	var funds []wireFund
	if err := c.get(ctx, "/fonds/", &funds); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(funds))
	for _, f := range funds {
		isin := strings.ToUpper(strings.TrimSpace(f.ISIN))
		if isin != "" && f.Ticker != "" {
			out[isin] = f.Ticker
		}
	}
	return out, nil
}

type wireFundCurrency struct {
	CurrencyFond string `json:"currency_fond"`
}

type wirePoint struct {
	Date  string          `json:"date"`
	Value decimal.Decimal `json:"value"`
}

// NAVHistory is the fund's value per unit from from on, oldest first, in the
// fund's own currency.
func (c *Client) NAVHistory(ctx context.Context, ticker string, from time.Time) ([]marketdata.DayPrice, error) {
	var fund wireFundCurrency
	if err := c.get(ctx, "/fonds/"+url.PathEscape(ticker)+"/", &fund); err != nil {
		return nil, err
	}
	currency := strings.ToUpper(fund.CurrencyFond)
	if len(currency) != 3 {
		return nil, fmt.Errorf("finex: %s: fund currency %q is not a currency code", ticker, fund.CurrencyFond)
	}
	var points []wirePoint
	if err := c.get(ctx, "/fonds/"+url.PathEscape(ticker)+"/history/", &points); err != nil {
		return nil, err
	}
	out := make([]marketdata.DayPrice, 0, len(points))
	for _, p := range points {
		day, err := time.Parse(time.DateOnly, p.Date)
		if err != nil {
			return nil, fmt.Errorf("finex: %s: history date %q: %w", ticker, p.Date, err)
		}
		if day.Before(from) || !p.Value.IsPositive() {
			continue
		}
		out = append(out, marketdata.DayPrice{Day: day, Price: p.Value, Currency: currency})
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("finex: %s: %w", path, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("finex: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("finex: %s: read: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("finex: %s: status %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("finex: %s: decode: %w", path, err)
	}
	return nil
}
