package family

import (
	"errors"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
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
	case errors.Is(err, ErrUsernameTaken):
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
	authed := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(fn))
	}
	srv.Mount("GET /api/v1/setup/status", wrap(h.handleSetupStatus))
	srv.Mount("POST /api/v1/setup", wrap(h.handleSetup))
	srv.Mount("POST /api/v1/auth/login", wrap(h.handleLogin))
	srv.Mount("POST /api/v1/auth/logout", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleLogout))))
	srv.Mount("GET /api/v1/auth/me", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleMe))))
	srv.Mount("PATCH /api/v1/space", authed(h.handleUpdateSpace))
	srv.Mount("GET /api/v1/members", authed(h.handleListMembers))
	srv.Mount("POST /api/v1/members", authed(h.handleCreateMember))
	srv.Mount("PATCH /api/v1/members/{userId}", authed(h.handleUpdateMember))
	srv.Mount("DELETE /api/v1/members/{userId}", authed(h.handleDeleteMember))
}

func toSessionInfo(u User, p Principal, sp Space) apitypes.SessionInfo {
	return apitypes.SessionInfo{
		User:         apitypes.UserInfo{Id: u.ID, Username: u.Username, DisplayName: u.DisplayName},
		Role:         apitypes.Role(p.Role),
		SpaceId:      sp.ID,
		SpaceName:    sp.Name,
		BaseCurrency: sp.BaseCurrency,
	}
}

func (h *Handler) sessionInfo(r *http.Request, u User, p Principal) (apitypes.SessionInfo, error) {
	sp, err := h.store.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		return apitypes.SessionInfo{}, err
	}
	return toSessionInfo(u, p, sp), nil
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

// handleUpdateSpace changes the space's base currency (owner-only; role and
// format checks live in Service.UpdateBaseCurrency, matching how the
// members routes below delegate their owner-only checks to Service).
func (h *Handler) handleUpdateSpace(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req apitypes.UpdateSpaceRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	sp, err := h.svc.UpdateBaseCurrency(r.Context(), p, req.BaseCurrency)
	if err != nil {
		WriteError(w, err)
		return
	}
	u, err := h.store.UserByID(r.Context(), p.UserID)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toSessionInfo(u, p, sp))
}

func memberInfo(m Member) apitypes.MemberInfo {
	return apitypes.MemberInfo{
		Id: m.ID, Username: m.Username, DisplayName: m.DisplayName, Role: apitypes.Role(m.Role),
	}
}

func (h *Handler) handleListMembers(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	members, err := h.store.ListMembers(r.Context(), p.SpaceID)
	if err != nil {
		WriteError(w, err)
		return
	}
	out := make([]apitypes.MemberInfo, 0, len(members))
	for _, m := range members {
		out = append(out, memberInfo(m))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreateMember(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req apitypes.CreateMemberRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	m, err := h.svc.CreateMember(r.Context(), p, req.Username, req.DisplayName, req.Password, Role(req.Role))
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, memberInfo(m))
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) handleUpdateMember(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	targetID, ok := pathUUID(w, r, "userId")
	if !ok {
		return
	}
	var req apitypes.UpdateMemberRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	m, err := h.svc.UpdateMemberRole(r.Context(), p, targetID, Role(req.Role))
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, memberInfo(m))
}

func (h *Handler) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	targetID, ok := pathUUID(w, r, "userId")
	if !ok {
		return
	}
	if err := h.svc.RemoveMember(r.Context(), p, targetID); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
