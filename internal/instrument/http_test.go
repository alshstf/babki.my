package instrument_test

import (
	"context"
	"encoding/json"
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
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// newAPI wires the full stack: family + instrument modules.
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	instrument.NewHandler(instrument.NewStore(pool), auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("setup: %v %d", err, resp.StatusCode)
	}
	return ts.URL, client
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
	resp = do(t, c, "GET", url+"/api/v1/instruments?query=SBER", "")
	var byTicker []struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&byTicker)
	if resp.StatusCode != 200 || len(byTicker) != 1 || byTicker[0].Name != "Сбербанк" {
		t.Fatalf("search by ticker = %d, %+v", resp.StatusCode, byTicker)
	}

	// search by Cyrillic name fragment finds the share too
	resp = do(t, c, "GET", url+"/api/v1/instruments?query="+neturl.QueryEscape("Сбер"), "")
	var byName []struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&byName)
	if resp.StatusCode != 200 || len(byName) != 1 || byName[0].Name != "Сбербанк" {
		t.Fatalf("search by name = %d, %+v", resp.StatusCode, byName)
	}

	// limit narrows the result set instead of erroring
	resp = do(t, c, "GET", url+"/api/v1/instruments?limit=1", "")
	var limited []struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&limited)
	if resp.StatusCode != 200 || len(limited) != 1 {
		t.Fatalf("search limit=1 = %d, %+v", resp.StatusCode, limited)
	}

	// a limit above the max is clamped rather than rejected: an oversized
	// limit is a harmless client detail, not a validation failure.
	resp = do(t, c, "GET", url+"/api/v1/instruments?limit=999999", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("search limit=999999 = %d, want 200 (clamped): %s", resp.StatusCode, b)
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
