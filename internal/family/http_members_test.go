package family_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
)

func setupOwner(t *testing.T) (string, *http.Client) {
	ts, client := newAPI(t)
	resp := postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	return ts.URL, client
}

func TestMembersCRUD(t *testing.T) {
	url, owner := setupOwner(t)

	// create member (owner)
	resp := postJSON(t, owner, url+"/api/v1/members",
		`{"username":"kate","display_name":"Kate","password":"password9","role":"editor"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create member = %d", resp.StatusCode)
	}
	var m struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if m.Role != "editor" || m.ID == "" {
		t.Fatalf("member = %+v", m)
	}

	// list shows two
	resp, _ = owner.Get(url + "/api/v1/members")
	var list []struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if resp.StatusCode != 200 || len(list) != 2 {
		t.Fatalf("list = %d, %d members", resp.StatusCode, len(list))
	}

	// editor cannot create members
	jar, _ := cookiejar.New(nil)
	kate := &http.Client{Jar: jar}
	if resp = postJSON(t, kate, url+"/api/v1/auth/login",
		`{"username":"kate","password":"password9"}`); resp.StatusCode != 200 {
		t.Fatalf("kate login = %d", resp.StatusCode)
	}
	if resp = postJSON(t, kate, url+"/api/v1/members",
		`{"username":"x","display_name":"X","password":"password9","role":"viewer"}`); resp.StatusCode != 403 {
		t.Fatalf("kate create member = %d, want 403", resp.StatusCode)
	}

	// owner demotes kate to viewer
	req, _ := http.NewRequest("PATCH", url+"/api/v1/members/"+m.ID,
		jsonBody(`{"role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = owner.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("patch member = %d", resp.StatusCode)
	}

	// owner deletes kate
	req, _ = http.NewRequest("DELETE", url+"/api/v1/members/"+m.ID, nil)
	resp, _ = owner.Do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("delete member = %d", resp.StatusCode)
	}

	// kate's session no longer authorized (membership gone)
	if resp, _ = kate.Get(url + "/api/v1/auth/me"); resp.StatusCode != 401 {
		t.Fatalf("kate me after removal = %d, want 401", resp.StatusCode)
	}
}

func TestOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	url, owner := setupOwner(t)

	// find own id via me
	resp, _ := owner.Get(url + "/api/v1/auth/me")
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)

	req, _ := http.NewRequest("PATCH", url+"/api/v1/members/"+me.User.ID, jsonBody(`{"role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, _ = owner.Do(req); resp.StatusCode != 400 {
		t.Fatalf("demote owner = %d, want 400", resp.StatusCode)
	}
	req, _ = http.NewRequest("DELETE", url+"/api/v1/members/"+me.User.ID, nil)
	if resp, _ = owner.Do(req); resp.StatusCode != 400 {
		t.Fatalf("delete owner = %d, want 400", resp.StatusCode)
	}
}

// TestCreateMemberDuplicateUsername verifies that creating a member with a
// username that's already taken (e.g. by the owner from setup) returns 409
// Conflict, not the 500 that an unmapped unique_violation would produce.
func TestCreateMemberDuplicateUsername(t *testing.T) {
	url, owner := setupOwner(t)

	resp := postJSON(t, owner, url+"/api/v1/members",
		`{"username":"alex","display_name":"Alex Clone","password":"password9","role":"editor"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create member with taken username = %d, want 409", resp.StatusCode)
	}
}
