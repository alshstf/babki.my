package table

import (
	"bytes"
	"encoding/base64"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"babki.my/babki/internal/account"
)

// tbankHeader is Т-Банк's statement header as its web version writes it.
const tbankHeader = `"Имя счёта";"Номер карты";"Дата операции";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Статус";"Категория по-умолчанию";"Ваша категория";"MCC";"Описание";"Сообщение";"Округление";"Сумма операции с округлением";"Бонусы (включая кэшбэк)";"Учёт в аналитике"` + "\r\n"

// A made-up statement of two accounts: a purchase in dollars off the ruble
// card, a gift the person filed under their own category, a payment that did
// not go through, and the savings account's interest.
const tbankStatement = "\uFEFF" + tbankHeader +
	`"Карта";"*0001";"03.10.2026 10:15:00";"-12,50";"USD";"-1100,00";"RUB";"Ок";"Сервис";"";"5734";"Облако";"";"0,00";"-1100,00";"11,00";"Да"` + "\r\n" +
	`"Карта";"";"04.10.2026 09:00:00";"5000,00";"RUB";"5000,00";"RUB";"Ок";"Переводы";"Подарки";"";"Иван И.";"С днём рождения";"0,00";"5000,00";"0,00";"Да"` + "\r\n" +
	`"Карта";"*0001";"05.10.2026 18:00:00";"-300,00";"RUB";"-300,00";"RUB";"Ошибка";"Связь";"";"";"Мой телефон";"";"0,00";"-300,00";"0,00";"Нет"` + "\r\n" +
	`"Копилка";"";"06.10.2026 03:00:00";"4,47";"RUB";"4,47";"RUB";"Ок";"Проценты";"";"";"Проценты на остаток";"";"0,00";"4,47";"0,00";"Да"` + "\r\n"

// previewOn reads a file against a ruble card account with the mapping given,
// or guessed when nil.
func previewOn(t *testing.T, content string, format Format, m *Mapping) Preview {
	t.Helper()
	acc := account.WithBalance{Account: account.Account{ID: uuid.New(), Currency: "RUB", Type: account.TypeChecking}}
	svc := NewService(oneAccount{acc}, catalogOfTests, storedJournal(nil), &takesAll{}, nil, nil, noCategories{})
	p, err := svc.Preview(t.Context(), uuid.New(), acc.ID, content, format, m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Т-Банк's statement holds every account the person chose in one file, so
// nothing loads until they say which of its accounts this journal account is;
// then the others' rows are shown and passed over, as is an operation the bank
// did not carry out. The money is the account's — rubles off a ruble card for
// a purchase in dollars, the dollars kept in the note — and the person's own
// category is over the bank's.
func TestATBankStatementLoadsTheChosenAccountAsItSawTheMoney(t *testing.T) {
	guessed := previewOn(t, tbankStatement, FormatText, nil)
	if guessed.Tracker != TrackerTBank {
		t.Fatalf("tracker = %q, want the statement recognized as Т-Банк's", guessed.Tracker)
	}
	if want := []FileAccount{{"Карта", 3}, {"Копилка", 1}}; !slices.Equal(guessed.FileAccounts, want) {
		t.Errorf("file accounts = %v, want %v", guessed.FileAccounts, want)
	}
	sameRows(t, guessed, []string{
		"2 unparsed account_not_chosen Карта",
		"3 unparsed account_not_chosen Карта",
		"4 unparsed account_not_chosen Карта",
		"5 unparsed account_not_chosen Копилка",
	})

	m := guessed.Mapping
	m.Account = "Карта"
	p := previewOn(t, tbankStatement, FormatText, &m)
	sameRows(t, p, []string{
		"2 new withdrawal -110000",
		"3 new deposit 500000",
		"4 unparsed declined Ошибка",
		"5 unparsed other_account Копилка",
	})
	bought, gift := p.Rows[0].Operation, p.Rows[1].Operation
	if bought.Currency != "RUB" || bought.MCC != "5734" || bought.Counterparty != "Облако" {
		t.Errorf("purchase = %s %s, MCC %q, counterparty %q; want rubles, 5734, «Облако»",
			bought.Currency, bought.OccurredOn.Format("2006-01-02"), bought.MCC, bought.Counterparty)
	}
	if bought.Note != "12,50 USD · Сервис" {
		t.Errorf("purchase note = %q, want the dollars paid and the bank's category", bought.Note)
	}
	if gift.Note != "С днём рождения · Подарки" {
		t.Errorf("gift note = %q, want the message and the person's own category, not the bank's «Переводы»", gift.Note)
	}

	m.Account = "Копилка"
	sameRows(t, previewOn(t, tbankStatement, FormatText, &m), []string{
		"2 unparsed other_account Карта",
		"3 unparsed other_account Карта",
		"4 unparsed other_account Карта",
		"5 new deposit 447",
	})
}

// The same statement saved as Excel holds the date and the sums as numbers.
func TestATBankWorkbookIsReadAsItsCSV(t *testing.T) {
	f := excelize.NewFile()
	const s = "Sheet1"
	tbl, _ := Parse(tbankHeader)
	for i, v := range tbl.Rows[0].Cells {
		ref, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(s, ref, v)
	}
	withTime, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr(`dd"."MM"."yyyy HH:mm:ss`)})
	for col, v := range []any{"Карта", "*0001", 46298.4270833, -1100.0, "RUB", -1100.0, "RUB", "Ок", "Сервис", "", 5734.0, "Облако", "", 0.0, -1100.0, 11.0, "Да"} {
		ref, _ := excelize.CoordinatesToCellName(col+1, 2)
		_ = f.SetCellValue(s, ref, v)
	}
	_ = f.SetCellStyle(s, "C2", "C2", withTime)
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		t.Fatal(err)
	}
	p := previewOn(t, base64.StdEncoding.EncodeToString(b.Bytes()), FormatXLSX, nil)
	sameRows(t, p, []string{"2 new withdrawal -110000"})
	if op := p.Rows[0].Operation; op.OccurredOn.Format("2006-01-02") != "2026-10-03" || op.MCC != "5734" {
		t.Errorf("row = %s MCC %q, want 2026-10-03 and 5734", op.OccurredOn.Format("2006-01-02"), op.MCC)
	}
}

// A header cell whose field an earlier column took stays unmapped: a bank's
// second amount, «Сумма операции с округлением», is not the type column —
// taking it for one left every row of Т-Банк's statement «not mapped».
func TestAColumnWhoseFieldIsTakenIsNotTakenForAVaguerOne(t *testing.T) {
	tbl, _ := Parse("Дата операции;Сумма операции;Сумма операции с округлением;Описание\n" +
		"03.10.2026;-250,00;-250,00;Такси\n")
	m := Guess(tbl)
	if col, mapped := m.Columns[FieldType]; mapped {
		t.Errorf("the type was guessed in column %d, want no type column: the sign decides", col)
	}
	if m.Columns[FieldAmount] != 1 {
		t.Errorf("amount in column %d, want the first sum", m.Columns[FieldAmount])
	}
}
