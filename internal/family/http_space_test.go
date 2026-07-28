package family_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// TestUpdateSpaceBaseCurrency verifies PATCH /api/v1/space: an owner can
// change the space's base currency and the new value is reflected both in
// the PATCH response and in a subsequent GET /api/v1/auth/me; an editor is
// forbidden; and a malformed currency code is rejected.
func TestUpdateSpaceBaseCurrency(t *testing.T) {
	url, owner := setupOwner(t)

	// default is RUB (migration 0006 default)
	resp, _ := owner.Get(url + "/api/v1/auth/me")
	var me struct {
		BaseCurrency string `json:"base_currency"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if resp.StatusCode != 200 || me.BaseCurrency != "RUB" {
		t.Fatalf("me before update = %d %+v, want 200 RUB", resp.StatusCode, me)
	}

	// owner updates base currency
	req, _ := http.NewRequest("PATCH", url+"/api/v1/space", jsonBody(`{"base_currency":"USD"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := owner.Do(req)
	if err != nil {
		t.Fatalf("PATCH space: %v", err)
	}
	var updated struct {
		BaseCurrency string `json:"base_currency"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	if resp.StatusCode != 200 || updated.BaseCurrency != "USD" {
		t.Fatalf("PATCH space = %d %+v, want 200 USD", resp.StatusCode, updated)
	}

	// me now reflects the new currency
	resp, _ = owner.Get(url + "/api/v1/auth/me")
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if resp.StatusCode != 200 || me.BaseCurrency != "USD" {
		t.Fatalf("me after update = %d %+v, want 200 USD", resp.StatusCode, me)
	}

	// add an editor and confirm they cannot change the base currency
	resp = postJSON(t, owner, url+"/api/v1/members",
		`{"username":"kate","display_name":"Kate","password":"password9","role":"editor"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create member = %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	kate := &http.Client{Jar: jar}
	if resp = postJSON(t, kate, url+"/api/v1/auth/login",
		`{"username":"kate","password":"password9"}`); resp.StatusCode != 200 {
		t.Fatalf("kate login = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("PATCH", url+"/api/v1/space", jsonBody(`{"base_currency":"EUR"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err = kate.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("editor PATCH space = %d, %v; want 403", resp.StatusCode, err)
	}

	// malformed currency code is rejected
	req, _ = http.NewRequest("PATCH", url+"/api/v1/space", jsonBody(`{"base_currency":"usd"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err = owner.Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatalf("PATCH space with lowercase currency = %d, %v; want 400", resp.StatusCode, err)
	}
	req, _ = http.NewRequest("PATCH", url+"/api/v1/space", jsonBody(`{"base_currency":"US"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err = owner.Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatalf("PATCH space with 2-letter currency = %d, %v; want 400", resp.StatusCode, err)
	}
}
