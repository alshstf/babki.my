package budget

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves the budget and its limits.
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
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/budget", view(h.handleMonth))
	srv.Mount("GET /api/v1/budget/limits", view(h.handleLimits))
	srv.Mount("PUT /api/v1/budget/limits", edit(h.handleSet))
	srv.Mount("DELETE /api/v1/budget/limits/{categoryId}/{fromMonth}", edit(h.handleDelete))
}

const monthLayout = "2006-01"

func monthText(m time.Time) string { return m.Format(monthLayout) }

// LimitAPI is a limit as the API and the export write it.
func LimitAPI(l Limit) apitypes.BudgetLimit {
	return apitypes.BudgetLimit{CategoryId: l.CategoryID, FromMonth: monthText(l.From), AmountMinor: l.Amount, Rollover: l.Rollover}
}

func (h *Handler) handleMonth(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	m, err := time.Parse(monthLayout, r.URL.Query().Get("month"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "month must be YYYY-MM")
		return
	}
	b, err := h.svc.Month(r.Context(), p.SpaceID, m)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.Budget{
		Month: monthText(b.Month), BaseCurrency: b.BaseCurrency, Lines: make([]apitypes.BudgetLine, 0, len(b.Lines)),
		PlannedMinor: b.Planned, SpentMinor: b.Spent, LeftMinor: b.Left, UnlimitedMinor: b.Unlimited,
		MissingRates: b.MissingRates,
	}
	if out.MissingRates == nil {
		out.MissingRates = []string{}
	}
	for _, l := range b.Lines {
		out.Lines = append(out.Lines, apitypes.BudgetLine{
			CategoryId: l.CategoryID, LimitMinor: l.Limit, Rollover: l.Rollover, Since: monthText(l.Since),
			CarriedMinor: l.Carried, SpentMinor: l.Spent, LeftMinor: l.Left,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleLimits(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	list, err := h.svc.Limits(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.BudgetLimit, 0, len(list))
	for _, l := range list {
		out = append(out, LimitAPI(l))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleSet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.BudgetLimit
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	from, err := time.Parse(monthLayout, req.FromMonth)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "from_month must be YYYY-MM")
		return
	}
	l := Limit{CategoryID: req.CategoryId, From: from, Amount: req.AmountMinor, Rollover: req.Rollover}
	if err := h.svc.SetLimit(r.Context(), p.SpaceID, l); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("categoryId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "categoryId must be a uuid")
		return
	}
	from, err := time.Parse(monthLayout, r.PathValue("fromMonth"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "fromMonth must be YYYY-MM")
		return
	}
	if err := h.svc.DeleteLimit(r.Context(), p.SpaceID, id, from); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
