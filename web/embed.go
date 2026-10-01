//go:build embedui

package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the built application embedded at compile time; see
// spaHandler for what each path gets.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // impossible: dist is embedded at compile time
	}
	return spaHandler(sub)
}
