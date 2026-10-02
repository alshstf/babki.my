package jobs

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// StatusHandler serves GET /api/v1/data-sources.
type StatusHandler struct {
	pool *pgxpool.Pool
	auth *family.Auth
	sm   *scs.SessionManager
	now  func() time.Time
}

func NewStatusHandler(pool *pgxpool.Pool, auth *family.Auth, sm *scs.SessionManager) *StatusHandler {
	return &StatusHandler{pool: pool, auth: auth, sm: sm, now: time.Now}
}

func (h *StatusHandler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/data-sources", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleList)))))
}

func (h *StatusHandler) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := Sources(r.Context(), h.pool)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	now := h.now()
	out := make([]apitypes.DataSource, 0, len(list))
	for _, s := range list {
		d := apitypes.DataSource{
			Kind: s.Kind, EverySeconds: int(s.Every / time.Second), LastError: s.LastError, Stale: s.Stale(now),
			LastSuccessAt: nullable.NewNullNullable[time.Time](), LastFailureAt: nullable.NewNullNullable[time.Time](),
		}
		if s.LastSuccessAt != nil {
			d.LastSuccessAt = nullable.NewNullableWithValue(s.LastSuccessAt.UTC())
		}
		if s.LastFailureAt != nil {
			d.LastFailureAt = nullable.NewNullableWithValue(s.LastFailureAt.UTC())
		}
		out = append(out, d)
	}
	httpjson.Write(w, http.StatusOK, out)
}
