package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/shopspring/decimal"
)

// Security is what the exchange says about a paper on a board this program
// prices: enough for a catalog entry.
type Security struct {
	SecID string
	ISIN  string
	Name  string
	// Kind is "share", "bond" or "etf" — the catalog's own words.
	Kind string
	// Currency is the currency the paper trades in on its board.
	Currency string
	// FaceValue and FaceCurrency are a bond's original face value and its
	// currency; zero and empty for anything else.
	FaceValue    decimal.Decimal
	FaceCurrency string
}

// kinds maps the exchange's security group to the catalog's kind. A
// depositary receipt is held and taxed as a share, as the brokers file it.
var kinds = map[string]string{
	"stock_shares": "share",
	"stock_dr":     "share",
	"stock_bonds":  "bond",
	"stock_etf":    "etf",
	"stock_ppif":   "etf",
}

// tradedBoards are the priced boards and their settlement currencies.
var tradedBoards = map[string]string{
	"TQBR": "RUB", "TQOB": "RUB", "TQCB": "RUB", "TQRD": "RUB", "TQTF": "RUB",
}

// FindSecurity looks a paper up by ticker or ISIN, answering only an exact
// match traded on a priced board.
func (c *Client) FindSecurity(ctx context.Context, code string) (Security, bool, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return Security{}, false, nil
	}
	rows, err := c.search(ctx, code)
	if err != nil {
		return Security{}, false, err
	}
	for _, row := range rows {
		currency, onBoard := tradedBoards[row.board]
		if !row.traded || row.kind == "" || !onBoard || (row.secid != code && row.isin != code) {
			continue
		}
		sec := Security{SecID: row.secid, ISIN: row.isin, Name: row.shortname, Kind: row.kind, Currency: currency}
		if err := c.withFace(ctx, &sec); err != nil {
			return Security{}, false, err
		}
		return sec, true, nil
	}
	return Security{}, false, nil
}

// RememberedByISIN is what the exchange's reference still says about a
// paper, traded or not: a receipt of a company that moved to Russia, a fund
// wound up. The name is the full one, since no board's ticker is current;
// Currency is left empty, the paper having no board that trades it (the caller
// knows what its operations were paid in). Decision Р-19: a paper the broker
// forgot is created from this.
func (c *Client) RememberedByISIN(ctx context.Context, isin string) (Security, bool, error) {
	isin = strings.ToUpper(strings.TrimSpace(isin))
	if isin == "" {
		return Security{}, false, nil
	}
	rows, err := c.search(ctx, isin)
	if err != nil {
		return Security{}, false, err
	}
	for _, row := range rows {
		if row.isin != isin || row.kind == "" {
			continue
		}
		name := row.name
		if name == "" {
			name = row.shortname
		}
		sec := Security{SecID: row.secid, ISIN: row.isin, Name: name, Kind: row.kind}
		if err := c.withFace(ctx, &sec); err != nil {
			return Security{}, false, err
		}
		return sec, true, nil
	}
	return Security{}, false, nil
}

// found is one row of the exchange's search, with the catalog's kind ("" for
// a group this program does not hold, an index among them).
type found struct {
	secid, isin, shortname, name, kind, board string
	traded                                    bool
}

// search asks the exchange for every paper whose name, ticker or ISIN holds
// code — indices, delisted issues, every board among them.
func (c *Client) search(ctx context.Context, code string) ([]found, error) {
	var body struct {
		Securities struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"securities"`
	}
	columns := []string{"secid", "shortname", "name", "isin", "group", "primary_boardid", "is_traded"}
	q := url.Values{
		"q":                  {code},
		"iss.meta":           {"off"},
		"securities.columns": {strings.Join(columns, ",")},
	}
	if err := c.getJSON(ctx, "/iss/securities.json?"+q.Encode(), code, &body); err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, name := range body.Securities.Columns {
		col[name] = i
	}
	for _, name := range columns {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("moex: find %s: response missing %s", code, name)
		}
	}
	text := func(row []any, name string) string {
		s, _ := row[col[name]].(string)
		return s
	}
	var out []found
	for _, row := range body.Securities.Data {
		if len(row) < len(body.Securities.Columns) {
			continue
		}
		traded, _ := row[col["is_traded"]].(float64)
		out = append(out, found{
			secid:     strings.ToUpper(text(row, "secid")),
			isin:      strings.ToUpper(text(row, "isin")),
			shortname: text(row, "shortname"),
			name:      text(row, "name"),
			kind:      kinds[text(row, "group")],
			board:     text(row, "primary_boardid"),
			traded:    traded == 1,
		})
	}
	return out, nil
}

// withFace adds a bond's original face value and its currency.
func (c *Client) withFace(ctx context.Context, sec *Security) error {
	if sec.Kind != "bond" {
		return nil
	}
	return c.bondFace(ctx, sec)
}

// bondFace reads a bond's original face value and currency; the exchange
// writes roubles as SUR.
func (c *Client) bondFace(ctx context.Context, sec *Security) error {
	var body struct {
		Description struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"description"`
	}
	path := fmt.Sprintf("/iss/securities/%s.json?iss.meta=off&iss.only=description&description.columns=name,value",
		url.PathEscape(sec.SecID))
	if err := c.getJSON(ctx, path, sec.SecID, &body); err != nil {
		return err
	}
	values := map[string]string{}
	for _, row := range body.Description.Data {
		if len(row) < 2 {
			continue
		}
		name, _ := row[0].(string)
		value, _ := row[1].(string)
		values[name] = value
	}
	face := values["INITIALFACEVALUE"]
	if face == "" {
		face = values["FACEVALUE"]
	}
	if face == "" {
		return nil
	}
	d, err := decimal.NewFromString(face)
	if err != nil {
		return fmt.Errorf("moex: %s: face value %q: %w", sec.SecID, face, err)
	}
	sec.FaceValue = d
	sec.FaceCurrency = normalizeCurrency(values["FACEUNIT"])
	return nil
}

func (c *Client) getJSON(ctx context.Context, path, what string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("moex: %s: build request: %w", what, err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("moex: %s: request: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("moex: %s: unexpected status %d", what, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("moex: %s: decode: %w", what, err)
	}
	return nil
}
