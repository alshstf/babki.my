package family

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

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
	guard *loginGuard
	// setupCode, when set, must be given to first-run setup: it is written to the
	// server log, so the machine's owner, not whoever reaches the port first,
	// becomes the instance's owner.
	setupCode string
}

// WithSetupCode makes first-run setup ask for code (see setupCode).
func (h *Handler) WithSetupCode(code string) *Handler {
	h.setupCode = code
	return h
}

func NewHandler(svc *Service, store *Store, auth *Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, store: store, auth: auth, sm: sm, guard: newLoginGuard()}
}

// WriteError maps domain errors to HTTP responses; other modules use it too.
// Anything else is a 500 "internal error", and its text is logged through
// slog.Default (configured by cmd/babki), since the request log alone would not
// say which row broke.
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
		slog.Default().Error("request failed", "err", err.Error())
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
	srv.Mount("POST /api/v1/auth/password", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleChangePassword))))
	srv.Mount("POST /api/v1/auth/sign-out-elsewhere", h.sm.LoadAndSave(h.auth.RequireAuth(http.HandlerFunc(h.handleSignOutElsewhere))))
	srv.Mount("GET /api/v1/tax-residencies", authed(h.handleListTaxResidencies))
	srv.Mount("PATCH /api/v1/space", authed(h.handleUpdateSpace))
	srv.Mount("GET /api/v1/members", authed(h.handleListMembers))
	srv.Mount("POST /api/v1/members", authed(h.handleCreateMember))
	srv.Mount("PATCH /api/v1/members/{userId}", authed(h.handleUpdateMember))
	srv.Mount("DELETE /api/v1/members/{userId}", authed(h.handleDeleteMember))
}

// CostBasisRulesAPI renders a country's rules for the wire; the positions
// response uses it too, so both payloads agree. Notices is never null.
func CostBasisRulesAPI(r TaxRules) apitypes.CostBasisRules {
	out := apitypes.CostBasisRules{
		Country:   r.Country,
		Method:    apitypes.CostBasisMethod(r.Method),
		Perimeter: apitypes.CostBasisPerimeter(r.Perimeter),
		Supported: r.Supported(),
		Notices:   make([]apitypes.CostBasisNotice, 0, len(r.Notices())),
	}
	for _, n := range r.Notices() {
		out.Notices = append(out.Notices, apitypes.CostBasisNotice(n))
	}
	return out
}

func toSessionInfo(u User, p Principal, sp Space) apitypes.SessionInfo {
	return apitypes.SessionInfo{
		User:           apitypes.UserInfo{Id: u.ID, Username: u.Username, DisplayName: u.DisplayName},
		Role:           apitypes.Role(p.Role),
		SpaceId:        sp.ID,
		SpaceName:      sp.Name,
		BaseCurrency:   sp.BaseCurrency,
		TaxResidency:   sp.TaxResidency,
		FullValuation:  apitypes.FullValuation(sp.FullValuation),
		CostBasisRules: CostBasisRulesAPI(sp.CostBasisRules()),
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
	httpjson.Write(w, http.StatusOK, apitypes.SetupStatus{SetupNeeded: needed, CodeRequired: h.setupCode != ""})
}

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if h.setupCode != "" {
		given := ""
		if req.SetupCode != nil {
			given = strings.TrimSpace(*req.SetupCode)
		}
		if subtle.ConstantTimeCompare([]byte(strings.ToUpper(given)), []byte(h.setupCode)) != 1 {
			httpjson.Error(w, http.StatusForbidden, "the setup code from the server's log is required")
			return
		}
	}
	u, p, err := h.svc.Setup(r.Context(), SetupParams{
		SpaceName: req.SpaceName, Username: req.Username,
		DisplayName: req.DisplayName, Password: req.Password,
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	h.signInAndAnswer(w, r, u, p, http.StatusCreated)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req apitypes.LoginRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	// Checked before any lookup or hash: a refused attempt costs nothing and says
	// nothing about the username.
	addr := clientAddr(r)
	if wait := h.guard.wait(addr, req.Username); wait > 0 {
		// Whole seconds, rounded up: "0" would invite the retry it refuses.
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		httpjson.Error(w, http.StatusTooManyRequests, "too many sign-in attempts, try again later")
		return
	}
	u, p, err := h.svc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		// Only a credential refusal counts; a database outage locks nobody out.
		if errors.Is(err, ErrInvalidCredentials) {
			h.guard.failed(addr, req.Username)
		}
		WriteError(w, err)
		return
	}
	h.guard.succeeded(addr, req.Username)
	h.signInAndAnswer(w, r, u, p, http.StatusOK)
}

// signInAndAnswer starts a session and describes it, for setup (201) and
// login (200). If SignIn fails after setup created the owner, the answer is a
// 500 and the client recovers by logging in; the user is not deleted.
func (h *Handler) signInAndAnswer(w http.ResponseWriter, r *http.Request, u User, p Principal, status int) {
	if err := h.auth.SignIn(r.Context(), u.ID); err != nil {
		WriteError(w, err)
		return
	}
	info, err := h.sessionInfo(r, u, p)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, status, info)
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

// handleListTaxResidencies publishes the countries with cost basis rules, so
// clients offer exactly what the server accepts.
func (h *Handler) handleListTaxResidencies(w http.ResponseWriter, r *http.Request) {
	all := TaxResidencies()
	out := make([]apitypes.CostBasisRules, 0, len(all))
	for _, rules := range all {
		out = append(out, CostBasisRulesAPI(rules))
	}
	httpjson.Write(w, http.StatusOK, out)
}

// handleUpdateSpace changes the space's settings; checks live in
// Service.UpdateSpace.
func (h *Handler) handleUpdateSpace(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req apitypes.UpdateSpaceRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	sp, err := h.svc.UpdateSpace(r.Context(), p, SpaceSettings{
		BaseCurrency: req.BaseCurrency, TaxResidency: req.TaxResidency, FullValuation: (*string)(req.FullValuation),
	})
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

// handleChangePassword replaces the caller's password. A wrong current password
// counts against the sign-in lock and is a 400: the session is fine, the field
// is not. The caller's session is renewed; every other one ends.
func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req apitypes.ChangePasswordRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	u, err := h.store.UserByID(r.Context(), p.UserID)
	if err != nil {
		WriteError(w, err)
		return
	}
	addr := clientAddr(r)
	if wait := h.guard.wait(addr, u.Username); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		httpjson.Error(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	at, err := h.svc.ChangePassword(r.Context(), p.UserID, req.CurrentPassword, req.NewPassword)
	if errors.Is(err, ErrInvalidCredentials) {
		h.guard.failed(addr, u.Username)
		httpjson.Error(w, http.StatusBadRequest, "the current password is wrong")
		return
	}
	if err != nil {
		WriteError(w, err)
		return
	}
	h.guard.succeeded(addr, u.Username)
	if err := h.auth.SignInAt(r.Context(), p.UserID, at); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSignOutElsewhere ends every session of the caller but this one.
func (h *Handler) handleSignOutElsewhere(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	at, err := h.svc.SignOutElsewhere(r.Context(), p.UserID)
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := h.auth.SignInAt(r.Context(), p.UserID, at); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setupCodeAlphabet omits look-alikes (0/O, 1/I/L): the code is read from a log.
const setupCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewSetupCode returns a fresh eight-character setup code, about 39 bits.
func NewSetupCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = setupCodeAlphabet[int(v)%len(setupCodeAlphabet)]
	}
	return string(out)
}
