package family_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// newAPI spins up the full HTTP stack (server + family module) for tests.
// Reused by the account module tests via the same pattern.
func newAPI(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	store := family.NewStore(pool)
	svc := family.NewService(store)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, store)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(svc, store, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func TestSetupLoginMeLogoutFlow(t *testing.T) {
	ts, client := newAPI(t)

	// status: setup needed
	resp, _ := client.Get(ts.URL + "/api/v1/setup/status")
	var st struct {
		SetupNeeded bool `json:"setup_needed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if resp.StatusCode != 200 || !st.SetupNeeded {
		t.Fatalf("setup/status = %d %+v", resp.StatusCode, st)
	}

	// perform setup → 201, session cookie set
	resp = postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"Демо","username":"alex","display_name":"Alex","password":"secret123"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	var sess struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		Role      string `json:"role"`
		SpaceName string `json:"space_name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	if sess.Role != "owner" || sess.User.Username != "alex" || sess.SpaceName != "Демо" {
		t.Errorf("session info = %+v", sess)
	}

	// me works with cookie
	if resp, _ = client.Get(ts.URL + "/api/v1/auth/me"); resp.StatusCode != 200 {
		t.Fatalf("me = %d", resp.StatusCode)
	}

	// second setup → 409
	if resp = postJSON(t, client, ts.URL+"/api/v1/setup",
		`{"space_name":"X","username":"bb","display_name":"B","password":"12345678"}`); resp.StatusCode != 409 {
		t.Fatalf("second setup = %d, want 409", resp.StatusCode)
	}

	// logout → me becomes 401
	if resp = postJSON(t, client, ts.URL+"/api/v1/auth/logout", ``); resp.StatusCode != 204 {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	if resp, _ = client.Get(ts.URL + "/api/v1/auth/me"); resp.StatusCode != 401 {
		t.Fatalf("me after logout = %d, want 401", resp.StatusCode)
	}

	// login wrong / right
	if resp = postJSON(t, client, ts.URL+"/api/v1/auth/login",
		`{"username":"alex","password":"nope"}`); resp.StatusCode != 401 {
		t.Fatalf("bad login = %d", resp.StatusCode)
	}
	if resp = postJSON(t, client, ts.URL+"/api/v1/auth/login",
		`{"username":"alex","password":"secret123"}`); resp.StatusCode != 200 {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if resp, _ = client.Get(ts.URL + "/api/v1/auth/me"); resp.StatusCode != 200 {
		t.Fatalf("me after login = %d", resp.StatusCode)
	}
}
