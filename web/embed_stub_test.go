//go:build !embedui

package web_test

import (
	"net/http/httptest"
	"testing"

	"babki.my/babki/web"
)

func TestStubReturns503(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	web.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
