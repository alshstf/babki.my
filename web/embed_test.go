//go:build embedui

package web_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/web"
)

// Requires built web/dist (make ui).
func TestEmbedServesIndexAndFallback(t *testing.T) {
	h := web.Handler()
	for _, path := range []string{"/", "/accounts/42"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Errorf("GET %s: body is not SPA index.html", path)
		}
	}
}

// TestEmbedServesTheInstallFiles: a phone installs the app from its manifest
// and shows the offline page through the worker (#404). Answered with
// index.html, as a route would be, the install silently fails.
func TestEmbedServesTheInstallFiles(t *testing.T) {
	h := web.Handler()
	for path, want := range map[string]string{
		"/manifest.json":        `"start_url"`,
		"/sw.js":                "offline.html",
		"/offline.html":         "Нет связи",
		"/icon-512.png":         "PNG",
		"/apple-touch-icon.png": "PNG",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
			continue
		}
		if body := rec.Body.String(); !strings.Contains(body, want) || strings.Contains(body, `<div id="root">`) {
			t.Errorf("GET %s: not the file itself", path)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache so an update reaches the phone", path, cc)
		}
	}
}
