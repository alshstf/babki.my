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

// freePort returns a TCP port nothing is listening on, by binding one and
// letting go of it again. There is a gap between the release and Run's own
// bind, and nothing can close it — (*Server).Run takes an address and does its
// own Listen, so a test cannot hand it a listener. The gap is not silent,
// which is what makes it acceptable: if something takes the port in between,
// ListenAndServe fails and Run returns that error to the assertions below,
// rather than the test hanging or passing on nothing.
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

// TestRunDrainsInFlightRequestsAndReturnsNil covers (*Server).Run, which had
// no test of its own: a real listener, a request still being handled when the
// context is cancelled, and what the method hands back afterwards.
//
// THE IN-FLIGHT REQUEST IS THE POINT. Shutdown differs from Close in exactly
// one visible way — it lets a handler that has already started finish and
// answer — so the slow handler below is started, then the context is cancelled
// while it is still inside, and its answer must arrive intact. Swapping
// Shutdown for Close turns this red, and nothing else in the package notices.
//
// The other two claims are Run's own postconditions: it returns nil rather
// than http.ErrServerClosed (the caller asked for the shutdown; it is not an
// error), and it does not return until the listener is closed — checked by
// dialling the port afterwards and requiring a refusal.
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

	// Wait for the listener to come up: Run starts it on a goroutine of its
	// own, so there is no moment before this at which a request would arrive.
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
