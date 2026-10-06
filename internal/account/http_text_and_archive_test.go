package account_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/platform/apitest"
)

// A name and an institution are bounded in characters, not bytes: a hundred
// Cyrillic letters are two hundred bytes and still fit. Both doors that take
// them, creation and the PATCH, are asked about both fields.
func TestAccountTextsAreBoundedInCharacters(t *testing.T) {
	url, c := newAPI(t)
	full := strings.Repeat("ж", account.MaxNameRunes)
	over := full + "ж"

	if resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"`+full+`","type":"cash","currency":"RUB","institution":"`+strings.Repeat("ж", account.MaxInstitutionRunes)+`"}`); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("a name and an institution at their ceilings: %d %s", resp.StatusCode, body)
	}
	for name, body := range map[string]string{
		"name":        `{"name":"` + over + `","type":"cash","currency":"RUB"}`,
		"institution": `{"name":"Счёт","type":"cash","currency":"RUB","institution":"` + strings.Repeat("ж", account.MaxInstitutionRunes+1) + `"}`,
	} {
		if resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create with a %s one character too long = %d, want 400", name, resp.StatusCode)
		}
	}

	id := mkAccount(t, url, c, "Счёт", "RUB")
	for name, body := range map[string]string{
		"name":        `{"name":"` + over + `"}`,
		"institution": `{"institution":"` + strings.Repeat("ж", account.MaxInstitutionRunes+1) + `"}`,
	} {
		if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+id, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("rename with a %s one character too long = %d, want 400", name, resp.StatusCode)
		}
	}
	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+id, `{"name":"`+full+`"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("rename to a name at the ceiling = %d, want 200", resp.StatusCode)
	}
}

// The screen offers no balance for an archived account, and the server now
// refuses one too; bringing the account back from the archive opens it again.
func TestArchivedAccountTakesNoBalanceMark(t *testing.T) {
	url, c, id := newBoundedAccount(t)
	if resp := apitest.Do(t, c, "DELETE", url+"/api/v1/accounts/"+id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("archive = %d", resp.StatusCode)
	}
	resp := putBalance(t, url, c, id, 100)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "archived") {
		t.Errorf("balance on an archived account = %d %s, want 400 naming the archive", resp.StatusCode, body)
	}

	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+id, `{"status":"active"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("bring back = %d", resp.StatusCode)
	}
	if resp := putBalance(t, url, c, id, 100); resp.StatusCode != http.StatusOK {
		t.Errorf("balance once back from the archive = %d, want 200", resp.StatusCode)
	}
}
