package creditcard

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

// Handler serves a credit card's terms and status, and the list for the
// reminders.
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
	srv.Mount("GET /api/v1/accounts/{accountId}/credit-card", view(h.handleGet))
	srv.Mount("PUT /api/v1/accounts/{accountId}/credit-card", edit(h.handlePut))
	srv.Mount("DELETE /api/v1/accounts/{accountId}/credit-card", edit(h.handleDelete))
	srv.Mount("GET /api/v1/credit-cards", view(h.handleList))
}

func accountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "accountId must be a uuid")
		return uuid.UUID{}, false
	}
	return id, true
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	c, err := h.svc.Card(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, cardAPI(c))
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var req apitypes.CreditCardTerms
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	t := Terms{
		AccountID: id, Limit: req.LimitMinor, StatementDay: req.StatementDay, PaymentDays: req.PaymentDays,
		GraceKind: GraceKind(req.GraceKind), GraceDays: req.GraceDays, MinFloor: req.MinFloorMinor,
		WindowMonths: req.WindowMonths, GraceMonths: req.GraceMonths, GraceAllLost: req.GraceAllLost,
		PayByPeriodEnd: req.PayByPeriodEnd, ChargesInFull: req.ChargesInFull, TransferCategories: req.TransferCategories,
	}
	var err error
	if req.OpenedOn.IsSpecified() && !req.OpenedOn.IsNull() {
		on, err := time.Parse(time.DateOnly, req.OpenedOn.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "opened_on must be a date YYYY-MM-DD")
			return
		}
		t.OpenedOn = &on
	}
	if t.MinPercent, err = decimal.NewFromString(req.MinPercent); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "min_percent must be a decimal")
		return
	}
	if t.AnnualRate, err = decimal.NewFromString(req.AnnualRate); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "annual_rate must be a decimal")
		return
	}
	if req.OwnRate.IsSpecified() && !req.OwnRate.IsNull() {
		own, err := decimal.NewFromString(req.OwnRate.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "own_rate must be a decimal")
			return
		}
		t.OwnRate = &own
	}
	if _, err := h.svc.SetTerms(r.Context(), p.SpaceID, t); err != nil {
		family.WriteError(w, err)
		return
	}
	c, err := h.svc.Card(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, cardAPI(c))
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteTerms(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	cards, err := h.svc.All(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.CreditCardSummary, 0, len(cards))
	for _, c := range cards {
		out = append(out, apitypes.CreditCardSummary{
			AccountId: c.Account.ID, Name: c.Account.Name, Currency: c.Account.Currency,
			ByJournal: c.ByJournal, Status: statusAPI(c.Status),
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}

func date(t time.Time) string { return t.Format(time.DateOnly) }

// TermsAPI is the terms as the API and the export write them.
func TermsAPI(t Terms) apitypes.CreditCardTerms {
	own := nullable.NewNullNullable[string]()
	if t.OwnRate != nil {
		own = nullable.NewNullableWithValue(t.OwnRate.String())
	}
	opened := nullable.NewNullNullable[string]()
	if t.OpenedOn != nil {
		opened = nullable.NewNullableWithValue(date(*t.OpenedOn))
	}
	out := apitypes.CreditCardTerms{
		LimitMinor: t.Limit, StatementDay: t.StatementDay, PaymentDays: t.PaymentDays,
		GraceKind: apitypes.CreditCardTermsGraceKind(t.GraceKind), GraceDays: t.GraceDays,
		MinPercent: t.MinPercent.String(), MinFloorMinor: t.MinFloor,
		AnnualRate: t.AnnualRate.String(), OwnRate: own,
		WindowMonths: t.WindowMonths, GraceMonths: t.GraceMonths, OpenedOn: opened,
		GraceAllLost: t.GraceAllLost, PayByPeriodEnd: t.PayByPeriodEnd, ChargesInFull: t.ChargesInFull,
		TransferCategories: t.TransferCategories,
	}
	if out.TransferCategories == nil {
		out.TransferCategories = []uuid.UUID{}
	}
	return out
}

func cardAPI(c Card) apitypes.CreditCard {
	out := apitypes.CreditCard{
		Terms: TermsAPI(c.Terms), Status: statusAPI(c.Status), ByJournal: c.ByJournal,
		Benefit: nullable.NewNullNullable[apitypes.CreditCardBenefit](),
	}
	if b := c.Benefit; b != nil {
		out.Benefit = nullable.NewNullableWithValue(apitypes.CreditCardBenefit{
			From: date(b.From), To: date(b.To), OwnRateKnown: b.OwnRateKnown, OwnEarnedMinor: b.OwnEarned,
			CashbackMinor: b.Cashback, CostsMinor: b.Costs, PendingInterestMinor: b.Pending, TotalMinor: b.Total,
		})
	}
	return out
}

func statusAPI(st Status) apitypes.CreditCardStatus {
	out := apitypes.CreditCardStatus{
		DebtMinor: st.Debt, AvailableMinor: st.Available,
		LastStatement: date(st.LastStatement), NextStatement: date(st.NextStatement),
		MinimumMinor: st.Minimum, MinimumOn: date(st.MinimumOn), MinimumMissed: st.MinimumMissed,
		MinimumEstimate: st.MinimumEstimate, NonGraceMinor: st.NonGrace, NonGraceInterestMinor: st.NonGraceInterest,
		Grace: make([]apitypes.CreditCardDue, 0, len(st.Grace)), Lost: make([]apitypes.CreditCardLost, 0, len(st.Lost)),
		GraceOffSince: nullable.NewNullNullable[string](), GraceOffByMinimum: st.GraceOffByMinimum, ToRestoreMinor: st.ToRestore,
	}
	if !st.GraceOffSince.IsZero() {
		out.GraceOffSince = nullable.NewNullableWithValue(date(st.GraceOffSince))
	}
	for _, g := range st.Grace {
		out.Grace = append(out.Grace, apitypes.CreditCardDue{On: date(g.On), AmountMinor: g.Amount})
	}
	for _, l := range st.Lost {
		out.Lost = append(out.Lost, apitypes.CreditCardLost{
			From: date(l.From), To: date(l.To), Deadline: date(l.Deadline), AmountMinor: l.Amount, InterestMinor: l.Interest,
			Early: l.Early,
		})
	}
	return out
}
