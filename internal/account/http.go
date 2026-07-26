package account

import (
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Handler exposes the account module over HTTP.
type Handler struct {
	store *Store
	auth  *family.Auth
	sm    *scs.SessionManager
}

func NewHandler(store *Store, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/accounts", view(h.handleList))
	srv.Mount("POST /api/v1/accounts", edit(h.handleCreate))
	srv.Mount("PATCH /api/v1/accounts/{accountId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/accounts/{accountId}", edit(h.handleArchive))
	srv.Mount("PUT /api/v1/accounts/{accountId}/balance", edit(h.handleSetBalance))
}

func toAPI(a WithBalance) apitypes.AccountWithBalance {
	var ownerID *uuid.UUID
	if a.OwnerUserID != nil {
		ownerID = a.OwnerUserID
	}
	out := apitypes.AccountWithBalance{
		Id:          a.ID,
		OwnerUserId: ownerID,
		Name:        a.Name,
		Type:        apitypes.AccountType(a.Type),
		Currency:    a.Currency,
		Institution: a.Institution,
		Status:      apitypes.AccountStatus(a.Status),
		CreatedAt:   a.CreatedAt,
	}
	if a.Balance != nil {
		out.Balance = &apitypes.BalancePoint{
			AsOf:        a.Balance.AsOf.Format("2006-01-02"),
			AmountMinor: a.Balance.AmountMinor,
		}
	}
	return out
}

func parseAsOf(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("as_of must be YYYY-MM-DD")
	}
	if d.After(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)) {
		return time.Time{}, fmt.Errorf("as_of must not be in the future")
	}
	return d, nil
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accounts, err := h.store.ListWithBalance(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.AccountWithBalance, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, toAPI(a))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateAccountRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if req.Name == "" || !Type(req.Type).Valid() || !currencyRe.MatchString(req.Currency) {
		httpjson.Error(w, http.StatusBadRequest,
			"name is required, type must be valid, currency must be ISO-4217 uppercase")
		return
	}
	institution := ""
	if req.Institution != nil {
		institution = *req.Institution
	}
	var ownerID *uuid.UUID
	if req.OwnerUserId != nil {
		ownerID = req.OwnerUserId
	}
	a, err := h.store.Create(r.Context(), p.SpaceID, ownerID,
		req.Name, Type(req.Type), req.Currency, institution)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(WithBalance{Account: a}))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateAccountRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	upd := Update{Name: req.Name, Institution: req.Institution}
	if req.Status != nil {
		st := Status(*req.Status)
		if st != StatusActive && st != StatusArchived {
			httpjson.Error(w, http.StatusBadRequest, "invalid status")
			return
		}
		upd.Status = &st
	}
	if req.OwnerUserId != nil {
		upd.OwnerUserID = &req.OwnerUserId
	}
	if req.Name != nil && *req.Name == "" {
		httpjson.Error(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	a, err := h.store.Update(r.Context(), p.SpaceID, id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(a))
}

func (h *Handler) handleArchive(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	if err := h.store.Archive(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleSetBalance(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.SetBalanceRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	asOf, err := parseAsOf(req.AsOf)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.SetBalance(r.Context(), p.SpaceID, id, asOf, req.AmountMinor); err != nil {
		family.WriteError(w, err)
		return
	}
	a, err := h.store.ByID(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(a))
}
