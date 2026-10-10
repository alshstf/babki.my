package creditcard

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
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
	srv.Mount("PUT /api/v1/accounts/{accountId}/credit-card/installments/{operationId}", edit(h.handlePutInstallment))
	srv.Mount("DELETE /api/v1/accounts/{accountId}/credit-card/installments/{operationId}", edit(h.handleDeleteInstallment))
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
		RunFrom: RunFrom(req.GraceRunFrom), PayDay: req.PayDay, MinRoundUp: req.MinRoundUpMinor,
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
	if t.Fees, err = feesFromAPI(req.Fees); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if t.Cashback, err = cashbackFromAPI(req.Cashback); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if t.Installment, err = planFromAPI(req.Installment); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
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

func (h *Handler) handlePutInstallment(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	op, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid operationId")
		return
	}
	var req apitypes.CreditCardInstallmentPlan
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	plan, err := planFromAPI(req)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.SetInstallment(r.Context(), p.SpaceID, id, op, plan); err != nil {
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

func (h *Handler) handleDeleteInstallment(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	op, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid operationId")
		return
	}
	if err := h.svc.DeleteInstallment(r.Context(), p.SpaceID, id, op); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func planFromAPI(in apitypes.CreditCardInstallmentPlan) (Plan, error) {
	p := Plan{Months: in.Months, Fee: in.FeeMinor}
	if in.MonthlyFeePercent != "" {
		v, err := decimal.NewFromString(in.MonthlyFeePercent)
		if err != nil {
			return Plan{}, fmt.Errorf("monthly_fee_percent must be a decimal")
		}
		p.MonthlyFeePercent = v
	}
	return p, nil
}

func planAPI(p Plan) apitypes.CreditCardInstallmentPlan {
	return apitypes.CreditCardInstallmentPlan{Months: p.Months, MonthlyFeePercent: p.MonthlyFeePercent.String(), FeeMinor: p.Fee}
}

// InstallmentsExport is the card's purchases in installments as the export
// writes them, by their row.
func InstallmentsExport(plans map[uuid.UUID]Plan) []apitypes.ExportCardInstallment {
	out := make([]apitypes.ExportCardInstallment, 0, len(plans))
	for id, p := range plans {
		out = append(out, apitypes.ExportCardInstallment{OperationId: id, Plan: planAPI(p)})
	}
	slices.SortFunc(out, func(a, b apitypes.ExportCardInstallment) int {
		return strings.Compare(a.OperationId.String(), b.OperationId.String())
	})
	return out
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

// cashbackFromAPI reads the cashback rules; an empty percent is none.
func cashbackFromAPI(in apitypes.CreditCardCashback) (Cashback, error) {
	c := Cashback{MonthlyCap: in.MonthlyCapMinor, Points: in.Points, CreditDays: in.CreditDays}
	read := func(name, s string) (decimal.Decimal, error) {
		if s == "" {
			return decimal.Zero, nil
		}
		v, err := decimal.NewFromString(s)
		if err != nil {
			return decimal.Zero, fmt.Errorf("cashback.%s must be a decimal", name)
		}
		return v, nil
	}
	var err error
	if c.BasePercent, err = read("base_percent", in.BasePercent); err != nil {
		return Cashback{}, err
	}
	for _, cat := range in.Categories {
		pct, err := read("categories.percent", cat.Percent)
		if err != nil {
			return Cashback{}, err
		}
		c.Categories = append(c.Categories, CategoryPercent{CategoryID: cat.CategoryId, Percent: pct})
	}
	return c, nil
}

func cashbackAPI(c Cashback) apitypes.CreditCardCashback {
	out := apitypes.CreditCardCashback{
		BasePercent: c.BasePercent.String(), MonthlyCapMinor: c.MonthlyCap, Points: c.Points, CreditDays: c.CreditDays,
		Categories: make([]apitypes.CreditCardCashbackCategory, 0, len(c.Categories)),
	}
	for _, cat := range c.Categories {
		out.Categories = append(out.Categories, apitypes.CreditCardCashbackCategory{CategoryId: cat.CategoryID, Percent: cat.Percent.String()})
	}
	return out
}

// feesFromAPI reads the tariff's fees; an empty percent is none.
func feesFromAPI(in apitypes.CreditCardFees) (Fees, error) {
	f := Fees{Monthly: in.MonthlyMinor, CashFree: in.CashFreeMinor, CashFixed: in.CashFixedMinor, TransferFixed: in.TransferFixedMinor}
	for _, p := range []struct {
		name string
		in   string
		out  *decimal.Decimal
	}{
		{"cash_percent", in.CashPercent, &f.CashPercent},
		{"transfer_percent", in.TransferPercent, &f.TransferPercent},
		{"penalty_daily_percent", in.PenaltyDailyPercent, &f.PenaltyDaily},
	} {
		if p.in == "" {
			continue
		}
		v, err := decimal.NewFromString(p.in)
		if err != nil {
			return Fees{}, fmt.Errorf("fees.%s must be a decimal", p.name)
		}
		*p.out = v
	}
	return f, nil
}

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
		GraceRunFrom: apitypes.CreditCardTermsGraceRunFrom(t.RunFrom), PayDay: t.PayDay, MinRoundUpMinor: t.MinRoundUp,
		GraceAllLost: t.GraceAllLost, PayByPeriodEnd: t.PayByPeriodEnd, ChargesInFull: t.ChargesInFull,
		TransferCategories: t.TransferCategories,
		Fees: apitypes.CreditCardFees{
			MonthlyMinor: t.Fees.Monthly, CashFreeMinor: t.Fees.CashFree, CashPercent: t.Fees.CashPercent.String(),
			CashFixedMinor: t.Fees.CashFixed, TransferPercent: t.Fees.TransferPercent.String(),
			TransferFixedMinor: t.Fees.TransferFixed, PenaltyDailyPercent: t.Fees.PenaltyDaily.String(),
		},
		Cashback:    cashbackAPI(t.Cashback),
		Installment: planAPI(t.Installment),
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
		CashThisPeriodMinor: st.CashThisPeriod, PenaltyMinor: st.Penalty, MinimumOverdueMinor: st.MinimumOverdue,
		InstallmentsDueMinor: st.InstallmentsDue, Installments: make([]apitypes.CreditCardInstallment, 0, len(st.Installments)),
		CashbackExpectedMinor: st.CashbackExpected, CashbackOn: nullable.NewNullNullable[string](),
	}
	for _, in := range st.Installments {
		out.Installments = append(out.Installments, apitypes.CreditCardInstallment{
			OperationId: in.OperationID, On: date(in.On), AmountMinor: in.Amount, Plan: planAPI(in.Plan),
			Billed: in.Billed, LeftMinor: in.Left, NextMinor: in.Next, Note: in.Note,
		})
	}
	if !st.CashbackOn.IsZero() {
		out.CashbackOn = nullable.NewNullableWithValue(date(st.CashbackOn))
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
