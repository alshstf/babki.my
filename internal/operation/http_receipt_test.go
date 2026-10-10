package operation_test

import (
	"fmt"
	"net/http"
	"testing"

	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
)

// A receipt written once is found by its numbers, on whichever account; a
// longer document number with the same start is another receipt.
func TestAReceiptWrittenIsFoundByItsNumbers(t *testing.T) {
	url, c := newAPI(t)
	card, cash := mkAccount(t, url, c, "Карта", "RUB"), mkAccount(t, url, c, "Наличные", "RUB")
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-01","amount_minor":10000000,"currency":"RUB"}`, card))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-10-09","amount_minor":-123450,"currency":"RUB","note":"Чек 09.10.2026 19:15, ФН 7380440700000000, ФД 22"}`, card))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-01","amount_minor":1000000,"currency":"RUB"}`, cash))
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-10-08","amount_minor":-50000,"currency":"RUB","note":"Чек 08.10.2026 10:00, ФН 7380440700000000, ФД 223"}`, cash))

	var got []apitypes.ReceiptMatch
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/operations/receipt?fn=7380440700000000&fd=22", ""), &got)
	if len(got) != 1 || got[0].AccountId.String() != card || got[0].AmountMinor != -123450 || got[0].OccurredOn != "2026-10-09" {
		t.Errorf("ФД 22 = %+v", got)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/operations/receipt?fn=7380440700000000&fd=9", ""), &got)
	if len(got) != 0 {
		t.Errorf("an unknown receipt = %+v", got)
	}
	for _, q := range []string{"fn=abc&fd=1", "fn=1&fd=", "fn=1.*&fd=1"} {
		if r := apitest.Do(t, c, "GET", url+"/api/v1/operations/receipt?"+q, ""); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, r.StatusCode)
		}
	}
}
