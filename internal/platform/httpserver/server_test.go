package httpserver_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

func TestHealthzOK(t *testing.T) {
	pool := testdb.New(t)
	srv := httpserver.New(slog.Default(), pool)

	req := httptest.NewRequest("GET", "/api/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
}

func TestHealthzDegraded(t *testing.T) {
	pool := testdb.New(t)
	pool.Close() // simulate unavailable database
	srv := httpserver.New(slog.Default(), pool)

	req := httptest.NewRequest("GET", "/api/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestRoutesListsWhatWasMounted covers the three things Routes promises, since
// tests elsewhere derive their coverage from it: that a mounted pattern comes
// back with its method attached, that the framework's own routes are not in the
// list, and that the list handed out is a copy.
func TestRoutesListsWhatWasMounted(t *testing.T) {
	pool := testdb.New(t)
	srv := httpserver.New(slog.Default(), pool)
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	if got := srv.Routes(); len(got) != 0 {
		t.Errorf("a fresh server reports %v, want nothing: the healthcheck and the "+
			"/api/ catch-all are the framework's own and are not mounted routes", got)
	}

	srv.Mount("POST /api/v1/things", nothing)
	srv.Mount("GET /api/v1/things/{id}", nothing)
	want := []string{"POST /api/v1/things", "GET /api/v1/things/{id}"}
	got := srv.Routes()
	if !slices.Equal(got, want) {
		t.Fatalf("Routes() = %v, want %v", got, want)
	}

	// A caller writing into what it got back does not rewrite the router's own
	// record. Written in place rather than appended to, because appending to a
	// slice of exact capacity copies it whether or not Routes did.
	got[0] = "DELETE /api/v1/everything"
	if again := srv.Routes(); !slices.Equal(again, want) {
		t.Errorf("Routes() = %v after a caller overwrote an earlier result, want %v", again, want)
	}
}

func TestAPINotFoundIsJSON(t *testing.T) {
	pool := testdb.New(t)
	srv := httpserver.New(slog.Default(), pool)
	srv.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>spa</html>")) // imitate SPA mount
	}))

	req := httptest.NewRequest("GET", "/api/v1/definitely-not-there", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body is not JSON error: %s", rec.Body.String())
	}
}
