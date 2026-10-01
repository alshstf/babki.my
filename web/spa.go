package web

import (
	"io/fs"
	"net/http"
	"strings"
)

// assetsPrefix is where the build puts every file whose name carries a content
// hash (Vite's default assetsDir). A name there never means two different
// contents, which is what makes "cache forever" true for it.
const assetsPrefix = "assets/"

// spaHandler serves a built single-page application out of fsys.
//
// Three kinds of path, three answers:
//
//   - a file under assets/ is served to be cached for good; a name under
//     assets/ with NO file behind it is a 404. It used to get index.html with a
//     200: after an upgrade, a tab still open on the old build asked for an old
//     chunk, was handed HTML where it expected a script, and died with a blank
//     screen. A 404 is something the application can catch and act on.
//   - any other file (the icon, the manifest) is served as it is, revalidated.
//   - everything else is a route of the application and gets index.html,
//     revalidated on every load so that an upgrade reaches the browser.
//
// A directory is never listed: it is not a file, so it falls to one of the
// other two answers.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		isFile := false
		if p != "" && fs.ValidPath(p) {
			if info, err := fs.Stat(fsys, p); err == nil && !info.IsDir() {
				isFile = true
			}
		}
		switch {
		case strings.HasPrefix(p, assetsPrefix) && isFile:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
		case strings.HasPrefix(p, assetsPrefix) || p == strings.TrimSuffix(assetsPrefix, "/"):
			http.NotFound(w, r)
		case isFile:
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		default:
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, fsys, "index.html")
		}
	})
}
