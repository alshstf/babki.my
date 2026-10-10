package table

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// Source is what the journal calls the rows a table wrote.
const Source = operation.SourceTable

// catalog finds the paper a cell names.
type catalog interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
	ByTickerTradable(ctx context.Context, ticker string) (instrument.Instrument, error)
}

// Reason is why a row was not imported, as a code the screen words in the
// person's language.
type Reason string

const (
	ReasonNoType        Reason = "no_type"
	ReasonTypeNotMapped Reason = "type_not_mapped"
	ReasonNoDate        Reason = "no_date"
	ReasonBadDate       Reason = "bad_date"
	ReasonNoPaper       Reason = "no_paper"
	ReasonPaperNotFound Reason = "paper_not_found"
	ReasonBadCurrency   Reason = "bad_currency"
	ReasonNoNumber      Reason = "no_number"
	ReasonBadNumber     Reason = "bad_number"
	ReasonTooPrecise    Reason = "too_precise"
	ReasonTooLarge      Reason = "too_large"
	// ReasonEngineRefused is the journal's refusal; Value carries its words.
	ReasonEngineRefused Reason = "engine_refused"
)

// Unreadable is a row this package cannot turn into an operation: why, which
// field it stopped on (empty when none), and the cell's own text.
type Unreadable struct {
	Code  Reason
	Field Field
	Value string
}

func (u *Unreadable) Error() string {
	return fmt.Sprintf("%s %s %q", u.Code, u.Field, u.Value)
}

func unreadable(code Reason, field Field, value string) *Unreadable {
	return &Unreadable{Code: code, Field: field, Value: value}
}

var isinPattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{9}[0-9]$`)

// reader turns rows into operations for one account.
type reader struct {
	accountID uuid.UUID
	currency  string
	mapping   Mapping
	papers    catalog
	known     map[string]*instrument.Instrument // cell → paper, nil when not found
	// categories are the family's, for a category column; filing by rules
	// happens after reading (Service.file).
	categories []category.Category
}

func (r *reader) cell(line Line, f Field) string {
	col, ok := r.mapping.Columns[f]
	if !ok || col < 0 || col >= len(line.Cells) {
		return ""
	}
	return line.Cells[col]
}

// read turns one row into an operation, or says why it cannot.
func (r *reader) read(ctx context.Context, line Line) (operation.Operation, error) {
	op := operation.Operation{AccountID: r.accountID, Source: Source}

	typ, err := r.rowType(line)
	if err != nil {
		return op, err
	}
	op.Type = typ

	day, err := parseDay(r.cell(line, FieldDate))
	if err != nil {
		return op, err
	}
	op.OccurredOn = day

	paper, err := r.instrument(ctx, r.cell(line, FieldInstrument))
	if err != nil {
		return op, err
	}
	if paper != nil {
		op.InstrumentID = &paper.ID
	} else if typ == operation.TypeBuy || typ == operation.TypeSell || typ == operation.TypeAmortization {
		return op, unreadable(ReasonNoPaper, FieldInstrument, "")
	}

	op.Currency = r.currency
	if paper != nil {
		op.Currency = paper.Currency
	}
	if c := r.cell(line, FieldCurrency); c != "" {
		code, ok := currencyCode(c)
		if !ok {
			return op, unreadable(ReasonBadCurrency, FieldCurrency, c)
		}
		op.Currency = code
	}

	if typ == operation.TypeBuy || typ == operation.TypeSell {
		qty, _, err := parseNumber(r.cell(line, FieldQuantity), FieldQuantity)
		if err != nil {
			return op, err
		}
		price, _, err := parseNumber(r.cell(line, FieldPrice), FieldPrice)
		if err != nil {
			return op, err
		}
		qty, price = qty.Abs(), price.Abs()
		op.Quantity, op.Price = &qty, &price
	}

	amount, err := r.amount(line, typ, op)
	if err != nil {
		return op, err
	}
	op.AmountMinor = amount

	if c := r.cell(line, FieldFee); c != "" {
		fee, _, err := parseNumber(c, FieldFee)
		if err != nil {
			return op, err
		}
		if op.FeeMinor, err = minor(fee.Abs(), FieldFee, c); err != nil {
			return op, err
		}
	}
	op.Note = r.cell(line, FieldNote)
	op.Counterparty = clip(strings.TrimSpace(r.cell(line, FieldCounterparty)), operation.MaxCounterpartyRunes)
	if c := strings.TrimSpace(r.cell(line, FieldCategory)); c != "" && operation.Categorizable(op) {
		op.CategoryID = categoryNamed(r.categories, c, operation.CategoryKindOf(op.Type))
		// A bank's own category the family has no match for («Супермаркеты»)
		// stays with the row, in its note, where a rule can read it.
		if op.CategoryID == nil {
			if op.Note == "" {
				op.Note = c
			} else {
				op.Note += " · " + c
			}
		}
	}
	return op, nil
}

// rowType is the row's operation: its type cell as mapped, or — in a table
// with no type column, as a bank's statement has none — the sign of its
// amount: money out is a withdrawal, money in a deposit.
func (r *reader) rowType(line Line) (operation.Type, error) {
	if _, mapped := r.mapping.Columns[FieldType]; !mapped {
		cell := r.cell(line, FieldAmount)
		v, _, err := parseNumber(cell, FieldAmount)
		if err != nil {
			return "", err
		}
		if v.IsZero() {
			return "", unreadable(ReasonNoType, FieldAmount, cell)
		}
		if v.IsNegative() {
			return operation.TypeWithdrawal, nil
		}
		return operation.TypeDeposit, nil
	}
	typeCell := r.cell(line, FieldType)
	typ, ok := r.mapping.Types[typeKey(typeCell)]
	if !ok {
		if typeCell == "" {
			return "", unreadable(ReasonNoType, FieldType, "")
		}
		return "", unreadable(ReasonTypeNotMapped, FieldType, typeCell)
	}
	return typ, nil
}

// clip cuts s to at most n code points.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// CategorySeparator stands between a parent's name and a child's in a table's
// category cell, «Транспорт / Такси».
const CategorySeparator = " / "

// foldName is a category's name as a cell is compared with it: case aside,
// «ё» read as «е».
func foldName(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "ё", "е")
}

// categoryNamed is the family's active category of kind a cell names — by its
// own name, or as «Родитель / Дочерняя» the way the export writes it. Nil when
// none does, or when the name alone fits more than one: the row then waits
// unfiled (or for a rule) rather than land in a guess.
func categoryNamed(categories []category.Category, cell string, kind category.Kind) *uuid.UUID {
	parentName, name := "", foldName(cell)
	if i := strings.LastIndex(cell, CategorySeparator); i >= 0 {
		parentName, name = foldName(cell[:i]), foldName(cell[i+len(CategorySeparator):])
	}
	byID := make(map[uuid.UUID]category.Category, len(categories))
	for _, c := range categories {
		byID[c.ID] = c
	}
	var found *uuid.UUID
	for _, c := range categories {
		if c.Kind != kind || c.Archived || foldName(c.Name) != name {
			continue
		}
		if parentName != "" {
			if c.ParentID == nil || foldName(byID[*c.ParentID].Name) != parentName {
				continue
			}
		}
		if found != nil {
			return nil
		}
		id := c.ID
		found = &id
	}
	return found
}

// amount is the row's money, signed the way the journal records the type:
// out for a purchase, a withdrawal, a fee or a tax, in for the rest. A trade
// with no amount in the table is worked out from its quantity and price, the
// way a hand entry is (operation.TradeAmountMinor).
func (r *reader) amount(line Line, typ operation.Type, op operation.Operation) (int64, error) {
	cell := r.cell(line, FieldAmount)
	if cell == "" {
		if op.Quantity != nil && op.Price != nil {
			v, err := operation.TradeAmountMinor(typ, *op.Quantity, *op.Price)
			if err != nil {
				return 0, unreadable(ReasonTooLarge, FieldAmount, op.Quantity.String()+" × "+op.Price.String())
			}
			return v, nil
		}
		return 0, unreadable(ReasonNoNumber, FieldAmount, "")
	}
	v, _, err := parseNumber(cell, FieldAmount)
	if err != nil {
		return 0, err
	}
	m, err := minor(v.Abs(), FieldAmount, cell)
	if err != nil {
		return 0, err
	}
	switch typ {
	case operation.TypeBuy, operation.TypeWithdrawal, operation.TypeFee, operation.TypeTax:
		return -m, nil
	default:
		return m, nil
	}
}

// instrument finds the paper a cell names: by ISIN when the cell is one, by
// ticker otherwise. Nil, nil for an empty cell. Each cell is looked up once.
func (r *reader) instrument(ctx context.Context, cell string) (*instrument.Instrument, error) {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return nil, nil
	}
	if paper, seen := r.known[cell]; seen {
		if paper == nil {
			return nil, unreadable(ReasonPaperNotFound, FieldInstrument, cell)
		}
		return paper, nil
	}
	var (
		found instrument.Instrument
		err   error
	)
	if code := strings.ToUpper(cell); isinPattern.MatchString(code) {
		found, err = r.papers.ByISIN(ctx, code)
	} else {
		found, err = r.papers.ByTickerTradable(ctx, strings.ToUpper(cell))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		r.known[cell] = nil
		return nil, unreadable(ReasonPaperNotFound, FieldInstrument, cell)
	}
	if err != nil {
		return nil, err
	}
	r.known[cell] = &found
	return &found, nil
}

var dayLayouts = []string{"02.01.2006", "2006-01-02", "02/01/2006", "2.1.2006", "02.01.06"}

// parseDay reads the date at the start of a cell, ignoring a time after it.
func parseDay(cell string) (time.Time, error) {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return time.Time{}, unreadable(ReasonNoDate, FieldDate, "")
	}
	original := cell
	if i := strings.IndexAny(cell, " T"); i > 0 {
		cell = cell[:i]
	}
	for _, layout := range dayLayouts {
		if d, err := time.Parse(layout, cell); err == nil {
			return d, nil
		}
	}
	return time.Time{}, unreadable(ReasonBadDate, FieldDate, original)
}

// parseNumber reads a number as exports write it: blanks and apostrophes
// between thousands, a decimal comma or point, a minus of any dash or a
// bracketed amount. When a cell has both a comma and a point, the last of them
// is the decimal mark.
func parseNumber(cell string, what Field) (decimal.Decimal, bool, error) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', ' ', ' ', '\'', '’':
			return -1
		case '−', '–', '—':
			return '-'
		}
		return r
	}, cell)
	negative := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s, negative = s[1:len(s)-1], true
	}
	if s == "" {
		return decimal.Zero, false, unreadable(ReasonNoNumber, what, "")
	}
	comma, point := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	switch {
	case comma >= 0 && point >= 0 && comma > point:
		s = strings.ReplaceAll(s, ".", "")
		s = strings.Replace(s, ",", ".", 1)
	case comma >= 0 && point >= 0:
		s = strings.ReplaceAll(s, ",", "")
	case comma >= 0:
		if strings.Count(s, ",") > 1 {
			return decimal.Zero, false, unreadable(ReasonBadNumber, what, cell)
		}
		s = strings.Replace(s, ",", ".", 1)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, false, unreadable(ReasonBadNumber, what, cell)
	}
	if negative {
		d = d.Neg()
	}
	return d, d.IsNegative(), nil
}

// minor is an amount in minor units. More than two decimal places is refused
// rather than rounded: a figure the file states is not changed in silence.
func minor(d decimal.Decimal, what Field, cell string) (int64, error) {
	shifted := d.Shift(2)
	if !shifted.Equal(shifted.Truncate(0)) {
		return 0, unreadable(ReasonTooPrecise, what, cell)
	}
	v, err := money.Minor(shifted)
	if err != nil {
		return 0, unreadable(ReasonTooLarge, what, cell)
	}
	return v, nil
}

var currencyWords = map[string]string{
	"RUB": "RUB", "RUR": "RUB", "РУБ": "RUB", "РУБ.": "RUB", "₽": "RUB",
	"USD": "USD", "$": "USD", "EUR": "EUR", "€": "EUR", "CNY": "CNY", "¥": "CNY",
	"KZT": "KZT", "₸": "KZT", "GBP": "GBP", "£": "GBP", "CHF": "CHF", "HKD": "HKD",
}

// currencyCode reads a currency cell: an ISO code, or the sign or Russian
// abbreviation exports write instead.
func currencyCode(cell string) (string, bool) {
	key := strings.ToUpper(strings.TrimSpace(cell))
	if code, ok := currencyWords[key]; ok {
		return code, true
	}
	if len(key) == 3 && strings.IndexFunc(key, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0 {
		return key, true
	}
	return "", false
}

// fingerprint is what a row is recognized by when the same file, or one
// covering an overlapping period, is loaded again: its content as read, not
// its place in the file. The nth row with the same content gets n, so two
// identical trades on one day stay two.
func fingerprint(op operation.Operation, seen map[string]int) string {
	parts := []string{
		op.OccurredOn.Format("2006-01-02"), string(op.Type), op.Currency,
		fmt.Sprint(op.AmountMinor), fmt.Sprint(op.FeeMinor),
	}
	if op.InstrumentID != nil {
		parts = append(parts, op.InstrumentID.String())
	}
	for _, d := range []*decimal.Decimal{op.Quantity, op.Price} {
		if d != nil {
			parts = append(parts, d.String())
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	key := hex.EncodeToString(sum[:12])
	seen[key]++
	return fmt.Sprintf("%s:%s:%d", Source, key, seen[key])
}

// typeAllowed reports whether a table may hold op's type.
func typeAllowed(t operation.Type) bool { return slices.Contains(Types, t) }
