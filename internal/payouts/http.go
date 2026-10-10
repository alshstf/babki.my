package payouts

import (
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves GET /api/v1/payouts.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/payouts", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleForecast)))))
	srv.Mount("GET /api/v1/payouts/received", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleReceived)))))
}

// scopeOf reads account_id and instrument_id; false once it answered 400.
func scopeOf(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	var scope Scope
	q := r.URL.Query()
	for _, f := range []struct {
		name string
		into **uuid.UUID
	}{{"account_id", &scope.AccountID}, {"instrument_id", &scope.InstrumentID}} {
		if raw := q.Get(f.name); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				httpjson.Error(w, http.StatusBadRequest, f.name+" must be a uuid")
				return Scope{}, false
			}
			*f.into = &id
		}
	}
	return scope, true
}

func (h *Handler) handleReceived(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	days := 90
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "days must be a whole number")
			return
		}
		days = n
	}
	scope, ok := scopeOf(w, r)
	if !ok {
		return
	}
	checks, err := h.svc.Received(r.Context(), p.SpaceID, days, scope)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.PayoutCheck, 0, len(checks))
	for _, c := range checks {
		item := apitypes.PayoutCheck{
			On: c.On.Format(time.DateOnly), RecordOn: c.RecordOn.Format(time.DateOnly),
			Kind: apitypes.PayoutCheckKind(c.Kind), InstrumentId: c.InstrumentID, AccountId: c.AccountID,
			Quantity: c.Quantity.String(), PerUnit: nullable.NewNullNullable[string](),
			AmountMinor: nullable.NewNullNullable[int64](), Currency: c.Currency,
			Status: apitypes.PayoutCheckStatus(c.Status), GotMinor: c.Got,
			GotCurrency: nullable.NewNullNullable[string](), GotOn: nullable.NewNullNullable[string](),
		}
		if c.PerUnit != nil {
			item.PerUnit = nullable.NewNullableWithValue(c.PerUnit.String())
		}
		if c.Amount != nil {
			item.AmountMinor = nullable.NewNullableWithValue(*c.Amount)
		}
		if c.GotOn != nil {
			item.GotOn = nullable.NewNullableWithValue(c.GotOn.Format(time.DateOnly))
			item.GotCurrency = nullable.NewNullableWithValue(c.GotCurrency)
		}
		out = append(out, item)
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleForecast(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	months := 12
	if raw := q.Get("months"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "months must be a whole number")
			return
		}
		months = n
	}
	scope, ok := scopeOf(w, r)
	if !ok {
		return
	}
	f, err := h.svc.Forecast(r.Context(), p.SpaceID, months, scope)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(f))
}

func toAPI(f Forecast) apitypes.PayoutsForecast {
	out := apitypes.PayoutsForecast{
		BaseCurrency: f.BaseCurrency,
		From:         f.From.Format(time.DateOnly), To: f.To.Format(time.DateOnly),
		Months: make([]string, len(f.Months)), ByMonth: f.ByMonth, TotalMinor: f.Total,
		Payouts: make([]apitypes.Payout, 0, len(f.Events)), MissingRates: f.MissingRates,
	}
	for i, m := range f.Months {
		out.Months[i] = m.Format("2006-01")
	}
	for _, e := range f.Events {
		p := apitypes.Payout{
			On: e.On.Format(time.DateOnly), RecordOn: nullable.NewNullNullable[string](),
			Kind: apitypes.PayoutKind(e.Kind), InstrumentId: e.InstrumentID, AccountId: e.AccountID,
			Quantity: e.Quantity.String(), PerUnit: nullable.NewNullNullable[string](),
			AmountMinor: nullable.NewNullNullable[int64](), Currency: e.Currency,
			InBaseMinor: nullable.NewNullNullable[int64](),
		}
		if e.RecordOn != nil {
			p.RecordOn = nullable.NewNullableWithValue(e.RecordOn.Format(time.DateOnly))
		}
		if e.PerUnit != nil {
			p.PerUnit = nullable.NewNullableWithValue(e.PerUnit.String())
		}
		if e.Amount != nil {
			p.AmountMinor = nullable.NewNullableWithValue(*e.Amount)
		}
		if e.InBase != nil {
			p.InBaseMinor = nullable.NewNullableWithValue(*e.InBase)
		}
		out.Payouts = append(out.Payouts, p)
	}
	return out
}
