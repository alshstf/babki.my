package allocation

import (
	"net/http"
	"strconv"

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
	srv.Mount("GET /api/v1/allocation/topup", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleTopUp)))))
	srv.Mount("GET /api/v1/allocation/rebalance", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleRebalance)))))
	srv.Mount("POST /api/v1/allocation/what-if", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleWhatIf)))))
	srv.Mount("GET /api/v1/allocation/plans", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handlePlans)))))
	srv.Mount("POST /api/v1/allocation/plans", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleEditor, http.HandlerFunc(h.handleSavePlan)))))
	srv.Mount("DELETE /api/v1/allocation/plans/{planId}", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleEditor, http.HandlerFunc(h.handleDeletePlan)))))
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

func (h *Handler) handleTopUp(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	var amounts [2]int64
	for i, name := range []string{"amount_minor", "from_brokers_minor"} {
		if raw := q.Get(name); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				httpjson.Error(w, http.StatusBadRequest, name+" must be a whole number")
				return
			}
			amounts[i] = n
		}
	}
	amount, fromBrokers := amounts[0], amounts[1]
	v, err := h.svc.View(r.Context(), p.SpaceID, valuationOf(r))
	if err != nil {
		family.WriteError(w, err)
		return
	}
	changes, err := TopUp(v, amount, fromBrokers)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AllocationTopUp{
		AmountMinor: amount, FromBrokersMinor: fromBrokers, WorthAfterMinor: v.Worth + amount,
		Kinds: make([]apitypes.AllocationChange, 0, len(changes)),
	}
	for _, c := range changes {
		out.Kinds = append(out.Kinds, apitypes.AllocationChange{Key: c.Key, ChangeMinor: c.Minor, ShareAfter: c.ShareAfter.StringFixed(2)})
	}
	httpjson.Write(w, http.StatusOK, out)
}

// opsOf reads a plan's operations as the API writes them.
func opsOf(list []apitypes.AllocationPlanOperation) []Op {
	plan := make([]Op, 0, len(list))
	for _, o := range list {
		op := Op{Kind: OpKind(o.Kind), Account: o.AccountId, Currency: o.Currency, Minor: o.AmountMinor}
		if o.ToAccountId.IsSpecified() && !o.ToAccountId.IsNull() {
			to := o.ToAccountId.MustGet()
			op.To = &to
		}
		if o.InstrumentId.IsSpecified() && !o.InstrumentId.IsNull() {
			id := o.InstrumentId.MustGet()
			op.Instrument = &id
		}
		plan = append(plan, op)
	}
	return plan
}

func (h *Handler) handlePlans(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	h.writePlans(w, r, p.SpaceID)
}

func (h *Handler) handleSavePlan(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.AllocationSavePlan
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.svc.SavePlan(r.Context(), p.SpaceID, req.Name, opsOf(req.Operations)); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writePlans(w, r, p.SpaceID)
}

func (h *Handler) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("planId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "planId must be a uuid")
		return
	}
	if err := h.svc.DeletePlan(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writePlans(w, r, p.SpaceID)
}

func (h *Handler) writePlans(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID) {
	plans, err := h.svc.Plans(r.Context(), spaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AllocationSavedPlans{Plans: make([]apitypes.AllocationSavedPlan, 0, len(plans))}
	for _, p := range plans {
		sp := apitypes.AllocationSavedPlan{Id: p.ID, Name: p.Name, SavedAt: p.SavedAt, Operations: make([]apitypes.AllocationPlanOperation, 0, len(p.Ops))}
		for _, op := range p.Ops {
			o := apitypes.AllocationPlanOperation{
				Kind: apitypes.AllocationPlanOperationKind(op.Kind), AccountId: op.Account, Currency: op.Currency, AmountMinor: op.Minor,
				ToAccountId: nullable.NewNullNullable[uuid.UUID](), InstrumentId: nullable.NewNullNullable[uuid.UUID](),
			}
			if op.To != nil {
				o.ToAccountId = nullable.NewNullableWithValue(*op.To)
			}
			if op.Instrument != nil {
				o.InstrumentId = nullable.NewNullableWithValue(*op.Instrument)
			}
			sp.Operations = append(sp.Operations, o)
		}
		out.Plans = append(out.Plans, sp)
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleWhatIf(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.AllocationPlan
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	plan := opsOf(req.Operations)
	wi, err := h.svc.WhatIf(r.Context(), p.SpaceID, valuationOf(r), plan)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AllocationWhatIf{
		BaseCurrency: wi.BaseCurrency, Cuts: make([]apitypes.AllocationCut, 0, len(wi.Cuts)),
		Sales: make([]apitypes.AllocationPaperSale, 0, len(wi.Sales)), MissingRates: wi.MissingRates,
		Uninsured: make([]apitypes.AllocationUninsured, 0, len(wi.Uninsured)),
		TaxMinor:  nullable.NewNullNullable[int64](), TaxRate: nullable.NewNullNullable[string](),
	}
	if out.MissingRates == nil {
		out.MissingRates = []string{}
	}
	for _, c := range wi.Cuts {
		cut := apitypes.AllocationCut{Dimension: apitypes.AllocationCutDimension(c.Dimension), Rows: make([]apitypes.AllocationCutRow, 0, len(c.Rows))}
		for _, r := range c.Rows {
			row := apitypes.AllocationCutRow{
				Key: r.Key, BeforeMinor: r.Before, AfterMinor: r.After, ShareBefore: r.ShareBefore.StringFixed(2),
				ShareAfter: r.ShareAfter.StringFixed(2), Target: nullable.NewNullNullable[string](), Verdict: apitypes.AllocationCutRowVerdict(r.Verdict),
			}
			if r.Target != nil {
				row.Target = nullable.NewNullableWithValue(r.Target.StringFixed(2))
			}
			cut.Rows = append(cut.Rows, row)
		}
		out.Cuts = append(out.Cuts, cut)
	}
	for _, u := range wi.Uninsured {
		out.Uninsured = append(out.Uninsured, apitypes.AllocationUninsured{UserId: u.Person, Bank: u.Bank, BeforeMinor: u.Before, AfterMinor: u.After})
	}
	for _, s := range wi.Sales {
		out.Sales = append(out.Sales, apitypes.AllocationPaperSale{InstrumentId: s.Instrument, GainMinor: s.Gain, FreeMinor: s.Free, Known: s.Known})
	}
	if wi.Tax != nil {
		out.TaxMinor = nullable.NewNullableWithValue(*wi.Tax)
	}
	if wi.TaxRate != nil {
		out.TaxRate = nullable.NewNullableWithValue(wi.TaxRate.String())
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleRebalance(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var amount int64
	if raw := r.URL.Query().Get("amount_minor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "amount_minor must be a whole number")
			return
		}
		amount = n
	}
	v, moves, est, rules, err := h.svc.Rebalancing(r.Context(), p.SpaceID, valuationOf(r), amount)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AllocationRebalance{
		AmountMinor: amount, WorthAfterMinor: v.Worth + amount, Kinds: make([]apitypes.AllocationMove, 0, len(moves)),
		TaxMinor: nullable.NewNullNullable[int64](), TaxRate: nullable.NewNullNullable[string](), LongHoldingYears: rules.LongHoldingYears,
	}
	if est.Tax != nil {
		out.TaxMinor = nullable.NewNullableWithValue(*est.Tax)
	}
	if rules.SaleTaxBP > 0 {
		out.TaxRate = nullable.NewNullableWithValue(decimal.New(int64(rules.SaleTaxBP), -2).String())
	}
	for _, m := range moves {
		switch {
		case m.Minor > 0:
			out.BoughtMinor += m.Minor
		case m.Key == string(structure.KindBrokerCash):
			out.FromBrokersMinor -= m.Minor
		default:
			out.SoldMinor -= m.Minor
		}
		move := apitypes.AllocationMove{
			Key: m.Key, ChangeMinor: m.Minor, LaterMinor: m.Later, KeptMinor: m.Kept, ShareAfter: m.ShareAfter.StringFixed(2),
			Sale: nullable.NewNullNullable[apitypes.AllocationSale](),
		}
		if sale, ok := est.Sales[m.Key]; ok {
			move.Sale = nullable.NewNullableWithValue(apitypes.AllocationSale{Known: sale.Known, GainMinor: sale.Gain, FreeMinor: sale.Free})
		}
		out.Kinds = append(out.Kinds, move)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// valuationOf is the valuation asked (?valuation=liquid|full), liquid when
// absent.
func valuationOf(r *http.Request) structure.Valuation {
	if raw := r.URL.Query().Get("valuation"); raw != "" {
		return structure.Valuation(raw)
	}
	return structure.Liquid
}

// writeView answers with the worth against the targets, papers at the
// valuation asked (?valuation=liquid|full, liquid when absent).
func (h *Handler) writeView(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID) {
	v, err := h.svc.View(r.Context(), spaceID, valuationOf(r))
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
		Countries: rowsAPI(v.Countries), Sectors: rowsAPI(v.Sectors), PapersMinor: v.Papers,
		Brokers: rowsAPI(v.Brokers), Banks: rowsAPI(v.Banks), AtBrokersMinor: v.AtBrokers, AtBanksMinor: v.AtBanks,
		Unpriced: v.Unpriced, MissingRates: missing,
	}
}
