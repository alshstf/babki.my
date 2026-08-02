package family_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"testing"
)

// costBasisRulesResp mirrors apitypes.CostBasisRules for decoding in tests.
type costBasisRulesResp struct {
	Country   string   `json:"country"`
	Method    string   `json:"method"`
	Perimeter string   `json:"perimeter"`
	Supported bool     `json:"supported"`
	Notices   []string `json:"notices"`
}

type sessionResp struct {
	BaseCurrency   string             `json:"base_currency"`
	TaxResidency   string             `json:"tax_residency"`
	CostBasisRules costBasisRulesResp `json:"cost_basis_rules"`
}

func me(t *testing.T, c *http.Client, url string) sessionResp {
	t.Helper()
	resp, err := c.Get(url + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("GET me: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET me = %d", resp.StatusCode)
	}
	var out sessionResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	return out
}

func patchSpace(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PATCH", url+"/api/v1/space", jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PATCH space: %v", err)
	}
	return resp
}

// TestTaxResidencyDefaultsToRussiaAndTravelsWithTheSession pins what an
// existing space reads back: RU, with the rules the engine actually implements
// and nothing to warn about.
func TestTaxResidencyDefaultsToRussiaAndTravelsWithTheSession(t *testing.T) {
	url, owner := setupOwner(t)

	got := me(t, owner, url)
	if got.TaxResidency != "RU" {
		t.Fatalf("tax_residency = %q, want RU", got.TaxResidency)
	}
	want := costBasisRulesResp{Country: "RU", Method: "fifo", Perimeter: "account", Supported: true, Notices: []string{}}
	if got.CostBasisRules.Country != want.Country || got.CostBasisRules.Method != want.Method ||
		got.CostBasisRules.Perimeter != want.Perimeter || !got.CostBasisRules.Supported ||
		len(got.CostBasisRules.Notices) != 0 {
		t.Fatalf("cost_basis_rules = %+v, want %+v", got.CostBasisRules, want)
	}
}

// TestChangingResidencyToBritainMakesTheSessionSayTheBasisIsNotBritish is the
// user-visible half of the honesty rule: the owner picks a country whose rules
// this application does not follow, and the session says so immediately —
// method AND perimeter, both wrong for Britain, both reported.
func TestChangingResidencyToBritainMakesTheSessionSayTheBasisIsNotBritish(t *testing.T) {
	url, owner := setupOwner(t)

	resp := patchSpace(t, owner, url, `{"tax_residency":"GB"}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH tax_residency = %d: %s", resp.StatusCode, b)
	}
	var updated sessionResp
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if updated.TaxResidency != "GB" || updated.CostBasisRules.Supported {
		t.Fatalf("patch response = %+v, want GB and supported=false", updated)
	}

	got := me(t, owner, url)
	if got.TaxResidency != "GB" {
		t.Fatalf("tax_residency after patch = %q, want GB", got.TaxResidency)
	}
	if got.CostBasisRules.Method != "average" || got.CostBasisRules.Perimeter != "owner" {
		t.Errorf("cost_basis_rules = %+v, want average/owner", got.CostBasisRules)
	}
	if got.CostBasisRules.Supported {
		t.Error("supported = true for GB, want false")
	}
	notices := slices.Clone(got.CostBasisRules.Notices)
	slices.Sort(notices)
	if !slices.Equal(notices, []string{"method_mismatch", "perimeter_mismatch"}) {
		t.Errorf("notices = %v, want [method_mismatch perimeter_mismatch]", notices)
	}

	// The base currency was not part of the request and must be untouched.
	if got.BaseCurrency != "RUB" {
		t.Errorf("base_currency = %q after a tax_residency-only patch, want RUB", got.BaseCurrency)
	}
}

// TestUnknownOrMalformedResidencyIsRefused covers the substitution the task
// names by hand. A country this application has no rules for must be rejected
// outright, and the stored value must be exactly what it was — accepting it and
// behaving like Russia is the silent wrongness the whole change removes.
func TestUnknownOrMalformedResidencyIsRefused(t *testing.T) {
	url, owner := setupOwner(t)

	for _, body := range []string{
		`{"tax_residency":"XX"}`, // well-formed, but no rules row
		`{"tax_residency":"FR"}`, // a real country this application cannot answer for
		`{"tax_residency":"ru"}`, // lowercase
		`{"tax_residency":"RUS"}`,
		`{"tax_residency":""}`,
		`{}`, // nothing to change: a no-op accepted as success is its own small lie
	} {
		if resp := patchSpace(t, owner, url, body); resp.StatusCode != 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("PATCH %s = %d, want 400: %s", body, resp.StatusCode, b)
		}
	}

	if got := me(t, owner, url); got.TaxResidency != "RU" {
		t.Fatalf("tax_residency after refused patches = %q, want RU untouched", got.TaxResidency)
	}
}

// TestResidencyAndCurrencyAreIndependentAndOwnerOnly pins that the two space
// settings can be changed together or apart, and that only the owner may.
func TestResidencyAndCurrencyAreIndependentAndOwnerOnly(t *testing.T) {
	url, owner := setupOwner(t)

	if resp := patchSpace(t, owner, url, `{"base_currency":"USD","tax_residency":"DE"}`); resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH both = %d: %s", resp.StatusCode, b)
	}
	got := me(t, owner, url)
	if got.BaseCurrency != "USD" || got.TaxResidency != "DE" {
		t.Fatalf("after patching both = %+v, want USD/DE", got)
	}
	// Germany queues per depot, first in first out — exactly what is computed.
	if !got.CostBasisRules.Supported || len(got.CostBasisRules.Notices) != 0 {
		t.Errorf("DE cost_basis_rules = %+v, want supported with no notices", got.CostBasisRules)
	}

	// A currency-only patch leaves the residency alone.
	if resp := patchSpace(t, owner, url, `{"base_currency":"EUR"}`); resp.StatusCode != 200 {
		t.Fatalf("PATCH currency only = %d", resp.StatusCode)
	}
	if got = me(t, owner, url); got.BaseCurrency != "EUR" || got.TaxResidency != "DE" {
		t.Fatalf("after currency-only patch = %+v, want EUR/DE", got)
	}

	// An editor may not change either.
	resp := postJSON(t, owner, url+"/api/v1/members",
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
	if resp = patchSpace(t, kate, url, `{"tax_residency":"KZ"}`); resp.StatusCode != 403 {
		t.Fatalf("editor PATCH tax_residency = %d, want 403", resp.StatusCode)
	}
}

// TestTaxResidenciesEndpointServesTheOneList checks that the list a client
// offers comes from the server. A client shipping its own copy would drift and
// eventually offer a country the server refuses.
func TestTaxResidenciesEndpointServesTheOneList(t *testing.T) {
	url, owner := setupOwner(t)

	resp, err := owner.Get(url + "/api/v1/tax-residencies")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET tax-residencies = %v %d", err, resp.StatusCode)
	}
	var list []costBasisRulesResp
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byCountry := make(map[string]costBasisRulesResp, len(list))
	codes := make([]string, 0, len(list))
	for _, r := range list {
		byCountry[r.Country] = r
		codes = append(codes, r.Country)
	}
	if !slices.IsSorted(codes) {
		t.Errorf("countries = %v, want sorted", codes)
	}
	for _, country := range []string{"RU", "DE", "KZ", "US", "GB", "CA", "AU", "NL", "CH"} {
		r, ok := byCountry[country]
		if !ok {
			t.Errorf("%s missing from the published list", country)
			continue
		}
		if len(r.Notices) == 0 != r.Supported {
			t.Errorf("%s: supported=%v with notices=%v — the two must agree", country, r.Supported, r.Notices)
		}
	}
	if _, ok := byCountry["XX"]; ok {
		t.Error("XX is published as a selectable residency")
	}

	// Unauthenticated callers get nothing.
	anon := &http.Client{}
	if resp, err = anon.Get(url + "/api/v1/tax-residencies"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous GET tax-residencies = %v %d, want 401", err, resp.StatusCode)
	}
}
