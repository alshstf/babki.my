package httpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

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

// Routes returns mounted patterns with their method, omits the framework's own, and hands out a copy.
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

	// Written in place: appending to an exact-capacity slice would copy anyway.
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

// freePort returns a port nothing listens on. Something may take it before
// Run binds; then Run returns the error and the test fails visibly.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// Run lets an in-flight request finish (Shutdown, not Close), returns nil
// rather than ErrServerClosed, and closes the listener before returning.
func TestRunDrainsInFlightRequestsAndReturnsNil(t *testing.T) {
	pool := testdb.New(t)
	srv := httpserver.New(slog.Default(), pool)

	entered := make(chan struct{})
	srv.Mount("GET /api/v1/slow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("finished"))
	}))

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx, addr) }()

	// Wait for the listener, which Run starts on its own goroutine.
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/api/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the server never accepted a connection on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	slow := make(chan string, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/api/v1/slow")
		if err != nil {
			slow <- "request failed: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			slow <- "body failed: " + err.Error()
			return
		}
		slow <- string(body)
	}()

	<-entered // the handler is inside; now pull the rug
	cancel()

	if got := <-slow; got != "finished" {
		t.Errorf("the in-flight request answered %q, want \"finished\": "+
			"a request already being handled must survive the shutdown", got)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil: a shutdown the caller asked for is not an error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections after Run returned", addr)
	}
}

// hardened serves one write route and one page through the real middleware.
func hardened(t *testing.T) http.Handler {
	t.Helper()
	srv := httpserver.New(slog.Default(), testdb.New(t))
	srv.Mount("POST /api/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>spa</html>"))
	}))
	return srv.Handler()
}

// A write from another site, a sibling subdomain included, is refused; a
// request with neither header is not a browser's and passes.
func TestAWriteFromAnotherSiteIsRefused(t *testing.T) {
	h := hardened(t)
	for name, tc := range map[string]struct {
		headers map[string]string
		want    int
	}{
		"the application itself":            {map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
		"a request the user typed":          {map[string]string{"Sec-Fetch-Site": "none"}, http.StatusNoContent},
		"another site":                      {map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		"a sibling subdomain":               {map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		"an older browser, same origin":     {map[string]string{"Origin": "http://example.com"}, http.StatusNoContent},
		"an older browser, another origin":  {map[string]string{"Origin": "http://evil.test"}, http.StatusForbidden},
		"behind a proxy that rewrites Host": {map[string]string{"Origin": "https://babki.home", "X-Forwarded-Host": "babki.home"}, http.StatusNoContent},
		"not a browser at all":              {nil, http.StatusNoContent},
	} {
		req := httptest.NewRequest("POST", "/api/v1/thing", nil)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, tc.want)
		}
	}

	// Reads are never refused: links from other sites are how browsers arrive.
	req := httptest.NewRequest("GET", "/accounts", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a cross-site GET: %d, want 200", rec.Code)
	}
}

// Every answer carries the security headers, and API answers are not cached.
func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	h := hardened(t)
	for _, path := range []string{"/", "/api/healthz", "/api/v1/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "same-origin",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("GET %s: Content-Security-Policy = %q", path, csp)
		}
		isAPI := strings.HasPrefix(path, "/api/")
		if got := rec.Header().Get("Cache-Control"); isAPI != (got == "no-store") {
			t.Errorf("GET %s: Cache-Control = %q; an API answer is never stored, a page is left to its own handler", path, got)
		}
	}
}
