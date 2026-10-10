package recurring

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves the regular payments and hides or shows one.
type Handler struct {
	svc    *Service
	hidden *Hidden
	auth   *family.Auth
	sm     *scs.SessionManager
}

func NewHandler(svc *Service, hidden *Hidden, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, hidden: hidden, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/recurring", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleList)))))
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("PUT /api/v1/recurring/hidden", edit(h.handleHide))
	srv.Mount("DELETE /api/v1/recurring/hidden", edit(h.handleShow))
}

func (h *Handler) handleHide(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.RecurringKey
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.hidden.Hide(r.Context(), p.SpaceID, KeyOf(req.Name, req.Incoming, req.Currency)); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleShow(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.RecurringKey
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	found, err := h.hidden.Show(r.Context(), p.SpaceID, KeyOf(req.Name, req.Incoming, req.Currency))
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if !found {
		httpjson.Error(w, http.StatusNotFound, "no such hidden payment")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	found, err := h.svc.Find(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.RecurringPayment, 0, len(found))
	for _, f := range found {
		item := apitypes.RecurringPayment{
			Name: f.Name, Cadence: apitypes.RecurringPaymentCadence(f.Cadence), AmountMinor: f.Amount,
			Currency: f.Currency, AccountId: f.AccountID, CategoryId: nullable.NewNullNullable[uuid.UUID](),
			LastOn: f.Last.Format(time.DateOnly), NextOn: f.Next.Format(time.DateOnly), Count: f.Count, Overdue: f.Overdue,
			Hidden: f.Hidden,
		}
		if f.CategoryID != nil {
			item.CategoryId = nullable.NewNullableWithValue(*f.CategoryID)
		}
		out = append(out, item)
	}
	httpjson.Write(w, http.StatusOK, out)
}
