package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Split is a split the exchange published: on EffectiveOn one unit of SecID
// became To/From units. It carries the exchange's SECID, not an ISIN; resolve
// it with ISINBySecID, never by ticker through the catalog ("T" is both
// Т-Технологии and AT&T).
type Split struct {
	SecID       string
	EffectiveOn time.Time
	From, To    int64
}

// splitsPath is the exchange's whole splits table: unpaged, 56 rows (~3 KB) on
// 2026-08-22, so it is read in full each time.
const splitsPath = "/iss/statistics/engines/stock/splits.json"

// Splits returns every split the exchange publishes, oldest first.
//
// The date is not consistent: for FXUS and NVDA-RM it is the last day of the
// trading halt, for T the day trading resumed. Each falls inside a window with
// no trades, so applying the event at the start of the stored day is right
// either way. (In the US the NVIDIA split was two days earlier: an event's date
// belongs to the venue.)
//
// Unreadable rows are skipped rather than failing the call.
func (c *Client) Splits(ctx context.Context) ([]Split, error) {
	url := c.baseURL + splitsPath + "?iss.meta=off"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("moex: splits: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moex: splits: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moex: splits: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Splits struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"splits"`
	}
	dec := json.NewDecoder(resp.Body)
	// Ratio halves are whole numbers; UseNumber keeps them exact.
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("moex: splits: decode: %w", err)
	}

	index := make(map[string]int, len(body.Splits.Columns))
	for i, name := range body.Splits.Columns {
		index[name] = i
	}
	// A missing column fails the call: it is a changed format, not "no splits".
	for _, name := range []string{"secid", "tradedate", "before", "after"} {
		if _, ok := index[name]; !ok {
			return nil, fmt.Errorf("moex: splits: response has no %s column", name)
		}
	}

	out := make([]Split, 0, len(body.Splits.Data))
	for _, row := range body.Splits.Data {
		s, ok := parseSplitRow(row, index)
		if !ok {
			c.log.Debug("moex: a row of the splits table could not be read", "row", fmt.Sprint(row))
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func parseSplitRow(row []any, index map[string]int) (Split, bool) {
	cell := func(name string) any {
		i, ok := index[name]
		if !ok || i >= len(row) {
			return nil
		}
		return row[i]
	}
	secid, ok := cell("secid").(string)
	if !ok || secid == "" {
		return Split{}, false
	}
	raw, ok := cell("tradedate").(string)
	if !ok {
		return Split{}, false
	}
	day, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return Split{}, false
	}
	from, ok := wholeNumber(cell("before"))
	if !ok || from < 1 {
		return Split{}, false
	}
	to, ok := wholeNumber(cell("after"))
	if !ok || to < 1 {
		return Split{}, false
	}
	return Split{SecID: secid, EffectiveOn: day, From: from, To: to}, true
}

// wholeNumber reads a ratio half, refusing a fractional value rather than
// truncating it.
func wholeNumber(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

// ISINBySecID asks the exchange which ISIN a secid names — the only reliable
// way, since tickers are not identities. An instrument with no ISIN (futures,
// indices) returns "" and no error.
func (c *Client) ISINBySecID(ctx context.Context, secid string) (string, error) {
	url := fmt.Sprintf("%s/iss/securities/%s.json?iss.meta=off&iss.only=description", c.baseURL, secid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("moex: %s: build request: %w", secid, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("moex: %s: request: %w", secid, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("moex: %s: unexpected status %d", secid, resp.StatusCode)
	}

	// The description is a list of named rows; find ISIN by name, not position.
	var body struct {
		Description struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("moex: %s: decode: %w", secid, err)
	}
	nameIdx, valueIdx := -1, -1
	for i, c := range body.Description.Columns {
		switch c {
		case "name":
			nameIdx = i
		case "value":
			valueIdx = i
		}
	}
	if nameIdx < 0 || valueIdx < 0 {
		return "", fmt.Errorf("moex: %s: description has no name/value columns", secid)
	}
	for _, row := range body.Description.Data {
		if nameIdx >= len(row) || valueIdx >= len(row) {
			continue
		}
		if n, _ := row[nameIdx].(string); n != "ISIN" {
			continue
		}
		isin, _ := row[valueIdx].(string)
		return isin, nil
	}
	return "", nil
}
