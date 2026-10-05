// Package httpserver provides routing, middleware, the health check and
// graceful shutdown. Domain modules mount their routes on it.
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
	// Unmatched API paths get a JSON 404 rather than the SPA mounted at "/".
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpjson.Error(w, http.StatusNotFound, "not found")
	})
	return s
}

// Mount registers a handler for pattern.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
	s.routes = append(s.routes, pattern)
}

// Routes returns a copy of the patterns registered with Mount ("POST
// /api/v1/..."), in order, so tests can cover every route a module mounts.
func (s *Server) Routes() []string {
	return slices.Clone(s.routes)
}

// Handler returns the root handler with all middleware. Logging wraps
// recovery, so a recovered panic is still logged as a request.
func (s *Server) Handler() http.Handler {
	return withRequestLog(s.log, withRecover(s.log, withSecurityHeaders(withSameOrigin(s.mux))))
}

// Run blocks until ctx is cancelled, then performs graceful shutdown (10s).
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// Generous for a megabyte of JSON, but a slow client cannot hold a
		// connection forever. WriteTimeout covers the slowest handler: a
		// corporate action applied to every journal.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 2 * time.Minute,
		IdleTimeout:  2 * time.Minute,
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
