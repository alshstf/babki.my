package family

import (
	"errors"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler exposes the family module over HTTP.
type Handler struct {
	svc   *Service
	store *Store
	auth  *Auth
	sm    *scs.SessionManager
}

func NewHandler(svc *Service, store *Store, auth *Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, store: store, auth: auth, sm: sm}
}

// WriteError maps domain errors to HTTP responses. Shared by other modules.
func WriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		httpjson.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		httpjson.Error(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrForbidden):
		httpjson.Error(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrAlreadySetUp):
		httpjson.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		httpjson.Error(w, http.StatusNotFound, "not found")
	default:
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// Mount registers all family routes on the server.
func (h *Handler) Mount(srv *httpserver.Server) {
	wrap := func(fn http.HandlerFunc) http.Handler { return h.sm.LoadAndSave(fn) }
	srv.Mount("GET /api/v1/setup/status", wrap(h.handleSetupStatus))
	srv.Mount("POST /api/v1/setup", wrap(h.handleSetup))
	srv.Mount("POST /api/v1/auth/login", wrap(h.handleLogin))
	srv.Mount("POST /api/v1/auth/logout", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleLogout))))
	srv.Mount("GET /api/v1/auth/me", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleMe))))
}

func (h *Handler) sessionInfo(r *http.Request, u User, p Principal) (apitypes.SessionInfo, error) {
	sp, err := h.store.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		return apitypes.SessionInfo{}, err
	}
	return apitypes.SessionInfo{
		User:      apitypes.UserInfo{Id: u.ID, Username: u.Username, DisplayName: u.DisplayName},
		Role:      apitypes.Role(p.Role),
		SpaceId:   sp.ID,
		SpaceName: sp.Name,
	}, nil
}

func (h *Handler) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needed, err := h.svc.SetupNeeded(r.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.SetupStatus{SetupNeeded: needed})
}

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	u, p, err := h.svc.Setup(r.Context(), SetupParams{
		SpaceName: req.SpaceName, Username: req.Username,
		DisplayName: req.DisplayName, Password: req.Password,
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := h.auth.SignIn(r.Context(), u.ID); err != nil {
		WriteError(w, err)
		return
	}
	info, err := h.sessionInfo(r, u, p)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, info)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req apitypes.LoginRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	u, p, err := h.svc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := h.auth.SignIn(r.Context(), u.ID); err != nil {
		WriteError(w, err)
		return
	}
	info, err := h.sessionInfo(r, u, p)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, info)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.SignOut(r.Context()); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	u, err := h.store.UserByID(r.Context(), p.UserID)
	if err != nil {
		WriteError(w, err)
		return
	}
	info, err := h.sessionInfo(r, u, p)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, info)
}
