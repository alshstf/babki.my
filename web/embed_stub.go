//go:build !embedui

// Package web serves embedded SPA frontend (built with embedui tag)
// or stub 503 (normal dev build: frontend goes through vite dev).
package web

import "net/http"

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"ui not built","hint":"use 'make ui' and build with -tags embedui, or run vite dev server"}`))
	})
}
