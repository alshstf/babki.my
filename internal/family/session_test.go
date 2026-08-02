package family_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/testdb"
)

func cookiejarNew() (http.CookieJar, error) { return cookiejar.New(nil) }

// Full round-trip: sign in sets a cookie, cookie authenticates, role gate works.
func TestSessionAuthFlow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := family.NewStore(pool)
	svc := family.NewService(store)
	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "alex", DisplayName: "A", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if err := auth.SignIn(r.Context(), owner.UserID); err != nil {
			httpjson.Error(w, 500, err.Error())
			return
		}
		w.WriteHeader(204)
	})
	mux.Handle("GET /whoami", auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := family.PrincipalFromContext(r.Context())
		if !ok {
			httpjson.Error(w, 500, "no principal")
			return
		}
		httpjson.Write(w, 200, map[string]string{"role": string(p.Role)})
	})))
	mux.Handle("GET /owner-only", auth.RequireAuth(family.RequireRole(family.RoleOwner,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))))
	mux.Handle("GET /editor-only", auth.RequireAuth(family.RequireRole(family.RoleEditor,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))))

	srv := httptest.NewServer(sm.LoadAndSave(mux))
	defer srv.Close()
	jar, _ := cookiejarNew()
	client := &http.Client{Jar: jar}

	// unauthenticated → 401
	resp, _ := client.Get(srv.URL + "/whoami")
	if resp.StatusCode != 401 {
		t.Fatalf("unauth whoami = %d, want 401", resp.StatusCode)
	}

	// login
	resp, _ = client.Post(srv.URL+"/login", "application/json", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("login = %d", resp.StatusCode)
	}

	// authenticated
	resp, _ = client.Get(srv.URL + "/whoami")
	if resp.StatusCode != 200 {
		t.Fatalf("auth whoami = %d, want 200", resp.StatusCode)
	}
	// owner passes both role gates
	if resp, _ = client.Get(srv.URL + "/owner-only"); resp.StatusCode != 204 {
		t.Fatalf("owner-only = %d", resp.StatusCode)
	}
	if resp, _ = client.Get(srv.URL + "/editor-only"); resp.StatusCode != 204 {
		t.Fatalf("editor-only = %d", resp.StatusCode)
	}
}
