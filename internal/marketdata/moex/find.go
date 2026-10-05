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

// kinds maps the exchange's security group to the catalog's kind.
var kinds = map[string]string{
	"stock_shares": "share",
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
	var found struct {
		Securities struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"securities"`
	}
	q := url.Values{
		"q":                  {code},
		"iss.meta":           {"off"},
		"securities.columns": {"secid,shortname,isin,group,primary_boardid,is_traded"},
	}
	if err := c.getJSON(ctx, "/iss/securities.json?"+q.Encode(), code, &found); err != nil {
		return Security{}, false, err
	}
	col := map[string]int{}
	for i, name := range found.Securities.Columns {
		col[name] = i
	}
	for _, name := range []string{"secid", "shortname", "isin", "group", "primary_boardid", "is_traded"} {
		if _, ok := col[name]; !ok {
			return Security{}, false, fmt.Errorf("moex: find %s: response missing %s", code, name)
		}
	}
	text := func(row []any, name string) string {
		s, _ := row[col[name]].(string)
		return s
	}
	for _, row := range found.Securities.Data {
		if len(row) < len(found.Securities.Columns) {
			continue
		}
		traded, _ := row[col["is_traded"]].(float64)
		secid, isin := strings.ToUpper(text(row, "secid")), strings.ToUpper(text(row, "isin"))
		kind, priced := kinds[text(row, "group")]
		currency, onBoard := tradedBoards[text(row, "primary_boardid")]
		if traded != 1 || !priced || !onBoard || (secid != code && isin != code) {
			continue
		}
		sec := Security{SecID: secid, ISIN: isin, Name: text(row, "shortname"), Kind: kind, Currency: currency}
		if kind == "bond" {
			if err := c.bondFace(ctx, &sec); err != nil {
				return Security{}, false, err
			}
		}
		return sec, true, nil
	}
	return Security{}, false, nil
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
