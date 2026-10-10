package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"babki.my/babki/internal/marketdata"
)

// IndexHistory is an index's daily closes in [from, till], oldest first, from
// the exchange's index market (MCFTR, RGBITR), in roubles. Pages are walked
// until the cursor is exhausted.
func (c *Client) IndexHistory(ctx context.Context, symbol string, from, till time.Time) ([]marketdata.DayPrice, error) {
	var out []marketdata.DayPrice
	for start := 0; ; {
		q := url.Values{
			"from": {from.Format(time.DateOnly)}, "till": {till.Format(time.DateOnly)}, "start": {strconv.Itoa(start)},
			"iss.meta": {"off"}, "history.columns": {"TRADEDATE,CLOSE"},
		}
		path := fmt.Sprintf("/iss/history/engines/stock/markets/index/securities/%s.json?%s", url.PathEscape(symbol), q.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("moex: index %s: build request: %w", symbol, err)
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("moex: index %s: request: %w", symbol, err)
		}
		var body struct {
			History struct {
				Columns []string `json:"columns"`
				Data    [][]any  `json:"data"`
			} `json:"history"`
			Cursor struct {
				Data [][]any `json:"data"`
			} `json:"history.cursor"`
		}
		dec := json.NewDecoder(resp.Body)
		dec.UseNumber()
		err = dec.Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("moex: index %s: unexpected status %d", symbol, resp.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("moex: index %s: decode: %w", symbol, err)
		}
		for _, row := range body.History.Data {
			if len(row) < 2 {
				continue
			}
			text, _ := row[0].(string)
			day, err := time.Parse(time.DateOnly, text)
			if err != nil {
				return nil, fmt.Errorf("moex: index %s: trade date %q: %w", symbol, text, err)
			}
			price, ok := positive(row[1])
			if !ok {
				continue
			}
			out = append(out, marketdata.DayPrice{Day: day, Price: price, Currency: "RUB"})
		}
		if len(body.Cursor.Data) == 0 || len(body.Cursor.Data[0]) < 3 {
			return out, nil
		}
		index, _ := strconv.Atoi(fmt.Sprint(body.Cursor.Data[0][0]))
		total, _ := strconv.Atoi(fmt.Sprint(body.Cursor.Data[0][1]))
		size, _ := strconv.Atoi(fmt.Sprint(body.Cursor.Data[0][2]))
		next := index + size
		if size <= 0 || next >= total || next <= start {
			return out, nil
		}
		start = next
	}
}
