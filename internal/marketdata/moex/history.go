package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// historyBoards are the boards closing prices are taken from: those the daily
// quote uses, plus TQTF, where funds traded before moving to TQBR.
var historyBoards = map[string]bool{"TQBR": true, "TQTF": true, "TQOB": true, "TQCB": true, "TQRD": true}

// History returns secid's closing prices in [from, till], oldest first, from
// market "shares" or "bonds". It takes the official close, else the last trade;
// a day with neither is left out. Bond prices are in percent of face. Pages
// are walked until the cursor is exhausted.
func (c *Client) History(ctx context.Context, market, secid string, from, till time.Time) ([]marketdata.DayPrice, error) {
	var out []marketdata.DayPrice
	for start := 0; ; {
		q := url.Values{
			"from":            {from.Format(time.DateOnly)},
			"till":            {till.Format(time.DateOnly)},
			"start":           {strconv.Itoa(start)},
			"iss.meta":        {"off"},
			"history.columns": {"BOARDID,TRADEDATE,CLOSE,LEGALCLOSEPRICE,CURRENCYID"},
		}
		path := fmt.Sprintf("/iss/history/engines/stock/markets/%s/securities/%s.json?%s",
			url.PathEscape(market), url.PathEscape(secid), q.Encode())
		page, cursor, err := c.historyPage(ctx, path, secid)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		next := cursor.index + cursor.pageSize
		if cursor.pageSize <= 0 || next >= cursor.total || next <= start {
			return out, nil
		}
		start = next
	}
}

type historyCursor struct{ index, total, pageSize int }

func (c *Client) historyPage(ctx context.Context, path, secid string) ([]marketdata.DayPrice, historyCursor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, historyCursor{}, fmt.Errorf("moex: history %s: build request: %w", secid, err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, historyCursor{}, fmt.Errorf("moex: history %s: request: %w", secid, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, historyCursor{}, fmt.Errorf("moex: history %s: unexpected status %d", secid, resp.StatusCode)
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
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, historyCursor{}, fmt.Errorf("moex: history %s: decode: %w", secid, err)
	}

	col := map[string]int{}
	for i, name := range body.History.Columns {
		col[name] = i
	}
	for _, name := range []string{"BOARDID", "TRADEDATE", "CLOSE", "LEGALCLOSEPRICE", "CURRENCYID"} {
		if _, ok := col[name]; !ok && len(body.History.Data) > 0 {
			return nil, historyCursor{}, fmt.Errorf("moex: history %s: response missing %s", secid, name)
		}
	}
	var out []marketdata.DayPrice
	for _, row := range body.History.Data {
		if len(row) < len(body.History.Columns) {
			continue
		}
		board, _ := row[col["BOARDID"]].(string)
		if !historyBoards[board] {
			continue
		}
		dayText, _ := row[col["TRADEDATE"]].(string)
		day, err := time.Parse(time.DateOnly, dayText)
		if err != nil {
			return nil, historyCursor{}, fmt.Errorf("moex: history %s: trade date %q: %w", secid, dayText, err)
		}
		price, ok := positive(row[col["LEGALCLOSEPRICE"]])
		if !ok {
			price, ok = positive(row[col["CLOSE"]])
		}
		if !ok {
			continue
		}
		currency, _ := row[col["CURRENCYID"]].(string)
		out = append(out, marketdata.DayPrice{Day: day, Price: price, Currency: normalizeCurrency(currency)})
	}

	var cursor historyCursor
	if len(body.Cursor.Data) > 0 {
		ccol := map[string]int{}
		for i, name := range body.Cursor.Columns {
			ccol[name] = i
		}
		number := func(name string) int {
			i, ok := ccol[name]
			if !ok || i >= len(body.Cursor.Data[0]) {
				return 0
			}
			n, _ := body.Cursor.Data[0][i].(json.Number)
			v, _ := n.Int64()
			return int(v)
		}
		cursor = historyCursor{index: number("INDEX"), total: number("TOTAL"), pageSize: number("PAGESIZE")}
	}
	return out, cursor, nil
}

// positive reads a price cell; ok is false for an empty or non-positive one.
func positive(cell any) (decimal.Decimal, bool) {
	n, ok := cell.(json.Number)
	if !ok {
		return decimal.Zero, false
	}
	d, err := decimal.NewFromString(n.String())
	if err != nil || !d.IsPositive() {
		return decimal.Zero, false
	}
	return d, true
}

// Locate finds the security an ISIN or a ticker names (see FindSecurity) and
// says which market its history is kept under.
func (c *Client) Locate(ctx context.Context, code string) (secid, market, currency string, ok bool, err error) {
	sec, ok, err := c.FindSecurity(ctx, code)
	if err != nil || !ok {
		return "", "", "", ok, err
	}
	market = "shares"
	if sec.Kind == "bond" {
		market = "bonds"
	}
	return sec.SecID, market, sec.Currency, true, nil
}
