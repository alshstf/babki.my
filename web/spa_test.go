package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// build is a built application as Vite leaves it: the page, a hashed chunk and
// an unhashed file beside the page.
var build = fstest.MapFS{
	"index.html":               {Data: []byte(`<div id="root"></div>`)},
	"assets/index-CYSyLTS2.js": {Data: []byte(`console.log("app")`)},
	"favicon.svg":              {Data: []byte(`<svg/>`)},
}

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	spaHandler(build).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestAMissingChunkIsNotAnsweredWithThePage: after an upgrade a tab still open
// on the old build asks for a chunk that no longer exists. Answered with
// index.html and a 200, the browser refuses the "script" and the application
// dies with a blank screen (#201). It has to be a 404.
func TestAMissingChunkIsNotAnsweredWithThePage(t *testing.T) {
	for _, path := range []string{"/assets/family-CZ6lFP9q.js", "/assets/", "/assets"} {
		rec := get(t, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s answered with the page", path)
		}
		if strings.Contains(rec.Body.String(), "index-CYSyLTS2.js") {
			t.Errorf("GET %s listed the directory", path)
		}
	}
}

// TestAHashedFileIsCachedForGoodAndThePageNever: a name under assets/ carries
// its content's hash, so it may be kept for ever; the page is what points at
// those names, so it must be asked for again on every load or an upgrade never
// reaches the browser.
func TestAHashedFileIsCachedForGoodAndThePageNever(t *testing.T) {
	chunk := get(t, "/assets/index-CYSyLTS2.js")
	if chunk.Code != http.StatusOK || !strings.Contains(chunk.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("chunk: %d, Cache-Control %q; want 200 and immutable", chunk.Code, chunk.Header().Get("Cache-Control"))
	}
	for _, path := range []string{"/", "/accounts/42", "/favicon.svg"} {
		rec := get(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control %q, want no-cache", path, got)
		}
	}
	if route := get(t, "/accounts/42"); !strings.Contains(route.Body.String(), `id="root"`) {
		t.Error("a route of the application did not get the page")
	}
}
