package httpserver

import (
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"babki.my/babki/internal/platform/httpjson"
)

// withRequestLog logs each request: method, path, status, duration.
func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withRecover converts handler panics into 500 and logging without crashing the process.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// The stack is the whole diagnosis: a recovered panic without it
				// says that something broke and nothing about where.
				log.Error("panic in handler", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				httpjson.Error(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer
// (Flusher/Hijacker passthrough for streaming handlers).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// contentSecurityPolicy is what the application's own page needs and nothing
// else: its scripts, styles, fonts and images come from this origin. Inline
// STYLE attributes are allowed because the component library positions its
// popovers with them; inline scripts are not, and the page has none.
const contentSecurityPolicy = "default-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// withSecurityHeaders sets the response headers that cost nothing and close a
// class of browser attack each: no MIME sniffing, no framing by another site,
// no referrer to other origins, and a content policy that keeps the page to its
// own origin. API answers are additionally never stored by a cache — they are
// one family's finances.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// withSameOrigin refuses a state-changing request that a browser sent from
// another site.
//
// The session cookie is SameSite=Lax, which already keeps it off most
// cross-site requests — but that is one browser default, and it does nothing
// for a request that needs no cookie at all: a page elsewhere can submit the
// login form and leave the reader signed in to somebody else's account. So the
// server checks for itself.
//
// A browser says where a request came from: Sec-Fetch-Site on every request it
// makes, Origin on every cross-origin write. "same-origin" (and "none", a
// request the user typed) pass; another site, or a sibling subdomain, does not.
// A request carrying neither header is not from a browser — a script, a test,
// curl — and has no ambient cookie to be tricked out of, so it passes.
func withSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			if site != "same-origin" && site != "none" {
				httpjson.Error(w, http.StatusForbidden, "cross-site request refused")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !originIsOurs(origin, r) {
			httpjson.Error(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originIsOurs reports whether an Origin header names the host this request was
// addressed to — as the server sees it, or as a reverse proxy in front of it
// says the browser saw it.
func originIsOurs(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host || u.Host == r.Header.Get("X-Forwarded-Host")
}
