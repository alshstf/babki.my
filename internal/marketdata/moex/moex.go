// Package moex reads prices, gold rates, splits and bond schedules from the
// Moscow Exchange ISS.
package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// DefaultBaseURL is the Moscow Exchange ISS API root.
const DefaultBaseURL = "https://iss.moex.com"

// sourceName is the provider's Name and every quote's Source.
const sourceName = "moex"

// rubCurrencyID is ISS's code for roubles in CURRENCYID: the legacy SUR. Other
// codes are passed through.
const rubCurrencyID = "SUR"

// board is one ISS listing to query; ISS has no endpoint covering every
// instrument kind.
type board struct {
	// label identifies the board in error messages (e.g. "shares/TQBR").
	label string
	// path is the endpoint path appended to baseURL.
	path string
	// history is the path to one security's session history on the board.
	history string
}

// boards are queried in precedence order: when two report a ticker, the
// earlier wins. Checked against ISS on 2026-08-03:
//   - shares/TQBR carries shares, depositary receipts and funds; the fund board
//     TQTF is empty, so it is not queried.
//   - bonds/TQOB carries government bonds (OFZ); bonds/TQCB corporate bonds.
//   - bonds/TQRD carries a few dozen distressed listing-level-3 bonds, priced at
//     a few percent of a reduced face value; the prices are real quotes.
//
// The bond boards quote PREVPRICE in percent of face (ISS's `unit` is "%"),
// shares in money; another bonds-market board (such as the ETC ones) needs its
// own check before it is added. Boards that republish the same tickers in
// another currency or lot (SMAL, TQTY, TQOD, TQOY) are left out: a ticker does
// not say which settlement currency a holding is in. The remaining traded
// boards were empty or duplicated these.
var boards = []board{
	{
		label: "shares/TQBR", path: "/iss/engines/stock/markets/shares/boards/TQBR/securities.json",
		history: "/iss/history/engines/stock/markets/shares/boards/TQBR/securities/",
	},
	{
		label: "bonds/TQOB", path: "/iss/engines/stock/markets/bonds/boards/TQOB/securities.json",
		history: "/iss/history/engines/stock/markets/bonds/boards/TQOB/securities/",
	},
	{
		label: "bonds/TQCB", path: "/iss/engines/stock/markets/bonds/boards/TQCB/securities.json",
		history: "/iss/history/engines/stock/markets/bonds/boards/TQCB/securities/",
	},
	{
		label: "bonds/TQRD", path: "/iss/engines/stock/markets/bonds/boards/TQRD/securities.json",
		history: "/iss/history/engines/stock/markets/bonds/boards/TQRD/securities/",
	},
}

// requestedColumns limits each board response to what is parsed; columns are
// still mapped by name. PREVDATE dates the quote and costs about 50 KB a
// refresh.
const requestedColumns = "SECID,ISIN,PREVPRICE,PREVDATE,CURRENCYID"

// Client fetches instrument prices from the Moscow Exchange ISS API.
type Client struct {
	http    *http.Client
	baseURL string
	log     *slog.Logger

	// traded caches lastTradeDay's answers.
	mu     sync.Mutex
	traded map[tradedKey]tradedAnswer
	// schedules caches bonds' repayment schedules by ISIN.
	schedules map[string]cachedSchedule
}

// New returns a Client. A nil client means http.DefaultClient, an empty
// baseURL means DefaultBaseURL, and a nil log means slog.Default.
func New(client *http.Client, baseURL string, log *slog.Logger) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{http: client, baseURL: baseURL, log: log, traded: map[tradedKey]tradedAnswer{}, schedules: map[string]cachedSchedule{}}
}

// userAgent identifies this program to the exchange.
const userAgent = "babki.my/1.0 (+https://github.com/alshstf/babki.my)"

// Name implements marketdata.QuoteProvider.
func (c *Client) Name() string { return sourceName }

// QuotesFor queries every board and merges the results.
//
// PREVPRICE is the last trade of the previous main session, not a live price
// and not the official close (PREVLEGALCLOSEPRICE). For a security that did
// not trade, the exchange carries an older price into the session, so On is
// the last session, on or before PREVDATE, in which it actually traded (see
// lastTradeDay) — never a date this process picked (#90, #199). PREVDATE is
// the board's session date, shared by every row.
//
// Tickers absent from every board, or with a null PREVPRICE, are absent from
// the result. A board that fails fails the whole call: a partial result would
// read as "no price" for every instrument on the failed board.
//
// The first board that reports a usable price for a ticker wins; a null or
// undated price does not take the ticker's slot.
func (c *Client) QuotesFor(ctx context.Context, tickers []string) ([]marketdata.TickerQuote, error) {
	want := make(map[string]bool, len(tickers))
	for _, t := range tickers {
		want[t] = true
	}

	var quotes []marketdata.TickerQuote
	quoted := make(map[string]bool, len(tickers))
	for _, b := range boards {
		rows, err := c.fetchBoard(ctx, b)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			// Every queried board lists hundreds of securities every day, so zero rows
			// means the path no longer points at a live board (ISS answers 200 and an
			// empty array for a retired board). Warn and keep the other boards' prices.
			c.log.Warn("moex: board returned no securities at all, everything listed on it will have no price",
				"board", b.label, "path", b.path)
		}
		for _, row := range rows {
			if !want[row.ticker] || row.price == nil || quoted[row.ticker] {
				continue
			}
			if !row.price.IsPositive() {
				// ISS reports 0 for a suspended issue; drop it rather than value the holding
				// at nothing.
				c.log.Warn("moex: price is not positive, dropping it (this instrument keeps whatever earlier quote it already has)",
					"board", b.label, "ticker", row.ticker, "price", row.price.String())
				continue
			}
			if row.priceOn == nil {
				// ISS sent no readable date (it sends "0000-00-00" for a security listed that
				// morning). Drop the price rather than invent its day, and do not fail the
				// call over one row. Warn, with the raw cell, because a holding losing its
				// price would otherwise go unnoticed.
				c.log.Warn("moex: price came without a readable date, dropping it (this instrument keeps whatever earlier quote it already has)",
					"board", b.label, "ticker", row.ticker,
					"prevdate", row.priceOnRaw, "price", row.price.String())
				continue
			}
			// Asked only for published rows. A failure fails the call: the alternative is
			// dating the price by the session, known to be wrong for exactly these.
			on, err := c.lastTradeDay(ctx, b, row.ticker, *row.priceOn)
			if err != nil {
				return nil, err
			}
			quoted[row.ticker] = true
			quotes = append(quotes, marketdata.TickerQuote{
				Ticker:   row.ticker,
				ISIN:     row.isin,
				Price:    *row.price,
				Currency: normalizeCurrency(row.currency),
				On:       on,
			})
		}
	}

	return quotes, nil
}

func normalizeCurrency(currencyID string) string {
	if currencyID == rubCurrencyID {
		return "RUB"
	}
	return currencyID
}

// secRow is one parsed securities row; price is nil when PREVPRICE was null.
type secRow struct {
	ticker string
	price  *decimal.Decimal
	// priceOn is PREVDATE as a UTC-midnight day, nil when unreadable — a pointer
	// so "no date" is not mistaken for year 1.
	priceOn *time.Time
	// priceOnRaw is the PREVDATE cell as sent, for the log line.
	priceOnRaw string
	currency   string
	// isin identifies the security rather than the listing; empty when ISS sends
	// none.
	isin string
}

// parsePrevDate reads a PREVDATE cell as a UTC-midnight day, plus the raw cell
// for logging. A nil day means unreadable: "0000-00-00" and a changed format
// look the same here. The zero day "0001-01-01" is rejected too.
func parsePrevDate(raw any) (*time.Time, string) {
	text, ok := raw.(string)
	if !ok {
		// Includes JSON null.
		return nil, fmt.Sprintf("%v", raw)
	}
	day, err := time.Parse(time.DateOnly, text)
	if err != nil || day.IsZero() {
		return nil, text
	}
	return &day, text
}

// issSecuritiesResponse is ISS's columns-plus-rows securities block.
type issSecuritiesResponse struct {
	Securities struct {
		Columns []string `json:"columns"`
		Data    [][]any  `json:"data"`
	} `json:"securities"`
}

// fetchBoard requests and parses one board. iss.only=securities drops the
// marketdata blocks, which would be eighteen times larger.
func (c *Client) fetchBoard(ctx context.Context, b board) ([]secRow, error) {
	url := c.baseURL + b.path + "?iss.meta=off&iss.only=securities&securities.columns=" + requestedColumns
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: build request: %w", b.label, err)
	}

	// Name ourselves: a public feed is entitled to know who is asking, and the
	// CBR refuses Go's default agent.
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: request: %w", b.label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moex: %s: unexpected status %d", b.label, resp.StatusCode)
	}

	// UseNumber keeps the exact digits: prices are money.
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()

	var doc issSecuritiesResponse
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("moex: %s: decode response: %w", b.label, err)
	}

	return parseSecurities(b.label, doc.Securities.Columns, doc.Securities.Data)
}

// parseSecurities maps ISS's positional rows into secRow by column name; ISS
// does not guarantee column order.
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
	dateIdx, ok := index["PREVDATE"]
	if !ok {
		// Without PREVDATE every price would be dated by something other than the
		// exchange (#90), so the whole board fails.
		return nil, fmt.Errorf("moex: %s: response missing PREVDATE column", boardLabel)
	}
	currencyIdx, ok := index["CURRENCYID"]
	if !ok {
		return nil, fmt.Errorf("moex: %s: response missing CURRENCYID column", boardLabel)
	}
	// ISIN is optional: without it a price matches its catalog row by ticker
	// alone.
	isinIdx, hasISIN := index["ISIN"]

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
		if hasISIN {
			row.isin, _ = fields[isinIdx].(string)
		}
		row.priceOn, row.priceOnRaw = parsePrevDate(fields[dateIdx])

		// PREVPRICE is null when the exchange has no price at all on the board; the
		// ticker is simply absent from the result.
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

// goldSecurity is the exchange's spot gold, quoted per GRAM, and goldBoard
// the board its trading happens on. The broker's code for it is "XAU", which in
// ISO 4217 is a troy ounce; the owner's purchases are in grams, so the XAU rate
// must come from this instrument (8422.2 ₽ a gram on 2024-10-21, against ~260
// 000 ₽ an ounce). The CBR publishes no gold rate.
const (
	goldSecurity = "GLDRUB_TOM"
	goldBoard    = "CETS"
)

// GoldRates returns the CETS closing price of a gram of gold for each trading
// day in [from, to]. Other boards report zeros or a few trades. A day with no
// positive close is left out: a zero rate would also answer for later days.
func (c *Client) GoldRates(ctx context.Context, from, to time.Time) ([]marketdata.FxRate, error) {
	// ISS returns at most a hundred rows a page and says so only in its cursor
	// block. The cursor's TOTAL ends the loop: a server ignoring `start` would
	// otherwise return the same page forever.
	var out []marketdata.FxRate
	for start := 0; ; {
		page, err := c.goldPage(ctx, from, to, start)
		if err != nil {
			return nil, err
		}
		out = append(out, page.rates...)
		start += page.rows
		if page.rows == 0 || start >= page.total {
			return out, nil
		}
	}
}

// goldPageResult is one page: its rates, how many raw rows it held (four
// boards answer, one is read), and the answer's total.
type goldPageResult struct {
	rates []marketdata.FxRate
	rows  int
	total int
}

// goldPage fetches one page and its cursor block.
func (c *Client) goldPage(ctx context.Context, from, to time.Time, start int) (goldPageResult, error) {
	url := fmt.Sprintf("%s/iss/history/engines/currency/markets/selt/securities/%s.json"+
		"?iss.meta=off&iss.only=history,history.cursor&history.columns=BOARDID,TRADEDATE,CLOSE&from=%s&till=%s&start=%d",
		c.baseURL, goldSecurity, from.Format(time.DateOnly), to.Format(time.DateOnly), start)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return goldPageResult{}, fmt.Errorf("moex: gold history: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		History struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"history"`
		Cursor struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"history.cursor"`
	}
	dec := json.NewDecoder(resp.Body)
	// Prices are money: keep ISS's exact digits.
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return goldPageResult{}, fmt.Errorf("moex: gold history: decode: %w", err)
	}

	// Without a cursor TOTAL the loop stops on an empty page, which is right for a
	// server that pages honestly.
	total := 0
	if len(body.Cursor.Data) > 0 {
		for i, name := range body.Cursor.Columns {
			if name != "TOTAL" || i >= len(body.Cursor.Data[0]) {
				continue
			}
			if num, ok := body.Cursor.Data[0][i].(json.Number); ok {
				if n, err := num.Int64(); err == nil {
					total = int(n)
				}
			}
		}
	}

	idx := make(map[string]int, len(body.History.Columns))
	for i, name := range body.History.Columns {
		idx[name] = i
	}
	boardAt, dateAt, closeAt := idx["BOARDID"], idx["TRADEDATE"], idx["CLOSE"]
	if boardAt < 0 || dateAt < 0 || closeAt < 0 {
		return goldPageResult{}, fmt.Errorf("moex: gold history: the answer names no %s/%s/%s column", "BOARDID", "TRADEDATE", "CLOSE")
	}

	out := make([]marketdata.FxRate, 0, len(body.History.Data))
	for _, row := range body.History.Data {
		if len(row) <= boardAt || len(row) <= dateAt || len(row) <= closeAt {
			continue
		}
		if board, _ := row[boardAt].(string); board != goldBoard {
			continue
		}
		day, ok := row[dateAt].(string)
		if !ok {
			continue
		}
		on, err := time.Parse(time.DateOnly, day)
		if err != nil {
			continue
		}
		num, ok := row[closeAt].(json.Number)
		if !ok {
			continue
		}
		price, err := decimal.NewFromString(num.String())
		if err != nil || !price.IsPositive() {
			continue
		}
		out = append(out, marketdata.FxRate{
			Base: marketdata.GoldCode, Quote: "RUB", On: on, Rate: price, Source: sourceName,
		})
	}
	return goldPageResult{rates: out, rows: len(body.History.Data), total: total}, nil
}
