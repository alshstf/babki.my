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

// Handler serves GET /api/v1/recurring.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/recurring", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleList)))))
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
		}
		if f.CategoryID != nil {
			item.CategoryId = nullable.NewNullableWithValue(*f.CategoryID)
		}
		out = append(out, item)
	}
	httpjson.Write(w, http.StatusOK, out)
}
