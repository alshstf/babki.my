package table_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// categories reads the family's categories by «kind/name».
func categories(t *testing.T, url string, c *http.Client) map[string]string {
	t.Helper()
	var list []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	call(t, c, "GET", url+"/api/v1/categories", "", 200, &list)
	out := map[string]string{}
	for _, cat := range list {
		out[cat.Kind+"/"+cat.Name] = cat.ID
	}
	return out
}

func previewOf(t *testing.T, url string, c *http.Client, accountID, file string) preview {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"content": file})
	var got preview
	call(t, c, "POST", url+"/api/v1/accounts/"+accountID+"/imports/preview", string(body), 200, &got)
	return got
}

// A bank's statement has no type column: the sign says which way the money
// went. A category it names that the family has files the row; one it does
// not leaves the row to the family's rules, which read the description too.
func TestABankStatementIsReadBySignAndFiled(t *testing.T) {
	url, c := newAPI(t)
	var card struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`, 201, &card)
	cats := categories(t, url, c)
	call(t, c, "POST", url+"/api/v1/category-rules",
		fmt.Sprintf(`{"category_id":%q,"field":"any","pattern":"пятёрочка"}`, cats["expense/Продукты"]), 201, nil)

	file := "Дата операции;Сумма операции;Валюта операции;Описание;Категория\n" +
		"01.09.2026;-1 200,00;RUB;ПЯТЕРОЧКА 4411;Супермаркеты\n" +
		"02.09.2026;-640,00;RUB;Яндекс Go;Транспорт / Такси\n" +
		"05.09.2026;180 000,00;RUB;ООО Ромашка;Зарплата\n" +
		"06.09.2026;-350,00;RUB;Кофейня;\n" +
		"07.09.2026;0,00;RUB;Проверка карты;\n"
	got := previewOf(t, url, c, card.ID, file)
	if _, typed := got.Mapping.Columns["type"]; typed || got.Mapping.Columns["note"] != 3 || got.Mapping.Columns["category"] != 4 {
		t.Fatalf("mapping = %+v, want no type, the description as the note, the category", got.Mapping.Columns)
	}
	want := []struct {
		typ      string
		amount   int64
		category string
	}{
		{"withdrawal", -120_000, cats["expense/Продукты"]},
		{"withdrawal", -64_000, cats["expense/Такси"]},
		{"deposit", 18_000_000, cats["income/Зарплата"]},
		{"withdrawal", -35_000, ""},
	}
	if note := got.Rows[0].Operation.Note; note != "ПЯТЕРОЧКА 4411 · Супермаркеты" {
		t.Errorf("the bank's own category is lost: note %q", note)
	}
	for i, w := range want {
		row := got.Rows[i]
		if row.Operation == nil || row.Operation.Type != w.typ || row.Operation.AmountMinor != w.amount {
			t.Errorf("row %d = %+v", i, row)
			continue
		}
		gotCat := ""
		if row.Operation.CategoryID != nil {
			gotCat = *row.Operation.CategoryID
		}
		if gotCat != w.category {
			t.Errorf("row %d filed under %q, want %q", i, gotCat, w.category)
		}
	}
	if last := got.Rows[4]; last.Verdict != "unparsed" || last.Reason == nil || last.Reason.Code != "no_type" {
		t.Errorf("a zero amount = %+v, want unparsed", last)
	}
}

// The family's rules do not reach a broker's account: an unfiled row there is
// money between the family and the broker.
func TestABrokersTableIsNotFiledByRules(t *testing.T) {
	url, c := newAPI(t)
	var broker struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB"}`, 201, &broker)
	cats := categories(t, url, c)
	call(t, c, "POST", url+"/api/v1/category-rules",
		fmt.Sprintf(`{"category_id":%q,"field":"any","pattern":"пополнение"}`, cats["income/Подарки"]), 201, nil)
	got := previewOf(t, url, c, broker.ID, "Дата;Тип;Сумма;Заметка\n01.09.2026;Пополнение;1000;Пополнение счёта\n")
	if op := got.Rows[0].Operation; op == nil || op.CategoryID != nil {
		t.Errorf("the broker's deposit = %+v, want unfiled", op)
	}
}

// A journal written out keeps who and what for, and reads back with them.
func TestAJournalKeepsItsFilingThroughATable(t *testing.T) {
	url, c := newAPI(t)
	var from, into struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`, 201, &from)
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Новая карта","type":"checking","currency":"RUB"}`, 201, &into)
	cats := categories(t, url, c)
	call(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-09-02","amount_minor":-64000,"currency":"RUB","counterparty":"Яндекс Go","category_id":%q}`,
		from.ID, cats["expense/Такси"]), 201, nil)

	resp, err := c.Get(url + "/api/v1/accounts/" + from.ID + "/journal.csv")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "Яндекс Go;Транспорт / Такси") {
		t.Fatalf("the file lacks the filing:\n%s", raw)
	}
	got := previewOf(t, url, c, into.ID, string(raw))
	op := got.Rows[0].Operation
	if op == nil || op.Counterparty != "Яндекс Go" || op.CategoryID == nil || *op.CategoryID != cats["expense/Такси"] {
		t.Errorf("read back = %+v", op)
	}

	// And the import writes them into the journal.
	body, _ := json.Marshal(map[string]any{"content": string(raw), "mapping": got.Mapping, "file_name": "card.csv"})
	call(t, c, "POST", url+"/api/v1/accounts/"+into.ID+"/imports", string(body), 200, nil)
	var journal struct {
		Operations []struct {
			Counterparty string  `json:"counterparty"`
			CategoryID   *string `json:"category_id"`
		} `json:"operations"`
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+into.ID+"/operations", "", 200, &journal)
	if len(journal.Operations) != 1 || journal.Operations[0].Counterparty != "Яндекс Go" ||
		journal.Operations[0].CategoryID == nil || *journal.Operations[0].CategoryID != cats["expense/Такси"] {
		t.Errorf("the imported journal = %+v", journal.Operations)
	}
}
