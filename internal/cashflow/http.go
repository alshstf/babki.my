package cashflow

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

// Handler serves GET /api/v1/cashflow.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/cashflow", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleReport)))))
}

func (h *Handler) handleReport(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	var days [2]time.Time
	for i, name := range []string{"from", "to"} {
		day, err := time.Parse(time.DateOnly, q.Get(name))
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, name+" must be YYYY-MM-DD")
			return
		}
		days[i] = day
	}
	var whose Whose
	switch raw := q.Get("member"); raw {
	case "":
	case "shared":
		whose.Shared = true
	default:
		id, err := uuid.Parse(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "member must be a member's id or shared")
			return
		}
		whose.UserID = &id
	}
	report, err := h.svc.Report(r.Context(), p.SpaceID, days[0], days[1], whose)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(report))
}

func flowAPI(f Flow) apitypes.CashflowFlow {
	return apitypes.CashflowFlow{TotalMinor: f.Total, ByMonth: f.ByMonth}
}

func lineAPI(l Line) apitypes.CashflowLine {
	out := apitypes.CashflowLine{
		CategoryId: nullable.NewNullNullable[uuid.UUID](), Group: nullable.NewNullNullable[apitypes.CashflowLineGroup](),
		TotalMinor: l.Total, ByMonth: l.ByMonth, Direct: flowAPI(l.Direct), Children: []apitypes.CashflowLine{},
	}
	if l.CategoryID != nil {
		out.CategoryId = nullable.NewNullableWithValue(*l.CategoryID)
	}
	if l.Group != "" {
		out.Group = nullable.NewNullableWithValue(apitypes.CashflowLineGroup(l.Group))
	}
	for _, c := range l.Children {
		out.Children = append(out.Children, lineAPI(c))
	}
	return out
}

func sectionAPI(s Section) apitypes.CashflowSection {
	out := apitypes.CashflowSection{TotalMinor: s.Total, ByMonth: s.ByMonth, Lines: []apitypes.CashflowLine{}}
	for _, l := range s.Lines {
		out.Lines = append(out.Lines, lineAPI(l))
	}
	return out
}

func toAPI(r Report) apitypes.CashflowReport {
	out := apitypes.CashflowReport{
		BaseCurrency: r.BaseCurrency,
		From:         r.From.Format(time.DateOnly), To: r.To.Format(time.DateOnly),
		Months: make([]string, len(r.Months)),
		Income: sectionAPI(r.Income), Expense: sectionAPI(r.Expense),
		UnfiledIn: flowAPI(r.UnfiledIn), UnfiledOut: flowAPI(r.UnfiledOut),
		Investments: apitypes.CashflowInvestments{
			Deposited: flowAPI(r.Investments.Deposited), Withdrawn: flowAPI(r.Investments.Withdrawn),
			Payouts: flowAPI(r.Investments.Payouts), Interest: flowAPI(r.Investments.Interest),
			Costs: flowAPI(r.Investments.Costs),
		},
		MissingRates: r.MissingRates, LeftOut: r.LeftOut,
	}
	for i, m := range r.Months {
		out.Months[i] = m.Format("2006-01")
	}
	return out
}
