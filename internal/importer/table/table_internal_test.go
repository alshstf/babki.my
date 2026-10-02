package table

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

func TestATableIsSplitIntoCellsWhateverItsSeparator(t *testing.T) {
	for name, content := range map[string]string{
		"semicolon": "\uFEFFДата;Сумма\n02.10.2026;\"1 234,56\"\n\n03.10.2026;5\n",
		"comma":     "date,amount\n2026-10-02,\"1,234.56\"\n2026-10-03,5\n",
		"tab":       "date\tamount\n2026-10-02\t1234.56\n2026-10-03\t5\n",
	} {
		tbl, err := Parse(content)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(tbl.Rows) != 3 || len(tbl.Rows[1].Cells) != 2 {
			t.Errorf("%s: rows = %+v, want a header and two rows of two cells", name, tbl.Rows)
		}
		if last := tbl.Rows[2]; last.Number != 3 && last.Number != 4 {
			t.Errorf("%s: last row on line %d, want its line in the file", name, last.Number)
		}
	}
	if tbl, _ := Parse("Дата;Сумма\n02.10.2026;1\n\n03.10.2026;2\n"); tbl.Rows[2].Number != 4 {
		t.Errorf("a row after a blank line is on line %d, want 4", tbl.Rows[2].Number)
	}
	if _, err := Parse("  \n"); err == nil {
		t.Error("an empty file was read")
	}
}

func TestNumbersAreReadAsExportsWriteThem(t *testing.T) {
	for cell, want := range map[string]string{
		"1 234,56":   "1234.56",
		"1,234.56":   "1234.56",
		"1.234,56":   "1234.56",
		"−5":         "-5",
		"(12,30)":    "-12.3",
		"7550":       "7550",
		"0,000001":   "0.000001",
		"1\u00a0000": "1000",
	} {
		got, _, err := parseNumber(cell, "amount")
		if err != nil || !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%q = %s, %v; want %s", cell, got, err, want)
		}
	}
	for _, cell := range []string{"", "abc", "1,2,3"} {
		if _, _, err := parseNumber(cell, "amount"); err == nil {
			t.Errorf("%q was read as a number", cell)
		}
	}
	if _, err := minor(decimal.RequireFromString("1.005"), "amount"); err == nil {
		t.Error("an amount with three decimal places was rounded instead of refused")
	}
	if v, err := minor(decimal.RequireFromString("1234.5"), "amount"); err != nil || v != 123450 {
		t.Errorf("1234.5 = %d, %v; want 123450", v, err)
	}
}

func TestDatesAreReadInTheCommonForms(t *testing.T) {
	want := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for _, cell := range []string{"02.10.2026", "2026-10-02", "02/10/2026", "02.10.2026 14:30:00", "2026-10-02T14:30:00Z", "2.10.2026"} {
		if got, err := parseDay(cell); err != nil || !got.Equal(want) {
			t.Errorf("%q = %s, %v; want 2026-10-02", cell, got, err)
		}
	}
	if _, err := parseDay("10/02/2026x"); err == nil {
		t.Error("a malformed date was read")
	}
}

func TestTheMappingIsGuessedFromTheHeader(t *testing.T) {
	tbl, _ := Parse("Дата сделки;Тикер;ISIN;Операция;Кол-во;Цена;Сумма;Валюта;Комиссия;Комментарий\n" +
		"02.10.2026;SBER;RU0009029540;Покупка;10;300;3000;RUB;1,5;\n" +
		"03.10.2026;;;Пополнение;;;5000;RUB;;\n" +
		"04.10.2026;;;Списание комиссии;;;10;RUB;;\n")
	m := Guess(tbl)
	want := map[Field]int{
		FieldDate: 0, FieldInstrument: 2, FieldType: 3, FieldQuantity: 4, FieldPrice: 5,
		FieldAmount: 6, FieldCurrency: 7, FieldFee: 8, FieldNote: 9,
	}
	if !m.HasHeader {
		t.Fatal("the header was not recognized")
	}
	for f, col := range want {
		if m.Columns[f] != col {
			t.Errorf("%s in column %d, want %d", f, m.Columns[f], col)
		}
	}
	if m.Types["покупка"] != operation.TypeBuy || m.Types["пополнение"] != operation.TypeDeposit {
		t.Errorf("types = %v, want покупка and пополнение recognized", m.Types)
	}
	if _, guessed := m.Types["списание комиссии"]; guessed {
		t.Error("a type the program does not know was guessed instead of left to the person")
	}

	if headless := Guess(Table{Rows: []Line{{Number: 1, Cells: []string{"02.10.2026", "5000"}}}}); headless.HasHeader {
		t.Error("a line of figures was taken for a header")
	}
}

func TestIdenticalRowsStayDistinct(t *testing.T) {
	op := operation.Operation{Type: operation.TypeBuy, OccurredOn: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Currency: "RUB", AmountMinor: -300000}
	seen := map[string]int{}
	a, b := fingerprint(op, seen), fingerprint(op, seen)
	if a == b {
		t.Errorf("two identical rows share %s", a)
	}
	again := map[string]int{}
	if fingerprint(op, again) != a {
		t.Error("the same row in a second load of the file is recognized differently")
	}
}
