package account_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// newAPI wires the full stack: family + account modules.
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
	account.NewHandler(account.NewStore(pool), auth, sm).Mount(srv)

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

func TestAccountsCRUDAndBalance(t *testing.T) {
	url, c := newAPI(t)

	// create
	resp := do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокерский Т-Банк","type":"brokerage","currency":"RUB","institution":"Т-Банк"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, b)
	}
	var acc struct {
		ID      string `json:"id"`
		Balance any    `json:"balance"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&acc)
	if acc.ID == "" || acc.Balance != nil {
		t.Fatalf("created = %+v", acc)
	}

	// validation errors
	for _, bad := range []string{
		`{"name":"","type":"cash","currency":"RUB"}`,
		`{"name":"X","type":"nope","currency":"RUB"}`,
		`{"name":"X","type":"cash","currency":"russian rubles"}`,
	} {
		if resp = do(t, c, "POST", url+"/api/v1/accounts", bad); resp.StatusCode != 400 {
			t.Errorf("create %s = %d, want 400", bad, resp.StatusCode)
		}
	}

	// set balance + read list
	if resp = do(t, c, "PUT", url+"/api/v1/accounts/"+acc.ID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":150000000}`); resp.StatusCode != 200 {
		t.Fatalf("set balance = %d", resp.StatusCode)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts", "")
	var list []struct {
		Name    string `json:"name"`
		Balance *struct {
			AmountMinor int64  `json:"amount_minor"`
			AsOf        string `json:"as_of"`
		} `json:"balance"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Balance == nil || list[0].Balance.AmountMinor != 150000000 {
		t.Fatalf("list = %+v", list)
	}

	// bad balance payloads
	for _, bad := range []string{
		`{"as_of":"20.07.2026","amount_minor":1}`,
		`{"as_of":"2099-01-01","amount_minor":1}`,
	} {
		if resp = do(t, c, "PUT", url+"/api/v1/accounts/"+acc.ID+"/balance", bad); resp.StatusCode != 400 {
			t.Errorf("balance %s = %d, want 400", bad, resp.StatusCode)
		}
	}

	// patch + archive
	if resp = do(t, c, "PATCH", url+"/api/v1/accounts/"+acc.ID,
		`{"name":"Брокер Т"}`); resp.StatusCode != 200 {
		t.Fatalf("patch = %d", resp.StatusCode)
	}
	if resp = do(t, c, "DELETE", url+"/api/v1/accounts/"+acc.ID, ""); resp.StatusCode != 204 {
		t.Fatalf("archive = %d", resp.StatusCode)
	}

	// viewer cannot write
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
	if resp = do(t, vera, "GET", url+"/api/v1/accounts", ""); resp.StatusCode != 200 {
		t.Errorf("vera list = %d, want 200", resp.StatusCode)
	}
	if resp = do(t, vera, "POST", url+"/api/v1/accounts",
		`{"name":"X","type":"cash","currency":"RUB"}`); resp.StatusCode != 403 {
		t.Errorf("vera create = %d, want 403", resp.StatusCode)
	}
}
