package account

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// spaceStore is the subset of family.Store this handler needs: reading the
// space's base currency for GET /summary. Local interface (mirroring
// portfolio's journalStore/quoteStore) so tests can inject a fake or a real
// *family.Store interchangeably — Go interface assignability is structural,
// so *family.Store satisfies this with no conversion needed at the call
// site.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Handler exposes the account module over HTTP.
type Handler struct {
	store     *Store
	spaces    spaceStore
	converter *marketdata.Converter
	auth      *family.Auth
	sm        *scs.SessionManager
}

func NewHandler(store *Store, spaces spaceStore, converter *marketdata.Converter, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, spaces: spaces, converter: converter, auth: auth, sm: sm}
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
	srv.Mount("GET /api/v1/summary", view(h.handleSummary))
}

func toAPI(a WithBalance) apitypes.AccountWithBalance {
	var ownerID nullable.Nullable[uuid.UUID]
	if a.OwnerUserID != nil {
		ownerID = nullable.NewNullableWithValue(*a.OwnerUserID)
	} else {
		// Explicit null (not omitted) so clients can tell "shared account" apart
		// from a field the server simply didn't return.
		ownerID = nullable.NewNullNullable[uuid.UUID]()
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
	// as_of is a date-only field with no timezone attached. A day of slack
	// past the UTC "today" boundary is intentional: a user anywhere from
	// UTC+3 to UTC+12 must be able to record "today" as of their own local
	// date, and their tomorrow-in-UTC is still today somewhere on Earth.
	// Anything further out than that is rejected as a genuine future date.
	maxAllowedUTC := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	if d.After(maxAllowedUTC) {
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
	if req.OwnerUserId.IsSpecified() && !req.OwnerUserId.IsNull() {
		v := req.OwnerUserId.MustGet()
		ownerID = &v
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
	if req.OwnerUserId.IsSpecified() {
		if req.OwnerUserId.IsNull() {
			var cleared *uuid.UUID
			upd.OwnerUserID = &cleared
		} else {
			v := req.OwnerUserId.MustGet()
			ptr := &v
			upd.OwnerUserID = &ptr
		}
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

// handleSummary totals the space's accounts by currency, then converts and
// sums those per-currency totals into the space's base currency.
//
// The totals come exclusively from manually entered balances
// (account_balances, see Store.SummaryByCurrency). Positions computed by the
// portfolio engine from the operations journal are deliberately NOT added
// here: a brokerage account's securities are already reflected in the balance
// the user records for it, so counting positions on top would double count.
// Valuing brokerage accounts as "positions + cash" — and folding that
// valuation into this summary — is planned for plan 4b; until then the two
// views stay separate, and total_in_base_minor below is a sum of manual
// balances only, not full net worth including live market values.
//
// total_in_base_minor converts each currency's net_minor into base_currency
// at today's fx rate (ConvertMany, on time.Now().UTC()) and sums the
// results. A currency with no resolvable rate is dropped into unconverted
// rather than failing the request — a partial total beats an error page.
// total_in_base_minor is null only when there's at least one currency and
// none of them converted (unconverted covers every currency in totals); an
// empty space, or a space whose accounts are already all in base_currency,
// yields 0, not null.
func (h *Handler) handleSummary(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	totals, err := h.store.SummaryByCurrency(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	out := apitypes.Summary{
		Totals:       make([]apitypes.CurrencyTotal, 0, len(totals)),
		BaseCurrency: sp.BaseCurrency,
	}
	netByCurrency := make(map[string]int64, len(totals))
	for _, t := range totals {
		out.Totals = append(out.Totals, apitypes.CurrencyTotal{
			Currency: t.Currency, AssetsMinor: t.AssetsMinor,
			LiabilitiesMinor: t.LiabilitiesMinor, NetMinor: t.NetMinor,
		})
		netByCurrency[t.Currency] = t.NetMinor
	}

	// Filter out zero-amount currencies before conversion: zero minor units
	// convert to zero at any rate, so there's no need for an fx rate at all.
	// This prevents zero-amount currencies from incorrectly appearing in the
	// unconverted list when their rate is unavailable. The filtering preserves
	// the correct semantics: an empty space or all-zero balances yields
	// total_in_base_minor=0 (not null), while a space with only non-zero
	// currencies that all lack rates yields null.
	for currency, amount := range netByCurrency {
		if amount == 0 {
			delete(netByCurrency, currency)
		}
	}

	converted, missing, err := h.converter.ConvertMany(r.Context(), netByCurrency, sp.BaseCurrency, time.Now().UTC())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if missing == nil {
		missing = []string{} // unconverted must serialize as [], never null
	}
	out.Unconverted = missing

	if len(netByCurrency) > 0 && len(missing) == len(netByCurrency) {
		out.TotalInBaseMinor = nullable.NewNullNullable[int64]()
	} else {
		out.TotalInBaseMinor = nullable.NewNullableWithValue(converted)
	}

	httpjson.Write(w, http.StatusOK, out)
}
