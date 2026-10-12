package allocation

import (
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/structure"
)

// Handler serves /api/v1/allocation.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/allocation", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleView)))))
	srv.Mount("PUT /api/v1/allocation/targets", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleEditor, http.HandlerFunc(h.handleTargets)))))
	srv.Mount("PUT /api/v1/allocation/mode", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleEditor, http.HandlerFunc(h.handleMode)))))
}

func (h *Handler) handleView(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	h.writeView(w, r, p.SpaceID)
}

func (h *Handler) handleTargets(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.AllocationTargets
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	targets := make([]Target, 0, len(req.Targets))
	for _, t := range req.Targets {
		pct, err := decimal.NewFromString(t.Percent)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "percent must be a decimal")
			return
		}
		targets = append(targets, Target{Key: t.Key, Percent: pct})
	}
	if err := h.svc.SetTargets(r.Context(), p.SpaceID, Dimension(req.Dimension), targets); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeView(w, r, p.SpaceID)
}

func (h *Handler) handleMode(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.SetAllocationModeJSONBody
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.svc.SetMode(r.Context(), p.SpaceID, Mode(req.Mode)); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeView(w, r, p.SpaceID)
}

// writeView answers with the worth against the targets, papers at the
// valuation asked (?valuation=liquid|full, liquid when absent).
func (h *Handler) writeView(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID) {
	valuation := structure.Liquid
	if raw := r.URL.Query().Get("valuation"); raw != "" {
		valuation = structure.Valuation(raw)
	}
	v, err := h.svc.View(r.Context(), spaceID, valuation)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ViewAPI(v))
}

// TargetsAPI are targets as the API writes them.
func TargetsAPI(in []Target) []apitypes.AllocationTarget {
	out := make([]apitypes.AllocationTarget, 0, len(in))
	for _, t := range in {
		out = append(out, apitypes.AllocationTarget{Key: t.Key, Percent: t.Percent.StringFixed(2)})
	}
	return out
}

// ViewAPI is the worth against the targets as the API writes it.
func ViewAPI(v View) apitypes.Allocation {
	rowsAPI := func(in []Row) []apitypes.AllocationRow {
		out := make([]apitypes.AllocationRow, 0, len(in))
		for _, r := range in {
			row := apitypes.AllocationRow{
				Key: r.Key, Minor: r.Minor, Share: r.Share.StringFixed(2),
				Target: nullable.NewNullNullable[string](), Gap: nullable.NewNullNullable[string](), OffMinor: nullable.NewNullNullable[int64](),
			}
			if r.Target != nil {
				row.Target = nullable.NewNullableWithValue(r.Target.StringFixed(2))
				row.Gap = nullable.NewNullableWithValue(r.Gap.StringFixed(2))
				row.OffMinor = nullable.NewNullableWithValue(*r.Off)
			}
			out = append(out, row)
		}
		return out
	}
	missing := v.MissingRates
	if missing == nil {
		missing = []string{}
	}
	return apitypes.Allocation{
		BaseCurrency: v.BaseCurrency, Mode: apitypes.AllocationMode(v.Mode), WorthMinor: v.Worth,
		EverydayMinor: v.Everyday, UnsplitMinor: v.Unsplit, Kinds: rowsAPI(v.Kinds), Currencies: rowsAPI(v.Currencies),
		Unpriced: v.Unpriced, MissingRates: missing,
	}
}
