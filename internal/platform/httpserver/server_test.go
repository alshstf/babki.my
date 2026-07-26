package httpserver_test

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
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
