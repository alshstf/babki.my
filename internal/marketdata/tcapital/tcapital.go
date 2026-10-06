// Package tcapital reads the unit values T-Capital publishes for its closed
// funds of blocked assets (TECH2, TSPX2, TUSD2 and the like): they trade
// nowhere, and the management company states each unit's fair value once a
// month (issue #334, decision Р-11). The figures are the company's own
// disclosure page, whose data travels inside it as JSON; a change on its side
// shows as a failed job, never a figure.
package tcapital

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// DefaultURL is the company's statistics page.
const DefaultURL = "https://t-capital-funds.ru/statistics/"

const (
	userAgent = "Mozilla/5.0 (compatible; babki.my/1.0; +https://github.com/alshstf/babki.my)"
	maxBody   = 8 << 20
	// stateMarker opens the script that carries the page's data.
	stateMarker = `<script id="__REACT_QUERY_STATE__capital" type="application/json">`
	// closedFund is how a closed fund's name starts; open and exchange-traded
	// funds have market prices and are left to them.
	closedFund = "ЗПИФ"
	// pageLife is how long one reading of the page serves a job's run.
	pageLife = 10 * time.Minute
)

type Client struct {
	http *http.Client
	url  string

	mu     sync.Mutex
	funds  map[string]fund
	readAt time.Time
}

// fund is one closed fund's published unit values.
type fund struct {
	ticker   string
	currency string
	values   []marketdata.DayPrice
}

// New builds a client; an empty url is the company's own page, a nil client
// http.DefaultClient.
func New(client *http.Client, url string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if url == "" {
		url = DefaultURL
	}
	return &Client{http: client, url: url}
}

func (c *Client) Name() string { return "tcapital" }

// Funds is every closed fund with a published unit value, ticker by ISIN.
func (c *Client) Funds(ctx context.Context) (map[string]string, error) {
	funds, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(funds))
	for isin, f := range funds {
		out[isin] = f.ticker
	}
	return out, nil
}

// NAVHistory is the fund's published unit values from from on, oldest first:
// the page states the last two month ends.
func (c *Client) NAVHistory(ctx context.Context, ticker string, from time.Time) ([]marketdata.DayPrice, error) {
	funds, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range funds {
		if f.ticker != ticker {
			continue
		}
		var out []marketdata.DayPrice
		for _, v := range f.values {
			if !v.Day.Before(from) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	return nil, nil
}

// read is the page's closed funds, read at most once per pageLife.
func (c *Client) read(ctx context.Context) (map[string]fund, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.funds != nil && time.Since(c.readAt) < pageLife {
		return c.funds, nil
	}
	page, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	funds, err := parse(page)
	if err != nil {
		return nil, err
	}
	c.funds, c.readAt = funds, time.Now()
	return funds, nil
}

// wireFund is a fund as the page's data states it.
type wireFund struct {
	Name     string `json:"name"`
	Ticker   string `json:"ticker"`
	ISIN     string `json:"isin"`
	Currency string `json:"currency"`
	Metrics  []struct {
		Type string  `json:"type"`
		Last wireDay `json:"lastClosedDay"`
		Prev wireDay `json:"previousClosedDay"`
	} `json:"metrics"`
}

type wireDay struct {
	Date  string `json:"date"`
	Value string `json:"value"`
}

// parse finds every fund in the page's data, wherever the data nests it, and
// keeps the closed ones with a fair unit value.
func parse(page string) (map[string]fund, error) {
	start := strings.Index(page, stateMarker)
	if start < 0 {
		return nil, fmt.Errorf("tcapital: the page carries no data")
	}
	body := page[start+len(stateMarker):]
	end := strings.Index(body, "</script>")
	if end < 0 {
		return nil, fmt.Errorf("tcapital: the page's data is not closed")
	}
	var state any
	if err := json.Unmarshal([]byte(body[:end]), &state); err != nil {
		return nil, fmt.Errorf("tcapital: decode the page's data: %w", err)
	}
	out := map[string]fund{}
	walk(state, func(raw map[string]any) {
		if _, ok := raw["isin"]; !ok {
			return
		}
		if _, ok := raw["metrics"]; !ok {
			return
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return
		}
		var w wireFund
		if json.Unmarshal(encoded, &w) != nil || !strings.HasPrefix(w.Name, closedFund) || w.ISIN == "" {
			return
		}
		f := fund{ticker: w.Ticker, currency: strings.ToUpper(w.Currency)}
		for _, m := range w.Metrics {
			if m.Type != "fairPrice" {
				continue
			}
			for _, d := range []wireDay{m.Prev, m.Last} {
				if v, ok := dayValue(d, f.currency); ok {
					f.values = append(f.values, v)
				}
			}
		}
		if len(f.values) > 0 {
			out[strings.ToUpper(w.ISIN)] = f
		}
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("tcapital: the page names no closed fund with a unit value")
	}
	return out, nil
}

// dayValue reads one published value; the page groups thousands with spaces.
func dayValue(d wireDay, currency string) (marketdata.DayPrice, bool) {
	day, err := time.Parse(time.DateOnly, d.Date)
	if err != nil {
		return marketdata.DayPrice{}, false
	}
	value, err := decimal.NewFromString(strings.ReplaceAll(d.Value, " ", ""))
	if err != nil || !value.IsPositive() {
		return marketdata.DayPrice{}, false
	}
	return marketdata.DayPrice{Day: day, Price: value, Currency: currency}, true
}

// walk calls visit on every object in v, however deep.
func walk(v any, visit func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		visit(t)
		for _, child := range t {
			walk(child, visit)
		}
	case []any:
		for _, child := range t {
			walk(child, visit)
		}
	}
}

func (c *Client) get(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return "", fmt.Errorf("tcapital: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tcapital: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tcapital: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("tcapital: read: %w", err)
	}
	return string(body), nil
}
