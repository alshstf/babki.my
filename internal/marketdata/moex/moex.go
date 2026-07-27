// Package moex implements marketdata.QuoteProvider against the Moscow
// Exchange ISS (Informational & Statistical Server) securities endpoints.
package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Moscow Exchange ISS API root.
const DefaultBaseURL = "https://iss.moex.com"

// sourceName is used both as the provider's Name() and as Quote.Source for
// every quote this provider returns.
const sourceName = "moex"

// rubCurrencyID is the ISS code for Russian roubles on the CURRENCYID
// column. ISS reports it as "SUR" (a legacy ISO 4217 code for the pre-1998
// redenominated rouble) rather than the current "RUB"; every other
// CURRENCYID value observed on TQBR/TQOB is already a live ISO 4217 code and
// is passed through unchanged.
const rubCurrencyID = "SUR"

// board describes one ISS securities listing to query. ISS has no single
// endpoint covering all instrument kinds, so QuotesFor queries one board per
// kind and merges the results.
type board struct {
	// label identifies the board in error messages (e.g. "shares/TQBR").
	label string
	// path is the endpoint path appended to baseURL.
	path string
}

// boards lists every board QuotesFor queries. TQBR is the main equities
// board; TQOB is the main corporate/government bonds board.
var boards = []board{
	{label: "shares/TQBR", path: "/iss/engines/stock/markets/shares/boards/TQBR/securities.json"},
	{label: "bonds/TQOB", path: "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json"},
}

// requestedColumns is sent as the securities.columns query parameter on
// every board request, asking ISS to return only the columns this provider
// understands. ISS is free to return them in any order, so responses are
// still mapped by column name, not by position.
const requestedColumns = "SECID,PREVPRICE,CURRENCYID"

// Client fetches instrument prices from the Moscow Exchange ISS API.
type Client struct {
	http    *http.Client
	baseURL string
}

// New returns a Client. client may be nil, in which case http.DefaultClient
// is used; baseURL may be empty, in which case DefaultBaseURL is used.
// baseURL is parameterized (rather than hardcoded) so tests can point it at
// an httptest.Server instead of the real iss.moex.com.
func New(client *http.Client, baseURL string) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{http: client, baseURL: baseURL}
}

// Name implements marketdata.QuoteProvider.
func (c *Client) Name() string { return sourceName }

// QuotesFor implements marketdata.QuoteProvider. It queries both the shares
// (TQBR) and bonds (TQOB) boards and merges the results.
//
// ISS does not report a date for these boards (PREVPRICE is simply "the
// last traded price known to ISS right now"), so every returned quote's On
// field is set to the caller-supplied on rather than anything read from the
// response.
//
// Tickers not present on either board, and tickers whose PREVPRICE is null
// (no trade recorded), are silently absent from the result — not an error,
// per the marketdata.QuoteProvider contract.
func (c *Client) QuotesFor(ctx context.Context, tickers []string, on time.Time) ([]marketdata.TickerQuote, error) {
	want := make(map[string]bool, len(tickers))
	for _, t := range tickers {
		want[t] = true
	}

	var quotes []marketdata.TickerQuote
	for _, b := range boards {
		rows, err := c.fetchBoard(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !want[row.ticker] || row.price == nil {
				continue
			}
			quotes = append(quotes, marketdata.TickerQuote{
				Ticker:   row.ticker,
				Price:    *row.price,
				Currency: normalizeCurrency(row.currency),
				On:       on,
			})
		}
	}

	return quotes, nil
}

// normalizeCurrency maps ISS's "SUR" rouble code to the current ISO 4217
// "RUB" code; every other value is returned unchanged.
func normalizeCurrency(currencyID string) string {
	if currencyID == rubCurrencyID {
		return "RUB"
	}
	return currencyID
}

// secRow is one parsed row from an ISS securities response. price is nil
// when PREVPRICE was JSON null (no trade recorded for the instrument).
type secRow struct {
	ticker   string
	price    *decimal.Decimal
	currency string
}

// issSecuritiesResponse mirrors the shape ISS uses for securities.json
// endpoints: a "securities" block holding a column-name list and a matching
// list of positional data rows.
type issSecuritiesResponse struct {
	Securities struct {
		Columns []string `json:"columns"`
		Data    [][]any  `json:"data"`
	} `json:"securities"`
}

// fetchBoard requests and parses one board's securities.json response.
func (c *Client) fetchBoard(ctx context.Context, b board) ([]secRow, error) {
	url := c.baseURL + b.path + "?iss.meta=off&securities.columns=" + requestedColumns
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: build request: %w", b.label, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: request: %w", b.label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moex: %s: unexpected status %d", b.label, resp.StatusCode)
	}

	// UseNumber is required so JSON numbers decode as json.Number (the exact
	// digits ISS sent) rather than float64. Prices are money: unmarshaling
	// into float64 and converting to decimal.Decimal afterward would risk
	// losing precision for values with more significant digits than a
	// float64 mantissa can hold exactly.
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()

	var doc issSecuritiesResponse
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("moex: %s: decode response: %w", b.label, err)
	}

	return parseSecurities(b.label, doc.Securities.Columns, doc.Securities.Data)
}

// parseSecurities maps columns/data (ISS's positional row format) into
// secRow values by looking up each field's index from its column name.
// ISS does not guarantee column order, so indices are resolved once here
// rather than assumed to match requestedColumns.
func parseSecurities(boardLabel string, columns []string, data [][]any) ([]secRow, error) {
	index := make(map[string]int, len(columns))
	for i, name := range columns {
		index[name] = i
	}

	secidIdx, ok := index["SECID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing SECID column", boardLabel)
	}
	priceIdx, ok := index["PREVPRICE"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing PREVPRICE column", boardLabel)
	}
	currencyIdx, ok := index["CURRENCYID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing CURRENCYID column", boardLabel)
	}

	rows := make([]secRow, 0, len(data))
	for i, fields := range data {
		width := len(columns)
		if len(fields) < width {
			return nil, fmt.Errorf("moex: %s: row %d has %d fields, want %d", boardLabel, i, len(fields), width)
		}

		ticker, ok := fields[secidIdx].(string)
		if !ok {
			return nil, fmt.Errorf("moex: %s: row %d: SECID is not a string: %#v", boardLabel, i, fields[secidIdx])
		}

		currency, ok := fields[currencyIdx].(string)
		if !ok {
			return nil, fmt.Errorf("moex: %s: row %d (%s): CURRENCYID is not a string: %#v", boardLabel, i, ticker, fields[currencyIdx])
		}

		row := secRow{ticker: ticker, currency: currency}

		// PREVPRICE is nullable: ISS reports null when the instrument has
		// no last-traded price (e.g. newly listed, currently suspended).
		// That is a normal, expected condition, not an error — the caller
		// simply won't see this ticker in the result.
		if raw := fields[priceIdx]; raw != nil {
			num, ok := raw.(json.Number)
			if !ok {
				return nil, fmt.Errorf("moex: %s: row %d (%s): PREVPRICE is not a number: %#v", boardLabel, i, ticker, raw)
			}
			price, err := decimal.NewFromString(num.String())
			if err != nil {
				return nil, fmt.Errorf("moex: %s: row %d (%s): parse PREVPRICE %q: %w", boardLabel, i, ticker, num.String(), err)
			}
			row.price = &price
		}

		rows = append(rows, row)
	}

	return rows, nil
}
