package httpjson_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/httpjson"
)

func TestWriteAndError(t *testing.T) {
	rec := httptest.NewRecorder()
	httpjson.Write(rec, 201, map[string]string{"ok": "yes"})
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"ok":"yes"`) {
		t.Fatalf("Write: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}

	rec = httptest.NewRecorder()
	httpjson.Error(rec, 404, "not found")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"error":"not found"`) {
		t.Fatalf("Error: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDecode(t *testing.T) {
	type req struct {
		Name string `json:"name"`
	}
	// happy path
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	var dst req
	if err := httpjson.Decode(rec, r, &dst); err != nil || dst.Name != "x" {
		t.Fatalf("Decode happy: err=%v dst=%+v", err, dst)
	}
	// unknown field → error + 400 written
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"nope":1}`))
	rec = httptest.NewRecorder()
	if err := httpjson.Decode(rec, r, &dst); err == nil {
		t.Fatal("Decode unknown field: want error")
	}
	if rec.Code != 400 {
		t.Fatalf("Decode unknown field: code=%d, want 400", rec.Code)
	}
}

// A valid body over the limit gets 413. One big string field, so a Decode
// without MaxBytesReader would answer 200, not 400 by accident.
func TestDecodeRefusesABodyOverTheLimit(t *testing.T) {
	type req struct {
		Name string `json:"name"`
	}
	// 1MB is the limit; 2MB of payload is unambiguously over it.
	body := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var dst req
	if err := httpjson.Decode(rec, r, &dst); err == nil {
		t.Fatal("Decode accepted a 2MB body, want an error")
	}
	if rec.Code != 413 {
		t.Fatalf("code = %d, want 413: an oversized body is not a malformed one", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Errorf("body = %s, want the too-large message", rec.Body.String())
	}
}
