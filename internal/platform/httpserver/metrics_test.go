package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/metrics"
)

// A request is counted by the route it matched, not its path, so an id does
// not make a series of its own.
func TestRequestsAreCountedByTheirRoute(t *testing.T) {
	srv := httpserver.New(slog.Default(), nil)
	srv.Mount("GET /api/v1/things/{thingId}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	srv.Mount("GET /metrics", metrics.Handler())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	for _, id := range []string{"a1", "b2"} {
		resp, err := http.Get(ts.URL + "/api/v1/things/" + id)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	want := `babki_http_requests_total{method="GET",route="GET /api/v1/things/{thingId}",status="418"} 2`
	if !strings.Contains(string(body), want) {
		t.Errorf("metrics lack %s", want)
	}
	if strings.Contains(string(body), "/things/a1") {
		t.Error("a path with its id made a series")
	}
}
