package operation_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"babki.my/babki/internal/platform/apitest"
)

// journalStatedBasis reads one account's journal page and returns, by operation
// type, the raw stated_basis_change_minor each row carries.
func journalStatedBasis(t *testing.T, c *http.Client, url, accountID string) map[string]string {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("journal of %s = %d", accountID, resp.StatusCode)
	}
	var page struct {
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	apitest.Decode(t, resp, &page)
	out := map[string]string{}
	for _, op := range page.Operations {
		var typ string
		_ = json.Unmarshal(op["type"], &typ)
		out[typ] = string(op["stated_basis_change_minor"])
	}
	return out
}

// Ten shares bought for 1 000 ₽ moved to another account with a basis typed as
// 1 500 ₽: the family's basis of them grew by 500 ₽, and both halves of the
// move say so. A move whose basis the queue worked out says nothing.
func TestTheJournalSaysHowMuchATypedBasisChangedTheFamilysCost(t *testing.T) {
	f := newArrivalFixture(t)
	other := createID(t, f.c, f.url+"/api/v1/accounts", `{"name":"Другой","type":"brokerage","currency":"RUB"}`)
	if resp := apitest.Do(t, f.c, "POST", f.url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-06-01","quantity":"10","price":"100","amount_minor":-100000,"currency":"RUB"}`, other, f.sberID)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("buy = %d", resp.StatusCode)
	}
	third := createID(t, f.c, f.url+"/api/v1/accounts", `{"name":"Третий","type":"brokerage","currency":"RUB"}`)
	if resp := apitest.Do(t, f.c, "POST", f.url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-07-01","cost_minor":150000}`,
		other, third, f.sberID)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer with a typed basis = %d", resp.StatusCode)
	}

	for _, account := range []string{other, third} {
		got := journalStatedBasis(t, f.c, f.url, account)
		for _, typ := range []string{"transfer_out", "transfer_in"} {
			if v, ok := got[typ]; ok && v != "50000" {
				t.Errorf("%s on %s: stated_basis_change_minor = %s, want 50000", typ, account, v)
			}
		}
	}
	if got := journalStatedBasis(t, f.c, f.url, other)["buy"]; got != "null" {
		t.Errorf("a purchase carries stated_basis_change_minor = %s, want null", got)
	}

	// The queue's own move: nothing to say.
	if resp := apitest.Do(t, f.c, "POST", f.url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-02"}`,
		f.accountID, other, f.sberID)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer = %d", resp.StatusCode)
	}
	if got := journalStatedBasis(t, f.c, f.url, f.accountID)["transfer_out"]; got != "null" {
		t.Errorf("a move the queue priced carries stated_basis_change_minor = %s, want null", got)
	}
}
