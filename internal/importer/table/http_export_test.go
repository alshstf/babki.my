package table_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An account's journal is written out as a table the import reads back: the
// same operations, the same amounts, the paper by its ISIN.
func TestAJournalWrittenOutReadsBackIn(t *testing.T) {
	url, c := newAPI(t)
	var from, into, sber struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Откуда","type":"brokerage","currency":"RUB"}`, 201, &from)
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Куда","type":"brokerage","currency":"RUB"}`, 201, &into)
	call(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, 201, &sber)
	op := func(body string) {
		call(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,%s}`, from.ID, body), 201, nil)
	}
	op(`"type":"deposit","occurred_on":"2026-07-01","amount_minor":1000050,"currency":"RUB","note":"с карты; первая"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"10","price":"305.5","fee_minor":153,"currency":"RUB"`, sber.ID))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"dividend","occurred_on":"2026-07-20","amount_minor":3000,"currency":"RUB"`, sber.ID))
	op(`"type":"withdrawal","occurred_on":"2026-07-25","amount_minor":-50000,"currency":"RUB"`)

	resp, err := c.Get(url + "/api/v1/accounts/" + from.ID + "/journal.csv")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %v %d", err, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	file := string(raw)
	for _, want := range []string{
		"Дата;Тип;Бумага;Количество;Цена;Сумма;Валюта;Комиссия;Заметка",
		"10.07.2026;Покупка;RU0009029540;10;305,5;-3055,00;RUB;1,53;",
		`01.07.2026;Пополнение;;;;10000,50;RUB;;"с карты; первая"`,
	} {
		if !strings.Contains(file, want) {
			t.Errorf("the file lacks %q:\n%s", want, file)
		}
	}

	body, _ := json.Marshal(map[string]string{"content": file})
	var got preview
	call(t, c, "POST", url+"/api/v1/accounts/"+into.ID+"/imports/preview", string(body), 200, &got)
	if len(got.Rows) != 4 {
		t.Fatalf("read back %d rows, want 4", len(got.Rows))
	}
	want := []int64{1_000_050, -305_500, 3000, -50000}
	for i, row := range got.Rows {
		if row.Verdict != "new" || row.Operation == nil || row.Operation.AmountMinor != want[i] {
			t.Errorf("row %d = %+v, want new with %d", i, row, want[i])
		}
	}
	if got.Rows[1].Operation.FeeMinor != 153 || *got.Rows[1].Operation.InstrumentID != sber.ID {
		t.Errorf("the buy read back as %+v, want SBER with a 1,53 fee", got.Rows[1].Operation)
	}
}
