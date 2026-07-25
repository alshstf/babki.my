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
