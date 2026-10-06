package background_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"babki.my/babki/internal/background"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// The watcher's address answers 503 naming a figure source that has not
// worked for three of its days, and 200 once it works again. A broker's sync
// left stale by a removed connection is not a figure source.
func TestTheDataHealthNamesAStaleFigureSource(t *testing.T) {
	pool := testdb.New(t)
	srv := httpserver.New(slog.Default(), pool)
	background.NewDataHealthHandler(pool).Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ask := func() (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/healthz/data")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	set := func(kind string, ago time.Duration) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO job_outcomes (kind, last_success_at) VALUES ($1, now() - $2::interval)
			ON CONFLICT (kind) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`,
			kind, ago.String()); err != nil {
			t.Fatal(err)
		}
	}

	set("marketdata.refresh_fx", 5*24*time.Hour)
	set("tinvest.sync", 30*24*time.Hour)
	code, body := ask()
	if code != http.StatusServiceUnavailable || body["status"] != "stale" {
		t.Fatalf("answer %d %v, want 503 stale", code, body)
	}
	if stale, _ := body["stale"].([]any); len(stale) != 1 || stale[0] != "marketdata.refresh_fx" {
		t.Errorf("stale = %v, want only the central bank's rates", body["stale"])
	}

	set("marketdata.refresh_fx", time.Hour)
	if code, body := ask(); code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("answer %d %v, want 200 ok", code, body)
	}
}

// Prometheus reads each source's outcome when it asks.
func TestTheSourcesAreCollectedForPrometheus(t *testing.T) {
	pool := testdb.New(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO job_outcomes (kind, last_success_at) VALUES
		('marketdata.refresh_quotes', now() - interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(background.NewSourcesCollector(pool))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "babki_source_stale" {
			continue
		}
		for _, m := range f.GetMetric() {
			stale[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	if stale["marketdata.refresh_quotes"] != 1 || stale["marketdata.refresh_fx"] != 0 {
		t.Errorf("stale = %v, want the exchange's quotes 1 and the rates, never run, 0", stale)
	}
	n, err := testutil.GatherAndCount(reg, "babki_source_last_success_timestamp_seconds")
	if err != nil || n != 1 {
		t.Errorf("last success series = %d (%v), want the one source that ever succeeded", n, err)
	}
}
