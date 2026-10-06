package portfolio

import (
	"errors"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler is the portfolio's HTTP door: it reads requests, asks the Service
// and writes answers.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	srv.Mount("GET /api/v1/accounts/{accountId}/positions", view(h.handleList))
	srv.Mount("GET /api/v1/accounts/{accountId}/return", view(h.handleReturn))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/holdings", view(h.handleHoldings))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/prices", view(h.handlePrices))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/return", view(h.handlePaperReturn))
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("POST /api/v1/instruments/{instrumentId}/prices", edit(h.handleStatePrice))
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// handleList replays the account's journal into positions, closed ones
// included, sorted by instrument name. Like the journal endpoint, an unknown
// account id yields an empty list rather than a 404; data never crosses a
// space.
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	resp, _, err := h.svc.Positions(r.Context(), p.SpaceID, accountID)
	var notComputed journalDoesNotCompute
	switch {
	case errors.As(err, &notComputed):
		// Practically unreachable: every write replays the journal through this
		// engine first.
		httpjson.Error(w, http.StatusUnprocessableEntity, notComputed.Error())
	case errors.Is(err, errInstrumentNotInCatalog):
		httpjson.Error(w, http.StatusNotFound, "not found")
	case err != nil:
		family.WriteError(w, err)
	default:
		httpjson.Write(w, http.StatusOK, resp)
	}
}
