package table_test

import (
	"encoding/json"
	"fmt"
	"testing"
)

type tableImport struct {
	ID             string  `json:"id"`
	FileName       string  `json:"file_name"`
	RowsWritten    int     `json:"rows_written"`
	RowsDuplicate  int     `json:"rows_duplicate"`
	RowsUnparsed   int     `json:"rows_unparsed"`
	RowsRefused    int     `json:"rows_refused"`
	RolledBackAt   *string `json:"rolled_back_at"`
	OperationsLeft int     `json:"operations_left"`
}

type importResult struct {
	Import *tableImport `json:"import"`
	Rows   []struct {
		Line    int    `json:"line"`
		Verdict string `json:"verdict"`
	} `json:"rows"`
}

const mapping = `{"has_header":true,"columns":{"date":0,"type":1,"instrument":2,"quantity":3,"price":4,"amount":5},` +
	`"types":{"пополнение":"deposit","покупка":"buy","продажа":"sell"}}`

func importBody(t *testing.T, csv, name string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"content": csv, "mapping": json.RawMessage(mapping), "file_name": name})
	return string(b)
}

// A table is imported once: loading the same file again writes nothing new,
// the account lists both loads, and the first can be taken back whole — but
// not while a later operation rests on what it wrote.
func TestATableIsImportedOnceAndRolledBackWhole(t *testing.T) {
	url, c := newAPI(t)
	var acc, sber struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Альфа","type":"brokerage","currency":"RUB"}`, 201, &acc)
	call(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, 201, &sber)
	csv := "Дата;Операция;Бумага;Количество;Цена;Сумма\n" +
		"01.07.2026;Пополнение;;;;10 000\n" +
		"10.07.2026;Покупка;SBER;10;300;\n" +
		"12.07.2026;Продажа;SBER;100;320;\n" + // more than held
		"13.07.2026;Покупка;GAZP;1;150;\n" // not in the catalog
	imports := url + "/api/v1/accounts/" + acc.ID + "/imports"

	var first importResult
	call(t, c, "POST", imports, importBody(t, csv, "alfa-2026.csv"), 200, &first)
	if first.Import == nil || first.Import.RowsWritten != 2 || first.Import.RowsRefused != 1 ||
		first.Import.RowsUnparsed != 1 || first.Import.FileName != "alfa-2026.csv" || first.Import.OperationsLeft != 2 {
		t.Fatalf("first import = %+v, want 2 written, 1 refused, 1 unparsed", first.Import)
	}
	var journal struct {
		Operations []struct {
			Source string `json:"source"`
		} `json:"operations"`
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &journal)
	if len(journal.Operations) != 2 || journal.Operations[0].Source != "csv" {
		t.Fatalf("journal = %+v, want the two rows, from a table", journal.Operations)
	}

	var again importResult
	call(t, c, "POST", imports, importBody(t, csv, "alfa-2026.csv"), 200, &again)
	if again.Import != nil {
		t.Errorf("loading the same file again recorded %+v, want nothing new", again.Import)
	}
	if again.Rows[0].Verdict != "duplicate" || again.Rows[1].Verdict != "duplicate" {
		t.Errorf("second load verdicts = %+v, want the written rows as duplicates", again.Rows)
	}

	// A sale entered by hand now rests on the imported purchase.
	call(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"type":"sell","occurred_on":"2026-07-20","quantity":"5","price":"330","currency":"RUB"}`,
		acc.ID, sber.ID), 201, nil)
	call(t, c, "DELETE", url+"/api/v1/imports/"+first.Import.ID, "", 409, nil)

	var listed []tableImport
	call(t, c, "GET", imports, "", 200, &listed)
	if len(listed) != 1 || listed[0].RolledBackAt != nil {
		t.Fatalf("imports = %+v, want the one, not rolled back", listed)
	}

	// Without the sale it goes.
	var hand struct {
		Operations []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"operations"`
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &hand)
	for _, o := range hand.Operations {
		if o.Source == "manual" {
			call(t, c, "DELETE", url+"/api/v1/operations/"+o.ID, "", 204, nil)
		}
	}
	var rolled tableImport
	call(t, c, "DELETE", url+"/api/v1/imports/"+first.Import.ID, "", 200, &rolled)
	if rolled.RolledBackAt == nil || rolled.OperationsLeft != 0 {
		t.Errorf("rolled back = %+v, want marked, with no operations left", rolled)
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &journal)
	if len(journal.Operations) != 0 {
		t.Errorf("journal after rollback has %d rows, want none", len(journal.Operations))
	}
	call(t, c, "DELETE", url+"/api/v1/imports/"+first.Import.ID, "", 400, nil)

	// And the file can be loaded again.
	var third importResult
	call(t, c, "POST", imports, importBody(t, csv, "alfa-2026.csv"), 200, &third)
	if third.Import == nil || third.Import.RowsWritten != 2 {
		t.Errorf("reloading after the rollback = %+v, want the 2 rows written again", third.Import)
	}
}

// Rows loaded from a table are the person's own: they are edited and deleted
// in the journal like hand entries. Loading the same file again brings back a
// deleted row and leaves an edited one as it was edited.
func TestRowsFromATableAreThePersonsOwn(t *testing.T) {
	url, c := newAPI(t)
	var acc struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Счёт","type":"brokerage","currency":"RUB"}`, 201, &acc)
	csv := "Дата;Операция;Бумага;Количество;Цена;Сумма\n" +
		"01.07.2026;Пополнение;;;;10 000\n" +
		"02.07.2026;Пополнение;;;;500\n"
	imports := url + "/api/v1/accounts/" + acc.ID + "/imports"
	call(t, c, "POST", imports, importBody(t, csv, "a.csv"), 200, nil)

	var journal struct {
		Operations []struct {
			ID          string `json:"id"`
			OccurredOn  string `json:"occurred_on"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"operations"`
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &journal)
	var first, second string
	for _, o := range journal.Operations {
		if o.OccurredOn == "2026-07-01" {
			first = o.ID
		} else {
			second = o.ID
		}
	}
	call(t, c, "PUT", url+"/api/v1/operations/"+first, fmt.Sprintf(
		`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":1100000,"currency":"RUB"}`, acc.ID), 200, nil)
	call(t, c, "DELETE", url+"/api/v1/operations/"+second, "", 204, nil)

	var again importResult
	call(t, c, "POST", imports, importBody(t, csv, "a.csv"), 200, &again)
	if again.Rows[0].Verdict != "duplicate" || again.Rows[1].Verdict != "new" {
		t.Errorf("reload verdicts = %+v, want the edited row a duplicate and the deleted one back", again.Rows)
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &journal)
	amounts := map[string]int64{}
	for _, o := range journal.Operations {
		amounts[o.OccurredOn] = o.AmountMinor
	}
	if len(journal.Operations) != 2 || amounts["2026-07-01"] != 1_100_000 || amounts["2026-07-02"] != 50_000 {
		t.Errorf("journal = %+v, want the edit kept and the deleted row back", journal.Operations)
	}
}
