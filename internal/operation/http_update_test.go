package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
)

func put(t *testing.T, c *http.Client, url, id, body string) (int, string) {
	t.Helper()
	resp := apitest.Do(t, c, "PUT", url+"/api/v1/operations/"+id, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An operation entered by hand is edited in place: same id, same place among
// the operations of its day, the journal checked with the edit as it is when a
// new operation is entered.
func TestAnOperationIsEditedInPlace(t *testing.T) {
	pool, md := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(md))
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	sber := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	trade := func(typ, day, qty, price string) string {
		return fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":%q,"occurred_on":%q,"quantity":%q,"price":%q,"currency":"RUB"}`,
			acc, sber, typ, day, qty, price)
	}
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":1000000,"currency":"RUB"}`, acc))
	buy := mkOperation(t, url, c, trade("buy", "2026-07-10", "10", "300"))
	sell := mkOperation(t, url, c, trade("sell", "2026-07-10", "10", "310"))

	// The price was mistyped. The buy keeps its place ahead of the same day's
	// sell; entered again at the end of the day, it would come after the sell
	// and the sell would have nothing to sell.
	code, body := put(t, c, url, buy, trade("buy", "2026-07-10", "10", "290"))
	if code != http.StatusOK {
		t.Fatalf("edit the price = %d: %s", code, body)
	}
	rows := listJournal(t, url, c, acc)
	edited := findOperation(t, rows, buy)
	if edited.AmountMinor != -290_000 || len(rows) != 3 {
		t.Errorf("edited buy = %+v of %d rows, want −2 900 ₽ in place of the old one", edited, len(rows))
	}

	// A buy shrunk below what was sold after it, or moved after the sale, is
	// refused with the engine's reason, and nothing changes.
	for _, tc := range []struct{ name, body string }{
		{"fewer shares than were sold", trade("buy", "2026-07-10", "5", "290")},
		{"bought after the sale", trade("buy", "2026-07-11", "10", "290")},
	} {
		if code, body := put(t, c, url, buy, tc.body); code != http.StatusConflict {
			t.Errorf("%s = %d, want 409: %s", tc.name, code, body)
		}
	}
	if got := findOperation(t, listJournal(t, url, c, acc), buy); got.AmountMinor != -290_000 || got.OccurredOn != "2026-07-10" {
		t.Errorf("after refused edits the buy is %+v, want it as last edited", got)
	}

	// The sale's price too: it stays after the buy it sells from.
	if code, body := put(t, c, url, sell, trade("sell", "2026-07-10", "10", "315")); code != http.StatusOK {
		t.Fatalf("edit the sale's price = %d: %s", code, body)
	}

	// The sale can move later, and its note can change.
	code, body = put(t, c, url, sell, fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"type":"sell","occurred_on":"2026-07-12","quantity":"10","price":"310","currency":"RUB","note":"исправлена дата"}`,
		acc, sber))
	if code != http.StatusOK {
		t.Fatalf("move the sale = %d: %s", code, body)
	}
	if got := findOperation(t, listJournal(t, url, c, acc), sell); got.OccurredOn != "2026-07-12" {
		t.Errorf("moved sale = %+v, want dated 2026-07-12", got)
	}
}

// What cannot be edited in place is refused by name: a broker's row, a leg of a
// transfer, a change of account or of type, an operation that does not exist.
func TestWhatCannotBeEditedIsRefused(t *testing.T) {
	pool, md := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(md))
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	other := mkAccount(t, url, c, "Другой", "RUB")
	sber := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	deposit := func(account string) string {
		return fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":1000000,"currency":"RUB"}`, account)
	}
	dep := mkOperation(t, url, c, deposit(acc))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"10","price":"300","currency":"RUB"}`, acc, sber))
	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"5","occurred_on":"2026-07-15"}`, acc, other, sber))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	apitest.Decode(t, resp, &pair)
	imported := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-02","amount_minor":5000,"currency":"RUB"}`, acc))
	if _, err := pool.Exec(t.Context(), `UPDATE operations SET source = 'tinvest' WHERE id = $1`, imported); err != nil {
		t.Fatalf("mark imported: %v", err)
	}
	registry := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-03","amount_minor":100,"currency":"RUB"}`, acc))
	if _, err := pool.Exec(t.Context(), `UPDATE operations SET source = 'registry' WHERE id = $1`, registry); err != nil {
		t.Fatalf("mark the registry's: %v", err)
	}
	// Neither a broker's row nor the registry's is a person's to delete.
	for name, id := range map[string]string{"a broker's row": imported, "the registry's row": registry} {
		if resp := apitest.Do(t, c, "DELETE", url+"/api/v1/operations/"+id, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("deleting %s = %d, want 400", name, resp.StatusCode)
		}
	}

	for _, tc := range []struct {
		name, id, body string
		want           int
	}{
		{"a broker's row", imported, deposit(acc), http.StatusBadRequest},
		{"the registry's row", registry, deposit(acc), http.StatusBadRequest},
		{"a leg of a transfer", pair.In.ID, fmt.Sprintf(
			`{"account_id":%q,"instrument_id":%q,"type":"transfer_in","occurred_on":"2026-07-15","quantity":"4","amount_minor":120000,"currency":"RUB"}`,
			other, sber), http.StatusBadRequest},
		{"another account", dep, deposit(other), http.StatusBadRequest},
		{"another type", dep, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-07-01","amount_minor":-1000,"currency":"RUB"}`, acc), http.StatusBadRequest},
		{"no such operation", "0b8a5a8e-4c1e-4a43-9d0a-3f5b9c2d1e77", deposit(acc), http.StatusNotFound},
	} {
		if code, body := put(t, c, url, tc.id, tc.body); code != tc.want {
			t.Errorf("%s = %d, want %d: %s", tc.name, code, tc.want, body)
		}
	}
}
