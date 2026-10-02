package operation_test

import (
	"fmt"
	"net/http"
	"testing"
)

// The journal is narrowed by type, by paper and by period, each on its own
// and together; a type that does not exist is refused by name.
func TestTheJournalIsFilteredByTypePaperAndPeriod(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	sber := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	gazp := mkInstrument(t, url, c, `{"type":"share","name":"Газпром","ticker":"GAZP","currency":"RUB"}`)
	op := func(body string) { mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,%s}`, acc, body)) }
	op(`"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"RUB"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-02-10","quantity":"10","price":"300","currency":"RUB"`, sber))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-03-10","quantity":"10","price":"150","currency":"RUB"`, gazp))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"dividend","occurred_on":"2026-07-10","amount_minor":30000,"currency":"RUB"`, sber))
	op(`"type":"withdrawal","occurred_on":"2026-08-10","amount_minor":-100000,"currency":"RUB"`)

	count := func(query string) int {
		t.Helper()
		resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+query, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", query, resp.StatusCode)
		}
		var page struct {
			Operations []struct{} `json:"operations"`
		}
		decodeJSON(t, resp, &page)
		return len(page.Operations)
	}
	for query, want := range map[string]int{
		"":                               5,
		"type=buy":                       2,
		"type=deposit&type=withdrawal":   2,
		"instrument_id=" + sber:          2,
		"from=2026-03-01&to=2026-07-31":  2,
		"type=buy&instrument_id=" + sber: 1,
		"from=2026-09-01":                0,
		"to=2026-01-10":                  1,
	} {
		if got := count(query); got != want {
			t.Errorf("%q gave %d operations, want %d", query, got, want)
		}
	}
	for _, query := range []string{"type=gift", "instrument_id=nope", "from=10.01.2026"} {
		if resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+query, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", query, resp.StatusCode)
		}
	}
}
