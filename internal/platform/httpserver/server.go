// Package httpserver is the HTTP framework of the application: routing, middleware,
// healthcheck, graceful shutdown. Domain modules mount their routes here.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/version"
)

type Server struct {
	log    *slog.Logger
	pool   *pgxpool.Pool
	mux    *http.ServeMux
	routes []string
}

func New(log *slog.Logger, pool *pgxpool.Pool) *Server {
	s := &Server{log: log, pool: pool, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	// Catch-all for unmatched API paths: JSON 404 instead of falling through
	// to the SPA handler mounted at "/".
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpjson.Error(w, http.StatusNotFound, "not found")
	})
	return s
}

// Mount adds a handler (domain modules, UI).
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
	s.routes = append(s.routes, pattern)
}

// Routes lists the patterns Mount has registered, in registration order.
//
// THE METHOD IS PART OF A PATTERN ("POST /api/v1/..."), because that is how
// net/http spells one and two methods on one path are two routes. The routes
// New registers on itself — the healthcheck and the JSON 404 for unmatched
// /api/ paths — are not here: they go to the mux directly and belong to the
// framework rather than to a module.
//
// It exists for the tests that have to cover EVERY route of a module: a list of
// routes typed into a test goes on looking complete the day a route is mounted
// without a line added there, and the test then passes on the routes it does
// know while the new one is asked nothing at all. Taking the list from the
// router turns that into a failure (see
// TestEveryEndpointRefusesAnEditorAndAViewer in internal/importer/tinvest).
//
// The slice is a copy, so a caller appending to what it gets back cannot teach
// the router a route.
func (s *Server) Routes() []string {
	return slices.Clone(s.routes)
}

// Handler returns the root handler with all middleware.
//
// Order matters: withRequestLog wraps withRecover so that panics recovered
// deeper in the chain still produce an access log entry (status, duration).
func (s *Server) Handler() http.Handler {
	return withRequestLog(s.log, withRecover(s.log, s.mux))
}

// Run blocks until ctx is cancelled, then performs graceful shutdown (10s).
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, code := "ok", http.StatusOK
	if err := s.pool.Ping(ctx); err != nil {
		status, code = "degraded", http.StatusServiceUnavailable
		s.log.Warn("healthz: db ping failed", "err", err)
	}
	httpjson.Write(w, code, map[string]string{
		"status":  status,
		"version": version.Version,
	})
}
