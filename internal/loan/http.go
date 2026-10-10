package loan

import (
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves a loan account's terms, schedule and payments.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
	now  func() time.Time
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm, now: time.Now}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/accounts/{accountId}/loan", view(h.handleGet))
	srv.Mount("PUT /api/v1/accounts/{accountId}/loan", edit(h.handlePut))
	srv.Mount("DELETE /api/v1/accounts/{accountId}/loan", edit(h.handleDelete))
	srv.Mount("POST /api/v1/accounts/{accountId}/loan/payments", edit(h.handlePayment))
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
	t, err := h.svc.Terms(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeLoan(w, http.StatusOK, t)
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var req apitypes.LoanTerms
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	rate, err := decimal.NewFromString(req.AnnualRate)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "annual_rate must be a decimal")
		return
	}
	issued, err := time.Parse(time.DateOnly, req.IssuedOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "issued_on must be YYYY-MM-DD")
		return
	}
	t, err := h.svc.SetTerms(r.Context(), p.SpaceID, Terms{
		AccountID: id, Principal: req.PrincipalMinor, AnnualRate: rate, TermMonths: req.TermMonths,
		IssuedOn: issued, Kind: Kind(req.Kind),
	})
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeLoan(w, http.StatusOK, t)
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

func (h *Handler) handlePayment(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var req apitypes.LoanPaymentRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	on, err := time.Parse(time.DateOnly, req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on must be YYYY-MM-DD")
		return
	}
	_, _, _, err = h.svc.RecordPayment(r.Context(), p.SpaceID, Payment{
		LoanAccountID: id, FromAccountID: req.FromAccountId, OccurredOn: on,
		Principal: req.PrincipalMinor, Interest: req.InterestMinor,
	})
	if errors.Is(err, operation.ErrInconsistent) {
		httpjson.Error(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// writeLoan answers the terms with the schedule and where it stands today.
func (h *Handler) writeLoan(w http.ResponseWriter, status int, t Terms) {
	rows, err := Schedule(t)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	now := h.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	out := apitypes.Loan{
		Terms: apitypes.LoanTerms{
			PrincipalMinor: t.Principal, AnnualRate: t.AnnualRate.String(), TermMonths: t.TermMonths,
			IssuedOn: t.IssuedOn.Format(time.DateOnly), Kind: apitypes.LoanTermsKind(t.Kind),
		},
		Schedule:            make([]apitypes.LoanRow, 0, len(rows)),
		LeftByScheduleMinor: t.Principal,
		Next:                nullable.NewNullNullable[apitypes.LoanRow](),
	}
	for _, row := range rows {
		api := apitypes.LoanRow{
			On: row.On.Format(time.DateOnly), PaymentMinor: row.Payment, InterestMinor: row.Interest,
			PrincipalMinor: row.Principal, LeftMinor: row.Left,
		}
		out.Schedule = append(out.Schedule, api)
		out.TotalInterestMinor += row.Interest
		if row.On.Before(today) {
			out.LeftByScheduleMinor = row.Left
		} else if !out.Next.IsSpecified() || out.Next.IsNull() {
			out.Next = nullable.NewNullableWithValue(api)
		}
	}
	httpjson.Write(w, status, out)
}
