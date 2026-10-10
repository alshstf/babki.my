package table

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

// The template a person fills in by hand (#407): the header in the words
// Guess knows and a row of each kind a table takes, that read back as they
// are. Amounts are written without a sign; the type says which way the money
// goes.
var templateHeader = []string{"Дата", "Тип", "Бумага", "Количество", "Цена", "Сумма", "Валюта", "Комиссия", "Примечание"}

type templateRow struct {
	on                           time.Time
	typ, paper                   string
	quantity, price, amount, fee string
	currency, note               string
}

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

var templateRows = []templateRow{
	{on: day("2026-01-12"), typ: "Пополнение", amount: "100000", currency: "RUB", note: "С карты"},
	{on: day("2026-01-13"), typ: "Покупка", paper: "SBER", quantity: "100", price: "305.5", fee: "15.28", currency: "RUB"},
	{on: day("2026-01-20"), typ: "Продажа", paper: "SBER", quantity: "40", price: "312", fee: "6.24", currency: "RUB"},
	{on: day("2026-02-16"), typ: "Дивиденд", paper: "SBER", amount: "2049", currency: "RUB"},
	{on: day("2026-02-16"), typ: "Налог", paper: "SBER", amount: "266.37", currency: "RUB", note: "НДФЛ с дивидендов"},
	{on: day("2026-02-28"), typ: "Проценты", amount: "412.6", currency: "RUB", note: "На остаток"},
	{on: day("2026-03-01"), typ: "Комиссия", amount: "199", currency: "RUB", note: "Обслуживание счёта"},
	{on: day("2026-03-10"), typ: "Вывод", amount: "20000", currency: "RUB"},
}

// templateHelp explains the columns on the workbook's second sheet.
var templateHelp = [][]string{
	{"Колонка", "Что писать"},
	{"Дата", "День операции: 15.01.2026 или 2026-01-15"},
	{"Тип", "Покупка, Продажа, Пополнение, Вывод, Дивиденд, Купон, Проценты, Налог, Комиссия, Амортизация"},
	{"Бумага", "Тикер (SBER) или ISIN (RU0009029540); для движений денег — пусто"},
	{"Количество", "Штук — для покупки и продажи"},
	{"Цена", "За одну бумагу — для покупки и продажи"},
	{"Сумма", "Без знака; для покупки и продажи можно не писать — посчитается из количества и цены"},
	{"Валюта", "RUB, USD, CNY…; пусто — валюта счёта"},
	{"Комиссия", "Комиссия брокера по сделке, без знака"},
	{"Примечание", "Что угодно — останется в журнале"},
}

// cells is a row as text, with the decimal comma a Russian Excel
// reads numbers by.
func (r templateRow) cells() []string {
	comma := func(s string) string { return strings.ReplaceAll(s, ".", ",") }
	return []string{
		r.on.Format("02.01.2006"), r.typ, r.paper, comma(r.quantity), comma(r.price),
		comma(r.amount), r.currency, comma(r.fee), r.note,
	}
}

// TemplateCSV is the template as a CSV: a byte-order mark for Excel to read
// it as UTF-8, ';' between cells.
func TemplateCSV() []byte {
	var b bytes.Buffer
	b.WriteString("\uFEFF")
	w := csv.NewWriter(&b)
	w.Comma = ';'
	_ = w.Write(templateHeader)
	for _, r := range templateRows {
		_ = w.Write(r.cells())
	}
	w.Flush()
	return b.Bytes()
}

// TemplateXLSX is the template as an Excel workbook: dates and numbers as
// Excel's own, the help on a second sheet.
func TemplateXLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	const sheet, help = "Операции", "Как заполнять"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return nil, err
	}
	dateStyle, err := f.NewStyle(&excelize.Style{NumFmt: 14, CustomNumFmt: ptr("dd.mm.yyyy")})
	if err != nil {
		return nil, err
	}
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}
	put := func(sheet string, col, row int, v any) error {
		ref, err := excelize.CoordinatesToCellName(col, row)
		if err != nil {
			return err
		}
		return f.SetCellValue(sheet, ref, v)
	}
	for i, h := range templateHeader {
		if err := put(sheet, i+1, 1, h); err != nil {
			return nil, err
		}
	}
	for i, r := range templateRows {
		row := i + 2
		values := []any{r.on, r.typ, r.paper, num(r.quantity), num(r.price), num(r.amount), r.currency, num(r.fee), r.note}
		for j, v := range values {
			if v == nil || v == "" {
				continue
			}
			if err := put(sheet, j+1, row, v); err != nil {
				return nil, err
			}
		}
		ref, _ := excelize.CoordinatesToCellName(1, row)
		if err := f.SetCellStyle(sheet, ref, ref, dateStyle); err != nil {
			return nil, err
		}
	}
	if err := f.SetCellStyle(sheet, "A1", "I1", bold); err != nil {
		return nil, err
	}
	if err := f.SetColWidth(sheet, "A", "I", 13); err != nil {
		return nil, err
	}
	if err := f.SetColWidth(sheet, "I", "I", 24); err != nil {
		return nil, err
	}
	if _, err := f.NewSheet(help); err != nil {
		return nil, err
	}
	for i, line := range templateHelp {
		for j, v := range line {
			if err := put(help, j+1, i+1, v); err != nil {
				return nil, err
			}
		}
	}
	if err := f.SetCellStyle(help, "A1", "B1", bold); err != nil {
		return nil, err
	}
	if err := f.SetColWidth(help, "A", "A", 14); err != nil {
		return nil, err
	}
	if err := f.SetColWidth(help, "B", "B", 90); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		return nil, fmt.Errorf("table: write the template: %w", err)
	}
	return b.Bytes(), nil
}

// num is a template figure as Excel's number, or an empty cell.
func num(s string) any {
	if s == "" {
		return nil
	}
	f, _ := decimal.RequireFromString(s).Float64()
	return f
}

func ptr[T any](v T) *T { return &v }
