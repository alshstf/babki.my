package goals

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves the savings goals.
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
	srv.Mount("GET /api/v1/goals", view(h.handleList))
	srv.Mount("POST /api/v1/goals", edit(h.handleCreate))
	srv.Mount("PUT /api/v1/goals/{goalId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/goals/{goalId}", edit(h.handleDelete))
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	h.write(w, r, p.SpaceID)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	h.save(w, r, uuid.Nil)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("goalId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "goalId must be a uuid")
		return
	}
	h.save(w, r, id)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("goalId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "goalId must be a uuid")
		return
	}
	if err := h.svc.Delete(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	h.write(w, r, p.SpaceID)
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.GoalInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	yield, err := decimal.NewFromString(req.YieldPercent)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "yield_percent must be a decimal")
		return
	}
	g := Goal{ID: id, Name: req.Name, Yield: yield, Target: get(req.TargetMinor), Monthly: get(req.MonthlyMinor)}
	if on := get(req.TargetOn); on != nil {
		d, err := time.Parse(time.DateOnly, *on)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "target_on must be YYYY-MM-DD")
			return
		}
		g.TargetOn = &d
	}
	for _, f := range req.Funds {
		g.Funds = append(g.Funds, Fund{Account: f.AccountId, Minor: get(f.AmountMinor)})
	}
	if _, err := h.svc.Save(r.Context(), p.SpaceID, g); err != nil {
		family.WriteError(w, err)
		return
	}
	h.write(w, r, p.SpaceID)
}

func get[T any](v nullable.Nullable[T]) *T {
	if !v.IsSpecified() || v.IsNull() {
		return nil
	}
	x := v.MustGet()
	return &x
}

func of[T any](p *T) nullable.Nullable[T] {
	if p == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*p)
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID) {
	list, base, err := h.svc.List(r.Context(), spaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.Goals{BaseCurrency: base, Goals: make([]apitypes.Goal, 0, len(list))}
	for _, v := range list {
		g := apitypes.Goal{
			Id: v.ID, Name: v.Name, TargetMinor: of(v.Target), MonthlyMinor: of(v.Monthly), YieldPercent: v.Yield.StringFixed(2),
			TargetOn: nullable.NewNullNullable[string](), SavedMinor: v.Progress.Saved, Status: apitypes.GoalStatus(v.Progress.Status),
			Percent: v.Progress.Percent, ProjectedMinor: of(v.Progress.Projected), ShortMinor: of(v.Progress.Short), NeededMinor: of(v.Progress.Needed), ReachOn: nullable.NewNullNullable[string](),
			Months: v.Progress.Months, Funds: make([]apitypes.GoalFund, 0, len(v.Funds)), Overdrawn: v.Overdrawn,
		}
		if g.Overdrawn == nil {
			g.Overdrawn = []uuid.UUID{}
		}
		if v.TargetOn != nil {
			g.TargetOn = nullable.NewNullableWithValue(v.TargetOn.Format(time.DateOnly))
		}
		if v.Progress.ReachOn != nil {
			g.ReachOn = nullable.NewNullableWithValue(v.Progress.ReachOn.Format(time.DateOnly))
		}
		for _, f := range v.Funds {
			g.Funds = append(g.Funds, apitypes.GoalFund{AccountId: f.Account, AmountMinor: of(f.Minor), ValueMinor: v.FundValues[f.Account]})
		}
		out.Goals = append(out.Goals, g)
	}
	httpjson.Write(w, http.StatusOK, out)
}
