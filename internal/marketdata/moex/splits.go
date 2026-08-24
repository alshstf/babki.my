package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Split is one dividing of a security the exchange has published: on
// EffectiveOn, one unit of SecID became To/From units.
//
// SECID AND NOT AN ISIN, because that is all this table carries. The exchange
// names securities by its own code here, and this program identifies them by
// ISIN (migration 0020) — so a caller has to resolve one to the other, and
// ISINBySecID is how. It must not be done through this program's own catalog:
// MOEX's "T" is Т-Технологии, and this catalog also holds AT&T under "T", so
// matching by ticker would file Т-Технологии's ten-for-one split against AT&T.
type Split struct {
	SecID       string
	EffectiveOn time.Time
	From, To    int64
}

// splitsPath is the exchange's whole splits table. It is not paged and not
// filterable: measured on 2026-08-22 it holds 56 rows covering 2021 to 2026,
// about 3 KB, so the job reads all of it every time and lets the upsert decide
// what is new.
const splitsPath = "/iss/statistics/engines/stock/splits.json"

// Splits returns every dividing the exchange publishes, oldest first.
//
// WHAT THE DATE MEANS IS NOT CONSISTENT IN THIS TABLE, and a caller has to know
// it. Checked against ISS's own daily history on 2026-08-22:
//
//   - FXUS is listed as 2021-10-06. Its last pre-split trade was 2021-10-04 at
//     5746 ₽, 2021-10-05 and 2021-10-06 have no trades at all, and 2021-10-07
//     traded at 58.05 ₽ — one hundredth. So the date here is the last day of
//     the halt, and the new denomination started the day after.
//   - NVDA-RM is listed as 2021-07-21: last trade 2021-07-15 at 57550, no
//     trades through the 21st, 2021-07-22 at 14460 — a quarter. Again the last
//     halted day. (In the United States the same split was effective 2021-07-20,
//     two days earlier, which is why an event's date belongs to the VENUE the
//     paper is held at rather than to the company.)
//   - T (Т-Технологии) is listed as 2026-04-17, which MOEX's own notice
//     n99222 gives as the day trading RESUMED after the conversion.
//
// Every one of the three falls inside a window with no trades in it, which is
// what makes the registry's rule — apply at the START of the stored day — give
// the right answer for all three whichever end of the window the exchange
// happened to name.
//
// Rows this code cannot read are skipped rather than failing the call: the
// table mixes tickers with ISIN-shaped codes (RU000A0JP799 is in it) and
// nothing says a future row will hold two whole numbers. A row dropped is one
// split not recorded, which the caller can see in the count; a call failed is
// every split not recorded.
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
	// The ratio halves are whole numbers and are read as such; UseNumber keeps
	// them exact rather than routing 5000 through a float64, for the same
	// reason every other decoder in this package uses it.
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("moex: splits: decode: %w", err)
	}

	index := make(map[string]int, len(body.Splits.Columns))
	for i, name := range body.Splits.Columns {
		index[name] = i
	}
	// Every column is required, and a missing one fails the whole call rather
	// than emptying it quietly: without SECID there is no security, without
	// TRADEDATE no day, and without the pair no ratio. An answer with no usable
	// column is a changed format, and reporting it as "no splits published"
	// would leave every holding un-split with nothing saying why.
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

// wholeNumber reads a ratio half. json.Number because the decoder is in
// UseNumber mode; a value with a fractional part is refused rather than
// truncated — the exchange publishes these as whole numbers, and one that is
// not is a row this code does not understand.
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

// ISINBySecID asks the exchange what security a secid names and returns its
// ISIN.
//
// THIS IS THE ONLY HONEST WAY FROM ONE TO THE OTHER. A secid is the exchange's
// own code and a ticker is not an identity — the whole reason this program's
// catalog moved to ISINs (migration 0020) is that "T" names two different
// companies on two exchanges. Resolving through the local catalog would attach
// Т-Технологии's split to AT&T on the owner's own data.
//
// An empty ISIN comes back as an empty string with no error: ISS answers for
// futures, indices and other instruments that have none, and a caller has to
// tell "this security has no ISIN" apart from "the exchange would not answer".
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

	// The description block is a list of (name, title, value, type) rows rather
	// than an object, so the ISIN is found by looking for the row NAMED "ISIN"
	// — never by position, which ISS does not promise.
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
