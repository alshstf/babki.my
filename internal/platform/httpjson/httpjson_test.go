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
