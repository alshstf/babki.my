package instrument_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// newAPI wires the full stack: family + instrument modules.
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	url, c, _ := newAPIWithCatalog(t)
	return url, c
}

// newAPIWithCatalog is newAPI plus the store behind it, for the one kind of
// test that has to write a row the HTTP door would refuse: a row as it was
// written BEFORE that door refused it. Reaching past the handler is the only
// way to set such a state up, and a test that could not set it up could not
// check that the repair still works.
func newAPIWithCatalog(t *testing.T) (string, *http.Client, *instrument.Store) {
	t.Helper()
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	store := instrument.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	instrument.NewHandler(store, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("setup: %v %d", err, resp.StatusCode)
	}
	return ts.URL, client, store
}

func do(t *testing.T, c *http.Client, method, url, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// catalogPage is GET /api/v1/instruments as a client reads it: an envelope,
// not the bare array it used to be. Decoded into a hand-written struct rather
// than into apitypes so that a change to the generated types cannot quietly
// change what this test asserts about the wire.
type catalogPage struct {
	Instruments []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"instruments"`
	HasMore bool `json:"has_more"`
}

func searchCatalog(t *testing.T, c *http.Client, url string) catalogPage {
	t.Helper()
	resp := do(t, c, "GET", url, "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d, want 200: %s", url, resp.StatusCode, b)
	}
	var page catalogPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
	return page
}

func TestInstrumentsCatalog(t *testing.T) {
	url, c := newAPI(t)

	// create a bond with paired face fields
	resp := do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"ОФЗ 26238","ticker":"SU26238RMFS4","isin":"RU000A1038V6","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create bond = %d: %s", resp.StatusCode, b)
	}
	var bond struct {
		ID             string  `json:"id"`
		Ticker         string  `json:"ticker"`
		FaceValueMinor *int64  `json:"face_value_minor"`
		FaceCurrency   *string `json:"face_currency"`
		Frozen         bool    `json:"frozen"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bond)
	if bond.ID == "" || bond.Ticker != "SU26238RMFS4" || bond.FaceValueMinor == nil ||
		*bond.FaceValueMinor != 100000 || bond.FaceCurrency == nil || *bond.FaceCurrency != "RUB" {
		t.Fatalf("created bond = %+v", bond)
	}

	// create a share with a Cyrillic name and no face fields
	resp = do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create share = %d: %s", resp.StatusCode, b)
	}
	var share struct {
		ID             string `json:"id"`
		FaceValueMinor *int64 `json:"face_value_minor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&share)
	if share.ID == "" || share.FaceValueMinor != nil {
		t.Fatalf("created share = %+v", share)
	}

	// search by ticker fragment finds the share
	byTicker := searchCatalog(t, c, url+"/api/v1/instruments?query=SBER")
	if len(byTicker.Instruments) != 1 || byTicker.Instruments[0].Name != "Сбербанк" {
		t.Fatalf("search by ticker = %+v", byTicker)
	}

	// search by Cyrillic name fragment finds the share too
	byName := searchCatalog(t, c, url+"/api/v1/instruments?query="+neturl.QueryEscape("Сбер"))
	if len(byName.Instruments) != 1 || byName.Instruments[0].Name != "Сбербанк" {
		t.Fatalf("search by name = %+v", byName)
	}

	// limit narrows the result set instead of erroring, and the page says so
	limited := searchCatalog(t, c, url+"/api/v1/instruments?limit=1")
	if len(limited.Instruments) != 1 || !limited.HasMore {
		t.Fatalf("search limit=1 = %+v, want one instrument and has_more true", limited)
	}

	// patch: freeze the bond
	resp = do(t, c, "PATCH", url+"/api/v1/instruments/"+bond.ID, `{"frozen":true}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch frozen = %d: %s", resp.StatusCode, b)
	}
	var patched struct {
		Frozen bool `json:"frozen"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&patched)
	if !patched.Frozen {
		t.Fatalf("patched = %+v, want frozen", patched)
	}

	// patch: empty name is rejected
	if resp = do(t, c, "PATCH", url+"/api/v1/instruments/"+bond.ID, `{"name":""}`); resp.StatusCode != 400 {
		t.Errorf("patch empty name = %d, want 400", resp.StatusCode)
	}

	// validation errors on create
	for _, bad := range []string{
		`{"type":"nope","name":"X","currency":"RUB"}`,                       // invalid type
		`{"type":"share","name":"","currency":"RUB"}`,                       // empty name
		`{"type":"share","name":"X","currency":"russian rubles"}`,           // bad currency format
		`{"type":"bond","name":"X","currency":"RUB","face_value_minor":1}`,  // face value without currency
		`{"type":"bond","name":"X","currency":"RUB","face_currency":"RUB"}`, // face currency without value
	} {
		if resp = do(t, c, "POST", url+"/api/v1/instruments", bad); resp.StatusCode != 400 {
			t.Errorf("create %s = %d, want 400", bad, resp.StatusCode)
		}
	}

	// viewer can search but cannot write
	if resp = do(t, c, "POST", url+"/api/v1/members",
		`{"username":"vera","display_name":"V","password":"password9","role":"viewer"}`); resp.StatusCode != 201 {
		t.Fatalf("create viewer = %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	vera := &http.Client{Jar: jar}
	if resp = do(t, vera, "POST", url+"/api/v1/auth/login",
		`{"username":"vera","password":"password9"}`); resp.StatusCode != 200 {
		t.Fatalf("vera login = %d", resp.StatusCode)
	}
	if resp = do(t, vera, "GET", url+"/api/v1/instruments", ""); resp.StatusCode != 200 {
		t.Errorf("vera search = %d, want 200", resp.StatusCode)
	}
	if resp = do(t, vera, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"X","currency":"RUB"}`); resp.StatusCode != 403 {
		t.Errorf("vera create = %d, want 403", resp.StatusCode)
	}
}

// TestCatalogPagingReachesPastTheFirstPage is #104 at the door a client
// actually knocks on. The endpoint took no `offset` at all, so an instrument
// past the ceiling could not be reached by any request that could be made — and
// the frontend asked for fifty, which put the fifty-first out of reach of
// everything but a text search of a name nobody had to remember.
func TestCatalogPagingReachesPastTheFirstPage(t *testing.T) {
	url, c := newAPI(t)

	// Five instruments, named so that the order they come back in is known.
	const catalogSize = 5
	for i := 1; i <= catalogSize; i++ {
		body := fmt.Sprintf(`{"type":"share","name":"Бумага %02d","currency":"RUB"}`, i)
		if resp := do(t, c, "POST", url+"/api/v1/instruments", body); resp.StatusCode != 201 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("create %d = %d: %s", i, resp.StatusCode, b)
		}
	}

	first := searchCatalog(t, c, url+"/api/v1/instruments?limit=2")
	if len(first.Instruments) != 2 || !first.HasMore {
		t.Fatalf("first page = %+v, want two instruments and has_more true", first)
	}
	if first.Instruments[0].Name != "Бумага 01" || first.Instruments[1].Name != "Бумага 02" {
		t.Errorf("first page names = %+v, want Бумага 01 and Бумага 02", first.Instruments)
	}

	// The page nothing could reach before.
	third := searchCatalog(t, c, url+"/api/v1/instruments?limit=2&offset=4")
	if len(third.Instruments) != 1 || third.HasMore {
		t.Fatalf("third page = %+v, want one instrument and has_more false", third)
	}
	if third.Instruments[0].Name != "Бумага 05" {
		t.Errorf("third page = %q, want Бумага 05", third.Instruments[0].Name)
	}

	// The offset applies to a filtered listing too, not only to the whole
	// catalog: a query and a page are independent of each other.
	filtered := searchCatalog(t, c,
		url+"/api/v1/instruments?query="+neturl.QueryEscape("Бумага")+"&limit=1&offset=2")
	if len(filtered.Instruments) != 1 || filtered.Instruments[0].Name != "Бумага 03" || !filtered.HasMore {
		t.Fatalf("filtered page = %+v, want Бумага 03 and has_more true", filtered)
	}
}

// TestCatalogRefusesAPageItCannotHonour is #118 on the endpoint where #118 was
// found. A ceiling the contract states and the server does not apply is not a
// rule: the clamp this replaces answered a request for 250 as though it had
// asked for 200, with nothing in the answer saying that the number sent was not
// the number applied.
//
// The bounds are written as literals here — 200 and 1 — rather than read from
// the handler's constants, because a test that takes both sides of a comparison
// from the same declaration moves with it and proves nothing.
func TestCatalogRefusesAPageItCannotHonour(t *testing.T) {
	url, c := newAPI(t)

	for _, bad := range []string{
		"?limit=201", // one past the ceiling the contract states
		"?limit=999999",
		"?limit=0",
		"?limit=-1",
		"?limit=fifty",
		"?offset=-1",
		"?offset=half",
	} {
		resp := do(t, c, "GET", url+"/api/v1/instruments"+bad, "")
		if resp.StatusCode != 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("GET /api/v1/instruments%s = %d, want 400: %s", bad, resp.StatusCode, b)
			continue
		}
		var refusal struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil || refusal.Error == "" {
			t.Errorf("GET /api/v1/instruments%s answered 400 with no reason in the body (%v)", bad, err)
		}
	}

	// The ceiling itself is accepted: a bound is refused PAST, not AT.
	atCeiling := searchCatalog(t, c, url+"/api/v1/instruments?limit=200&offset=0")
	if atCeiling.HasMore {
		t.Errorf("has_more = true on an empty catalog read at the ceiling")
	}
}
