package compare

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves POST /api/v1/deposit-comparison.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	// It reckons and keeps nothing: a viewer may compare too.
	srv.Mount("POST /api/v1/deposit-comparison", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleCompare)))))
}

func (h *Handler) handleCompare(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.DepositComparisonInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	ask := Ask{Amount: req.AmountMinor, Months: req.Months, Person: p.UserID, Preset: StateNow, Choices: map[CondKey]Choice{}}
	if id, err := req.PersonId.Get(); err == nil {
		ask.Person = id
	}
	if raw, err := req.KeyPercent.Get(); err == nil {
		pct, err := decimal.NewFromString(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "key_percent must be a decimal")
			return
		}
		ask.Key = Scenario{Custom: true, Percent: pct}
	}
	if req.Preset != nil {
		ask.Preset = State(*req.Preset)
	}
	if req.Choices != nil {
		for _, c := range *req.Choices {
			level := -1
			if l, err := c.Level.Get(); err == nil {
				level = l
			}
			ask.Choices[CondKey{c.ProgrammeId, c.VariantId, c.Condition}] = Choice{State: State(c.State), Level: level}
		}
	}
	if req.Audiences != nil {
		for _, a := range *req.Audiences {
			ask.Audiences = append(ask.Audiences, deposit.Audience(a))
		}
	}
	res, err := h.svc.Compare(r.Context(), p.SpaceID, ask)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, resultAPI(res))
}

func resultAPI(res Result) apitypes.DepositComparison {
	out := apitypes.DepositComparison{
		StartOn: res.Start.Format(time.DateOnly), EndOn: res.End.Format(time.DateOnly), KeyPercent: res.KeyPercent.String(),
		Person: res.Person, Skipped: res.Skipped, Rows: make([]apitypes.DepositComparisonRow, 0, len(res.Rows)),
	}
	minor := func(v *int64) nullable.Nullable[int64] {
		if v == nil {
			return nullable.NewNullNullable[int64]()
		}
		return nullable.NewNullableWithValue(*v)
	}
	for _, r := range res.Rows {
		e, _ := r.Programme.EditionOn(res.Start)
		row := apitypes.DepositComparisonRow{
			ProgrammeId: r.Programme.ID, ProgrammeName: r.Programme.Name, BankId: r.Programme.Bank, BankName: r.BankName,
			Kind: apitypes.DepositComparisonRowKind(r.Programme.Kind), VariantId: r.Variant.ID, VariantName: r.Variant.Name,
			Audience: make([]apitypes.DepositComparisonRowAudience, 0, len(r.Variant.Audience)),
			Term:     nullable.NewNullNullable[int](), TermUnit: nullable.NewNullNullable[apitypes.DepositComparisonRowTermUnit](),
			Payout: apitypes.DepositComparisonRowPayout(r.Earning.Payout), Prolonged: r.Earning.Prolonged, Days: r.Earning.Days,
			IncomeMinor: r.Earning.Income, YearlyPercent: r.Yearly.StringFixed(2), TaxMinor: minor(r.Tax), NetMinor: r.Net,
			InsuredMinor: minor(r.Insured), EditionFrom: e.From, Sources: e.Sources,
			Conditions: make([]apitypes.DepositComparisonCondition, 0, len(r.Conditions)),
		}
		if row.Sources == nil {
			row.Sources = []string{}
		}
		for _, a := range r.Variant.Audience {
			row.Audience = append(row.Audience, apitypes.DepositComparisonRowAudience(a))
		}
		if r.Programme.Kind == deposit.KindDeposit {
			row.Term = nullable.NewNullableWithValue(r.Earning.Term)
			row.TermUnit = nullable.NewNullableWithValue(apitypes.DepositComparisonRowTermUnit(r.Variant.TermUnit))
		}
		for _, c := range r.Conditions {
			cond := apitypes.DepositComparisonCondition{
				Id: c.ID, Name: c.Name, Kind: apitypes.DepositComparisonConditionKindBonus, Check: apitypes.DepositComparisonConditionCheck(c.Check),
				Group: nullable.NewNullNullable[string](), Plus: nullable.NewNullNullable[string](), Levels: []apitypes.BonusLevel{},
				NowKnown: c.Now.Known, NowMet: c.Now.Met, NowLevel: c.Now.Level,
				MeasuredMinor: nullable.NewNullNullable[int64](), MeasuredMonth: nullable.NewNullNullable[string](),
				State: apitypes.DepositComparisonConditionState(c.Used.State), ChosenLevel: c.Used.Level, Met: c.Met, Level: c.Level,
			}
			if c.Requirement {
				cond.Kind = apitypes.DepositComparisonConditionKindRequirement
			}
			if c.Group != "" {
				cond.Group = nullable.NewNullableWithValue(c.Group)
			}
			if c.Plus != "" {
				cond.Plus = nullable.NewNullableWithValue(c.Plus)
			}
			for _, l := range c.Levels {
				cond.Levels = append(cond.Levels, apitypes.BonusLevel{From: l.From, Plus: l.Plus})
			}
			if c.Now.Known && c.Check != deposit.CheckManual {
				cond.MeasuredMinor = nullable.NewNullableWithValue(c.Now.Value)
			}
			if !c.Now.Month.IsZero() {
				cond.MeasuredMonth = nullable.NewNullableWithValue(c.Now.Month.Format("2006-01"))
			}
			row.Conditions = append(row.Conditions, cond)
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}
