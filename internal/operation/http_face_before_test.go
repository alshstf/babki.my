package operation_test

import (
	"fmt"
	"testing"

	"babki.my/babki/internal/platform/apitest"
)

// A hand-entered amortization may carry the bond's face value before it
// (Р-4): the journal gives it back, an update without it clears it, and any
// other type, or a value below one minor unit, is refused.
func TestAHandEnteredAmortizationCarriesTheFaceBefore(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	bond := mkInstrument(t, url, c, `{"type":"bond","name":"ОФЗ","ticker":"OFZ","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-03-02",
		"quantity":"10","price":"990","amount_minor":-990000,"currency":"RUB"}`, acc, bond))

	amortization := func(extra string) string {
		return fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"amortization","occurred_on":"2026-04-01",
			"amount_minor":250000,"currency":"RUB"%s}`, acc, bond, extra)
	}
	id := mkOperation(t, url, c, amortization(`,"face_before_minor":100000`))
	if got := findOperation(t, listJournal(t, url, c, acc), id).FaceBeforeMinor; got == nil || *got != 100000 {
		t.Fatalf("face_before_minor = %v, want 100000", got)
	}

	if resp := apitest.Do(t, c, "PUT", url+"/api/v1/operations/"+id, amortization(``)); resp.StatusCode != 200 {
		t.Fatalf("update without the face value = %d, want 200", resp.StatusCode)
	}
	if got := findOperation(t, listJournal(t, url, c, acc), id).FaceBeforeMinor; got != nil {
		t.Errorf("face_before_minor after an update without it = %d, want null", *got)
	}

	for name, body := range map[string]string{
		"on a coupon": fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon","occurred_on":"2026-04-01",
			"amount_minor":5000,"currency":"RUB","face_before_minor":100000}`, acc, bond),
		"of zero": amortization(`,"face_before_minor":0`),
	} {
		if resp := apitest.Do(t, c, "POST", url+"/api/v1/operations", body); resp.StatusCode != 400 {
			t.Errorf("face_before_minor %s = %d, want 400", name, resp.StatusCode)
		}
	}
}
