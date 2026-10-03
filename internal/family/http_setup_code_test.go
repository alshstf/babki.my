package family_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// Where the server was given a setup code, first-run setup asks for it: the
// status says so, a missing or wrong code is a 403 that creates nobody, and
// the right one — typed in lower case with spaces around — goes through.
func TestFirstRunSetupAsksForTheCodeFromTheLog(t *testing.T) {
	pool := testdb.New(t)
	store := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, store)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(store), store, auth, sm).WithSetupCode("K7M2P9QX").Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := ts.Client()

	resp, err := c.Get(ts.URL + "/api/v1/setup/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		SetupNeeded  bool `json:"setup_needed"`
		CodeRequired bool `json:"code_required"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&status)
	if !status.SetupNeeded || !status.CodeRequired {
		t.Errorf("status = %+v, want setup needed and a code required", status)
	}

	body := func(code string) string {
		return `{"space_name":"S","username":"alex","display_name":"A","password":"secret123"` + code + `}`
	}
	for name, b := range map[string]string{
		"no code":    body(""),
		"wrong code": body(`,"setup_code":"AAAAAAAA"`),
	} {
		if resp := postJSON(t, c, ts.URL+"/api/v1/setup", b); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, resp.StatusCode)
		}
	}
	if n, _ := store.CountUsers(t.Context()); n != 0 {
		t.Fatalf("refused setups created %d users", n)
	}
	if resp := postJSON(t, c, ts.URL+"/api/v1/setup", body(`,"setup_code":" k7m2p9qx "`)); resp.StatusCode != http.StatusCreated {
		t.Errorf("the right code = %d, want 201", resp.StatusCode)
	}
}

func TestASetupCodeIsEightCharactersReadWithoutMistakes(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		code := family.NewSetupCode()
		if len(code) != 8 || strings.ContainsAny(code, "01OIL") {
			t.Fatalf("code %q: want eight characters with no 0, 1, O, I or L", code)
		}
		seen[code] = true
	}
	if len(seen) < 45 {
		t.Errorf("%d distinct codes out of 50, want them random", len(seen))
	}
}
