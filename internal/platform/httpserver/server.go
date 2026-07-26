// Package httpserver is the HTTP framework of the application: routing, middleware,
// healthcheck, graceful shutdown. Domain modules mount their routes here.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/version"
)

type Server struct {
	log  *slog.Logger
	pool *pgxpool.Pool
	mux  *http.ServeMux
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
