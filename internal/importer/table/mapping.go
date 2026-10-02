package table

import (
	"slices"
	"strings"

	"babki.my/babki/internal/operation"
)

// Field is what a column of the table means.
type Field string

const (
	FieldDate       Field = "date"
	FieldType       Field = "type"
	FieldInstrument Field = "instrument"
	FieldQuantity   Field = "quantity"
	FieldPrice      Field = "price"
	FieldAmount     Field = "amount"
	FieldCurrency   Field = "currency"
	FieldFee        Field = "fee"
	FieldNote       Field = "note"
)

// Fields lists every field, in the order a person reads a trade.
var Fields = []Field{
	FieldDate, FieldType, FieldInstrument, FieldQuantity, FieldPrice,
	FieldAmount, FieldCurrency, FieldFee, FieldNote,
}

// Types are the operations a table may hold: what a person records by hand,
// one row each. Transfers, conversions and splits travel in pairs or come from
// the registry, and a table row cannot say which pair it belongs to.
var Types = []operation.Type{
	operation.TypeBuy, operation.TypeSell, operation.TypeDeposit, operation.TypeWithdrawal,
	operation.TypeDividend, operation.TypeCoupon, operation.TypeInterest, operation.TypeTax,
	operation.TypeFee, operation.TypeAmortization,
}

// Mapping says how to read the table: whether its first line is a header,
// which column holds each field, and which value of the type column is which
// operation. A field with no column is absent from Columns.
type Mapping struct {
	HasHeader bool
	Columns   map[Field]int
	Types     map[string]operation.Type
}

// headerWords are the words a header cell is recognized by, in Russian and
// English exports alike. The first field whose word a cell contains wins, so
// the more specific words come first.
var headerWords = []struct {
	field Field
	words []string
}{
	{FieldFee, []string{"комисс", "commission", "fee"}},
	{FieldDate, []string{"дата", "date"}},
	{FieldType, []string{"тип", "операц", "вид", "направлен", "type", "action", "side"}},
	{FieldInstrument, []string{"isin", "тикер", "ticker", "symbol", "бумаг", "инструмент", "security"}},
	{FieldQuantity, []string{"кол", "штук", "quantity", "qty", "shares"}},
	{FieldPrice, []string{"цена", "price"}},
	{FieldAmount, []string{"сумма", "объем", "объём", "amount", "total", "value"}},
	{FieldCurrency, []string{"валют", "currency", "ccy"}},
	{FieldNote, []string{"коммент", "примеч", "описан", "note", "description", "comment"}},
}

// typeWords are the values of a type column recognized without being told.
var typeWords = map[string]operation.Type{
	"покупка": operation.TypeBuy, "купля": operation.TypeBuy, "buy": operation.TypeBuy, "bought": operation.TypeBuy,
	"продажа": operation.TypeSell, "sell": operation.TypeSell, "sold": operation.TypeSell,
	"пополнение": operation.TypeDeposit, "зачисление": operation.TypeDeposit, "ввод": operation.TypeDeposit,
	"ввод денежных средств": operation.TypeDeposit, "deposit": operation.TypeDeposit,
	"вывод": operation.TypeWithdrawal, "вывод денежных средств": operation.TypeWithdrawal,
	"снятие": operation.TypeWithdrawal, "withdrawal": operation.TypeWithdrawal,
	"дивиденд": operation.TypeDividend, "дивиденды": operation.TypeDividend, "dividend": operation.TypeDividend,
	"купон": operation.TypeCoupon, "купонный доход": operation.TypeCoupon, "coupon": operation.TypeCoupon,
	"проценты": operation.TypeInterest, "процент": operation.TypeInterest, "interest": operation.TypeInterest,
	"налог": operation.TypeTax, "ндфл": operation.TypeTax, "tax": operation.TypeTax,
	"комиссия": operation.TypeFee, "fee": operation.TypeFee, "commission": operation.TypeFee,
	"амортизация": operation.TypeAmortization, "частичное погашение": operation.TypeAmortization,
	"amortization": operation.TypeAmortization,
}

// typeKey is how a type cell is compared: case and surrounding blanks do not
// matter.
func typeKey(cell string) string { return strings.ToLower(strings.TrimSpace(cell)) }

// Guess proposes a mapping from the table's first line and the values its
// type column holds. A first line counts as a header when it names at least
// two fields. Anything it cannot place is left out for the person to set.
func Guess(t Table) Mapping {
	m := Mapping{Columns: map[Field]int{}, Types: map[string]operation.Type{}}
	if len(t.Rows) == 0 {
		return m
	}
	isin := -1
	for i, cell := range t.Rows[0].Cells {
		h := strings.ToLower(cell)
		if strings.Contains(h, "isin") {
			isin = i
		}
		for _, hw := range headerWords {
			if _, taken := m.Columns[hw.field]; taken {
				continue
			}
			if slices.ContainsFunc(hw.words, func(w string) bool { return strings.Contains(h, w) }) {
				m.Columns[hw.field] = i
				break
			}
		}
	}
	// An ISIN names a paper more surely than a ticker does.
	if isin >= 0 {
		m.Columns[FieldInstrument] = isin
	}
	m.HasHeader = len(m.Columns) >= 2
	if !m.HasHeader {
		m.Columns = map[Field]int{}
	}
	if col, ok := m.Columns[FieldType]; ok {
		for _, line := range dataRows(t, m) {
			if col >= len(line.Cells) {
				continue
			}
			key := typeKey(line.Cells[col])
			if typ, known := typeWords[key]; known {
				m.Types[key] = typ
			}
		}
	}
	return m
}

// dataRows is the table without its header.
func dataRows(t Table, m Mapping) []Line {
	if m.HasHeader && len(t.Rows) > 0 {
		return t.Rows[1:]
	}
	return t.Rows
}
