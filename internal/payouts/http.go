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
	var accountID *uuid.UUID
	if raw := q.Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "account_id must be a uuid")
			return
		}
		accountID = &id
	}
	f, err := h.svc.Forecast(r.Context(), p.SpaceID, months, accountID)
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
