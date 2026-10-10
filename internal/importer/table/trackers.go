package table

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// Tracker is another program's export this package knows (#407): its rows
// are rearranged into the columns of this package's own table, so the same
// guessing, preview and import take them, each row keeping its line.
type Tracker string

const (
	TrackerIntelinvest Tracker = "intelinvest"
	TrackerSnowball    Tracker = "snowball"
	TrackerGhostfolio  Tracker = "ghostfolio"
)

// The columns an export is rearranged into, headed with words Guess knows.
var trackerHeader = []string{"Дата", "Тип", "Бумага", "Количество", "Цена", "Сумма", "Валюта", "Комиссия", "Примечание"}

const (
	colDate = iota
	colType
	colPaper
	colQuantity
	colPrice
	colAmount
	colCurrency
	colFee
	colNote
)

// recognize rearranges a tracker's export; any other table is left as it is.
func recognize(t Table) Table {
	if len(t.Rows) == 0 {
		return t
	}
	header := newHeaderIndex(t.Rows[0].Cells)
	switch {
	case isIntelinvest(t):
		return fromIntelinvest(t)
	case header.has("event", "date", "symbol", "quantity", "feetax"):
		return fromSnowball(t, header)
	case header.has("date", "type", "unitprice"):
		return fromGhostfolio(t, header)
	}
	return t
}

// headerIndex finds a header's cells by name, case and blanks aside.
type headerIndex map[string]int

func newHeaderIndex(cells []string) headerIndex {
	out := headerIndex{}
	for i, c := range cells {
		k := strings.ToLower(strings.Join(strings.Fields(c), ""))
		if _, dup := out[k]; !dup {
			out[k] = i
		}
	}
	return out
}

func (h headerIndex) has(names ...string) bool {
	for _, n := range names {
		if _, ok := h[n]; !ok {
			return false
		}
	}
	return true
}

// get is the line's cell under the first of the names the header has.
func (h headerIndex) get(line Line, names ...string) string {
	for _, n := range names {
		if i, ok := h[n]; ok {
			return cellAt(line.Cells, i)
		}
	}
	return ""
}

func cellAt(cells []string, i int) string {
	if i < 0 || i >= len(cells) {
		return ""
	}
	return cells[i]
}

// rearranged starts a tracker's table: its header, its own operation words.
func rearranged(tracker Tracker, words map[string]operation.Type, number int, header []string) Table {
	return Table{Tracker: tracker, words: words, Rows: []Line{{Number: number, Cells: header}}}
}

func number(cell string) (decimal.Decimal, bool) {
	if strings.TrimSpace(cell) == "" {
		return decimal.Zero, false
	}
	d, _, err := parseNumber(cell, FieldAmount)
	return d, err == nil
}

// times is quantity × price as money, to the kopeck: a sum the export does not
// state but implies. A price left blank is one; a cell that is not a number
// is passed on for the reader to name.
func times(quantity, price string) string {
	q, ok := number(quantity)
	if !ok {
		return quantity
	}
	if strings.TrimSpace(price) == "" {
		return quantity
	}
	p, ok := number(price)
	if !ok {
		return price
	}
	return q.Mul(p).Abs().Round(2).String()
}

func nonZero(cell string) bool {
	d, ok := number(cell)
	return ok && !d.IsZero()
}

// Snowball Income's own table (help.snowball-analytics.com/import-custom):
// Event, Date (YYYY-MM-DD), Symbol, Price, Quantity, Currency, FeeTax, … .
// The money is in Quantity for deposits, withdrawals and dividends (the
// dividends' total; Price is per share), FeeTax is a trade's commission, the
// tax withheld from a dividend, or the whole of a Fee.
var snowballWords = map[string]operation.Type{
	"cash_in": operation.TypeDeposit, "cash_out": operation.TypeWithdrawal,
	"cash_gain": operation.TypeInterest, "cash_expense": operation.TypeFee,
}

func fromSnowball(t Table, h headerIndex) Table {
	out := rearranged(TrackerSnowball, snowballWords, t.Rows[0].Number, trackerHeader)
	for _, line := range t.Rows[1:] {
		event := h.get(line, "event")
		row := make([]string, len(trackerHeader))
		row[colDate], row[colType], row[colNote] = h.get(line, "date"), event, h.get(line, "note")
		row[colCurrency] = h.get(line, "currency")
		switch strings.ToLower(event) {
		case "dividend":
			row[colPaper], row[colAmount] = h.get(line, "symbol"), h.get(line, "quantity")
			out.Rows = append(out.Rows, Line{Number: line.Number, Cells: row})
			// The tax withheld is the journal's own operation, on the same line.
			if tax := h.get(line, "feetax"); nonZero(tax) {
				withheld := slices.Clone(row)
				withheld[colType], withheld[colAmount] = "Tax", tax
				out.Rows = append(out.Rows, Line{Number: line.Number, Cells: withheld})
			}
			continue
		case "cash_in", "cash_out", "cash_gain", "cash_expense":
			row[colAmount] = times(h.get(line, "quantity"), h.get(line, "price"))
			if row[colCurrency] == "" {
				row[colCurrency] = h.get(line, "symbol")
			}
		case "fee":
			row[colAmount] = h.get(line, "feetax")
		default:
			row[colPaper], row[colQuantity], row[colPrice] = h.get(line, "symbol"), h.get(line, "quantity"), h.get(line, "price")
			row[colFee] = h.get(line, "feetax")
		}
		out.Rows = append(out.Rows, Line{Number: line.Number, Cells: row})
	}
	return out
}

// Ghostfolio's activities, from its CSV import table or its JSON export:
// date, symbol, type (BUY, SELL, DIVIDEND, FEE, INTEREST, …), quantity,
// unitPrice, fee, currency, comment. A dividend or interest is quantity ×
// unitPrice; a fee is in fee. A paper of the data source MANUAL is named by
// Ghostfolio's own id, which names nothing here.
func fromGhostfolio(t Table, h headerIndex) Table {
	out := rearranged(TrackerGhostfolio, nil, t.Rows[0].Number, append(slices.Clone(trackerHeader), "Счёт"))
	for _, line := range t.Rows[1:] {
		typ := h.get(line, "type", "action", "buy/sell")
		qty := h.get(line, "quantity", "qty", "shares", "units")
		price := h.get(line, "unitprice", "price", "tradeprice")
		fee := h.get(line, "fee", "commission")
		paper := h.get(line, "symbol", "ticker", "code")
		if strings.EqualFold(h.get(line, "datasource"), "MANUAL") {
			paper = ""
		}
		row := make([]string, len(trackerHeader)+1)
		row[colDate], row[colType] = h.get(line, "date", "tradedate"), typ
		row[colCurrency], row[colNote] = h.get(line, "currency", "ccy"), h.get(line, "comment", "note")
		row[len(trackerHeader)] = h.get(line, "account")
		switch strings.ToUpper(typ) {
		case "DIVIDEND":
			row[colPaper], row[colAmount], row[colFee] = paper, times(qty, price), fee
		case "INTEREST":
			row[colAmount], row[colFee] = times(qty, price), fee
		case "FEE":
			row[colAmount] = fee
			if !nonZero(fee) {
				row[colAmount] = times(qty, price)
			}
		default:
			row[colPaper], row[colQuantity], row[colPrice], row[colFee] = paper, qty, price, fee
		}
		out.Rows = append(out.Rows, Line{Number: line.Number, Cells: row})
	}
	return out
}

// readGhostfolioJSON reads Ghostfolio's JSON export into its activities'
// table, numbered from 1 in the order the file lists them.
func readGhostfolioJSON(content string) (Table, error) {
	var export struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
		Activities []map[string]any `json:"activities"`
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(content, "\uFEFF")))
	dec.UseNumber()
	if err := dec.Decode(&export); err != nil || export.Activities == nil {
		return Table{}, fmt.Errorf("%w: a JSON file is read only as Ghostfolio's export", ErrBadTable)
	}
	if len(export.Activities) > MaxRows {
		return Table{}, fmt.Errorf("%w: more than %d rows; split the file", ErrBadTable, MaxRows)
	}
	accounts := map[string]string{}
	for _, a := range export.Accounts {
		accounts[a.ID] = a.Name
	}
	fields := []string{"date", "symbol", "type", "quantity", "unitPrice", "fee", "currency", "comment", "dataSource", "account"}
	t := Table{Rows: []Line{{Number: 0, Cells: fields}}}
	for i, a := range export.Activities {
		cells := make([]string, len(fields))
		for j, f := range fields {
			switch v := a[f].(type) {
			case string:
				cells[j] = strings.TrimSpace(v)
			case json.Number:
				cells[j] = v.String()
			}
		}
		if id, ok := a["accountId"].(string); ok {
			cells[len(fields)-1] = accounts[id]
		}
		t.Rows = append(t.Rows, Line{Number: i + 1, Cells: cells})
	}
	return t, nil
}

// Intelinvest's CSV, as it exports and imports it: no header, ';' between
// cells, «#…» lines of its own (a format version, the person's own papers),
// and TYPE;DATE;TICKER;QUANTITY;PRICE;FEE;NKD;NOMINAL;CURRENCY;FEE_CURRENCY;
// NOTE;LINK_ID. A bond's price is in percent of NOMINAL and its NKD is the
// accrued coupon paid with it; money moved by an operation is a row of its
// own (MONEYDEPOSIT, MONEYWITHDRAW) with the operation's LINK_ID and the
// money in PRICE. Read from the converters github.com/winzard/snowball and
// github.com/sosiska/t2122intel; Intelinvest publishes no description.
const (
	inType = iota
	inDate
	inTicker
	inQuantity
	inPrice
	inFee
	inNKD
	inNominal
	inCurrency
	_ // FEE_CURRENCY
	inNote
	inLink
)

var intelinvestWords = map[string]operation.Type{
	"stockbuy": operation.TypeBuy, "bondbuy": operation.TypeBuy, "share_buy": operation.TypeBuy,
	"stocksell": operation.TypeSell, "bondsell": operation.TypeSell, "share_sell": operation.TypeSell,
	"moneydeposit": operation.TypeDeposit, "moneywithdraw": operation.TypeWithdrawal,
	"income": operation.TypeInterest, "loss": operation.TypeFee,
}

// intelinvestKind says whether a cell is one of Intelinvest's operations,
// those a table cannot hold included.
func intelinvestKind(cell string) bool {
	k := strings.ToLower(cell)
	_, known := intelinvestWords[k]
	return known || k == "dividend" || k == "coupon" || k == "amortization" ||
		k == "currency_buy" || k == "currency_sell"
}

func isIntelinvest(t Table) bool {
	first := t.Rows[0].Cells
	return strings.HasPrefix(cellAt(first, 0), "#CsvFormatVersion") ||
		(len(first) > inLink && intelinvestKind(cellAt(first, inType)) && cellAt(first, inType) == strings.ToUpper(cellAt(first, inType)))
}

func moneyRow(cells []string) bool {
	k := strings.ToLower(cellAt(cells, inType))
	return k == "moneydeposit" || k == "moneywithdraw"
}

func fromIntelinvest(t Table) Table {
	var rows []Line
	ownPapers := false
	for _, line := range t.Rows {
		first := cellAt(line.Cells, 0)
		switch {
		case strings.HasPrefix(first, "#AssetsDefinitionsStart"):
			ownPapers = true
		case strings.HasPrefix(first, "#AssetsDefinitionsEnd"):
			ownPapers = false
		case !ownPapers && !strings.HasPrefix(first, "#"):
			rows = append(rows, line)
		}
	}
	// The operation a link belongs to, and the money it moved.
	owner, moved := map[string]int{}, map[string]string{}
	for _, line := range rows {
		if link := cellAt(line.Cells, inLink); link != "" && !moneyRow(line.Cells) {
			if _, seen := owner[link]; !seen {
				owner[link] = line.Number
			}
		}
	}
	for _, line := range rows {
		if link := cellAt(line.Cells, inLink); link != "" && moneyRow(line.Cells) {
			moved[link] = cellAt(line.Cells, inPrice)
		}
	}

	out := rearranged(TrackerIntelinvest, intelinvestWords, 0, trackerHeader)
	for _, line := range rows {
		c := line.Cells
		row := make([]string, len(trackerHeader))
		row[colDate], row[colType] = cellAt(c, inDate), cellAt(c, inType)
		row[colCurrency], row[colNote] = cellAt(c, inCurrency), cellAt(c, inNote)
		link := cellAt(c, inLink)
		paired := 0
		switch kind := strings.ToLower(cellAt(c, inType)); {
		case moneyRow(c):
			row[colAmount] = cellAt(c, inPrice)
			if link != "" {
				paired = owner[link]
			}
		case kind == "bondbuy" || kind == "bondsell":
			row[colPaper], row[colQuantity], row[colFee] = paperCode(cellAt(c, inTicker)), cellAt(c, inQuantity), cellAt(c, inFee)
			row[colPrice], row[colAmount] = bondTrade(cellAt(c, inQuantity), cellAt(c, inPrice), cellAt(c, inNominal), cellAt(c, inNKD))
		case kind == "dividend" || kind == "coupon" || kind == "amortization":
			row[colPaper] = paperCode(cellAt(c, inTicker))
			row[colAmount] = times(cellAt(c, inQuantity), cellAt(c, inPrice))
			// What reached the account, when the money row says.
			if m, ok := moved[link]; ok && link != "" {
				row[colAmount] = m
			}
		case kind == "income" || kind == "loss":
			row[colAmount] = times(cellAt(c, inQuantity), cellAt(c, inPrice))
			if cellAt(c, inQuantity) == "" {
				row[colAmount] = cellAt(c, inPrice)
			}
		default:
			row[colPaper], row[colQuantity], row[colPrice] = paperCode(cellAt(c, inTicker)), cellAt(c, inQuantity), cellAt(c, inPrice)
			row[colFee] = cellAt(c, inFee)
		}
		out.Rows = append(out.Rows, Line{Number: line.Number, Cells: row, PairedWith: paired})
	}
	return out
}

// paperCode is the paper a ticker cell names: Intelinvest writes some as
// TICKER:ISIN, and an ISIN names a paper more surely.
func paperCode(cell string) string {
	parts := strings.Split(cell, ":")
	for _, p := range parts {
		if isinPattern.MatchString(strings.TrimSpace(p)) {
			return strings.TrimSpace(p)
		}
	}
	return strings.TrimSpace(parts[0])
}

// bondTrade is a bond trade's price per bond — percent of the nominal — and
// what it cost with the accrued coupon. Without a nominal the percent cannot
// be money: the price is passed on marked so the row says why it stopped.
func bondTrade(quantity, percent, nominal, nkd string) (price, amount string) {
	p, okP := number(percent)
	n, okN := number(nominal)
	if !okP || !okN {
		return percent + "%", ""
	}
	perBond := p.Mul(n).Div(hundred)
	accrued, ok := number(nkd)
	if !ok {
		return perBond.String(), ""
	}
	q, ok := number(quantity)
	if !ok {
		return perBond.String(), ""
	}
	return perBond.String(), q.Abs().Mul(perBond).Add(accrued.Abs()).Round(2).String()
}

var hundred = decimal.NewFromInt(100)
