// Package coingecko prices cryptocurrencies from CoinGecko's public API, which
// needs no key (decision Р-20): a coin is found by its ticker, and its daily
// prices are read for the last year, which is as far back as the keyless API
// answers. CoinGecko asks to be named wherever its prices are shown.
package coingecko

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

// DefaultBaseURL is CoinGecko's public API.
const DefaultBaseURL = "https://api.coingecko.com/api/v3"

// HistoryDays is how far back the keyless API gives prices; a longer range is
// refused (error 10012).
const HistoryDays = 364

const (
	userAgent = "babki.my/1.0 (+https://github.com/alshstf/babki.my)"
	maxBody   = 8 << 20
)

type Client struct {
	http *http.Client
	base string
}

// New builds a client; an empty baseURL is CoinGecko's own, a nil client
// http.DefaultClient.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{http: client, base: strings.TrimRight(baseURL, "/")}
}

func (c *Client) Name() string { return "coingecko" }

// CoinFor is the coin a ticker most likely names: of the coins whose symbol is
// spelled exactly so, the one with the largest market capitalisation. Tickers
// are not unique («BTC» is also a dozen wrapped and lookalike coins), which is
// why the choice is stored and a person may correct it.
func (c *Client) CoinFor(ctx context.Context, ticker string) (string, bool, error) {
	ticker = strings.TrimSpace(ticker)
	if ticker == "" {
		return "", false, nil
	}
	var body struct {
		Coins []struct {
			ID     string `json:"id"`
			Symbol string `json:"symbol"`
			Rank   *int   `json:"market_cap_rank"`
		} `json:"coins"`
	}
	if err := c.get(ctx, "/search?query="+url.QueryEscape(ticker), &body); err != nil {
		return "", false, err
	}
	best, bestRank := "", 0
	for _, coin := range body.Coins {
		if !strings.EqualFold(coin.Symbol, ticker) || coin.ID == "" {
			continue
		}
		rank := int(^uint(0) >> 1)
		if coin.Rank != nil {
			rank = *coin.Rank
		}
		if best == "" || rank < bestRank {
			best, bestRank = coin.ID, rank
		}
	}
	return best, best != "", nil
}

// History is the coin's daily price in currency from from to till, oldest
// first. The API dates each daily price at midnight UTC: the price the day
// ended on, so it is filed under the day before.
func (c *Client) History(ctx context.Context, coin, currency string, from, till time.Time) ([]marketdata.DayPrice, error) {
	var body struct {
		Prices [][2]float64 `json:"prices"`
	}
	q := url.Values{
		"vs_currency": {strings.ToLower(currency)},
		"from":        {strconv.FormatInt(from.Unix(), 10)},
		"to":          {strconv.FormatInt(till.Unix(), 10)},
	}
	if err := c.get(ctx, "/coins/"+url.PathEscape(coin)+"/market_chart/range?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	var out []marketdata.DayPrice
	seen := map[time.Time]bool{}
	for _, p := range body.Prices {
		at := time.UnixMilli(int64(p[0])).UTC()
		if at.Hour() != 0 || at.Minute() != 0 {
			// The last point is «now», not a day's end; today's price is Current's.
			continue
		}
		day := at.AddDate(0, 0, -1).Truncate(24 * time.Hour)
		price := decimal.NewFromFloat(p[1])
		if seen[day] || !price.IsPositive() {
			continue
		}
		seen[day] = true
		out = append(out, marketdata.DayPrice{Day: day, Price: price, Currency: strings.ToUpper(currency)})
	}
	return out, nil
}

// Current is the coins' latest prices in currency, by coin, with the moment
// each was struck.
func (c *Client) Current(ctx context.Context, coins []string, currency string) (map[string]marketdata.DayPrice, error) {
	if len(coins) == 0 {
		return map[string]marketdata.DayPrice{}, nil
	}
	vs := strings.ToLower(currency)
	q := url.Values{"ids": {strings.Join(coins, ",")}, "vs_currencies": {vs}, "include_last_updated_at": {"true"}}
	var body map[string]map[string]json.Number
	if err := c.get(ctx, "/simple/price?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	out := make(map[string]marketdata.DayPrice, len(body))
	for coin, fields := range body {
		price, err := decimal.NewFromString(fields[vs].String())
		if err != nil || !price.IsPositive() {
			continue
		}
		at, err := fields["last_updated_at"].Int64()
		if err != nil {
			continue
		}
		out[coin] = marketdata.DayPrice{
			Day: time.Unix(at, 0).UTC().Truncate(24 * time.Hour), Price: price, Currency: strings.ToUpper(currency),
		}
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("coingecko: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("coingecko: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("coingecko: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coingecko: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("coingecko: decode: %w", err)
	}
	return nil
}

// HistoryWindow is how far back History answers without a key.
func (c *Client) HistoryWindow() time.Duration { return HistoryDays * 24 * time.Hour }
