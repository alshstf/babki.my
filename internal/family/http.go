package family

import (
	"errors"
	"log/slog"
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
//
// An error that matches none of the cases below is not a domain outcome at
// all: a DB failure, a canceled context, a figure too large to state in minor
// units (money.ErrOverflow). The client is told "internal error" and nothing
// more — deliberately, since the text of those errors is server internals —
// and that leaves the owner with a blank screen and no way to learn WHICH row
// broke. So the default branch logs the error's own text, which is where every
// context string the callers build ends up ("balance of account <uuid> in
// RUB", "%d terms totalling %s %s"). Without this the request log records only
// method, path, status and duration (see httpserver's withRequestLog), and the
// diagnosis is unreachable even with server access.
//
// It logs through slog.Default() because WriteError takes no logger and is
// called from forty-odd places across five packages, none of which could hand
// it one without threading a logger through every one of them. cmd/babki
// installs the configured logger as the default at startup (see setup in
// runtime.go), so this line lands in the same stream, level and format as
// every other rather than in a second, differently shaped one.
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
	srv.Mount("GET /api/v1/tax-residencies", authed(h.handleListTaxResidencies))
	srv.Mount("PATCH /api/v1/space", authed(h.handleUpdateSpace))
	srv.Mount("GET /api/v1/members", authed(h.handleListMembers))
	srv.Mount("POST /api/v1/members", authed(h.handleCreateMember))
	srv.Mount("PATCH /api/v1/members/{userId}", authed(h.handleUpdateMember))
	srv.Mount("DELETE /api/v1/members/{userId}", authed(h.handleDeleteMember))
}

// CostBasisRulesAPI renders one country's cost basis rules for the wire. It is
// exported because the positions response carries the same object (see
// portfolio.Handler.handleList): the figures and the statement of whose rules
// they follow must be built from one mapping, or the two payloads could
// eventually disagree about the same country.
//
// Notices never becomes null on the wire — TaxRules.Notices always returns a
// slice — so a reader can tell "nothing is wrong" from "the field is missing".
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
	h.signInAndAnswer(w, r, u, p, http.StatusCreated)
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
	h.signInAndAnswer(w, r, u, p, http.StatusOK)
}

// signInAndAnswer is the tail both doors that hand out a session share: start
// the session, then answer with what the client needs to know about it. It is
// one function because the two were the same eleven lines twice, and a session
// established by one route but described differently by the other is the kind
// of difference nobody would look for.
//
// The status differs and nothing else does: setup CREATED something, login did
// not.
//
// A failure of SignIn after the account already exists leaves a created owner
// with no cookie, and the answer is a 500 the client recovers from by logging
// in — deliberately, since the alternative is deleting a real user because a
// session store hiccuped. Setup's own writes are already atomic (see
// Store.CreateSpaceWithOwner); what is not, and cannot be, is a session that
// lives outside that transaction.
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

// handleListTaxResidencies publishes the countries this application has cost
// basis rules for, so a client offers exactly what the server will accept
// rather than a list of its own that drifts from it.
func (h *Handler) handleListTaxResidencies(w http.ResponseWriter, r *http.Request) {
	all := TaxResidencies()
	out := make([]apitypes.CostBasisRules, 0, len(all))
	for _, rules := range all {
		out = append(out, CostBasisRulesAPI(rules))
	}
	httpjson.Write(w, http.StatusOK, out)
}

// handleUpdateSpace changes the space's base currency and/or the owner's
// country of tax residency (owner-only; role, format and known-country checks
// live in Service.UpdateSpace, matching how the members routes below delegate
// their owner-only checks to Service).
func (h *Handler) handleUpdateSpace(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req apitypes.UpdateSpaceRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	sp, err := h.svc.UpdateSpace(r.Context(), p, SpaceSettings{
		BaseCurrency: req.BaseCurrency, TaxResidency: req.TaxResidency,
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
