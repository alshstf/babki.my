package family_test

import (
	"net/http"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
)

// A space name and a display name are bounded in characters, as a password is:
// a hundred Cyrillic letters are two hundred bytes and still a name of a
// hundred characters. Both doors that take a display name are asked.
func TestNamesHaveACeiling(t *testing.T) {
	ts, client := newAPI(t)
	full := strings.Repeat("я", family.MaxNameRunes)
	over := full + "я"

	for field, body := range map[string]string{
		"space_name":   `{"space_name":"` + over + `","username":"alex","display_name":"Alex","password":"secret123"}`,
		"display_name": `{"space_name":"Демо","username":"alex","display_name":"` + over + `","password":"secret123"}`,
	} {
		if resp := postJSON(t, client, ts.URL+"/api/v1/setup", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("setup with a %s one character too long = %d, want 400", field, resp.StatusCode)
		}
	}
	resp := postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"`+full+`","username":"alex","display_name":"`+full+`","password":"secret123"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup with both names at the ceiling = %d, want 201", resp.StatusCode)
	}

	member := func(name string) *http.Response {
		return postJSON(t, client, ts.URL+"/api/v1/members",
			`{"username":"vera","display_name":"`+name+`","password":"password9","role":"editor"}`)
	}
	if resp := member(over); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a member with a display name one character too long = %d, want 400", resp.StatusCode)
	}
	if resp := member(full); resp.StatusCode != http.StatusCreated {
		t.Errorf("a member with a display name at the ceiling = %d, want 201", resp.StatusCode)
	}
}
