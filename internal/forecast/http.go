package forecast

import (
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves GET /api/v1/forecast.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/forecast", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleForecast)))))
}

func (h *Handler) handleForecast(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	days := DefaultDays
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "days must be a whole number")
			return
		}
		days = n
	}
	f, err := h.svc.Of(r.Context(), p.SpaceID, days)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	day := func(d Day) apitypes.ForecastDay {
		return apitypes.ForecastDay{On: d.On.Format(time.DateOnly), BalanceMinor: d.Balance}
	}
	event := func(e Event) apitypes.ForecastEvent {
		return apitypes.ForecastEvent{
			On: e.On.Format(time.DateOnly), Name: e.Name, Kind: apitypes.ForecastEventKind(e.Kind), AccountId: e.AccountID,
			AmountMinor: e.Amount, Currency: e.Currency, InBaseMinor: e.InBase, Overdue: e.Overdue,
		}
	}
	out := apitypes.Forecast{
		BaseCurrency: f.BaseCurrency, Days: f.Days, StartMinor: f.Start, AccountsCounted: f.Accounts,
		Series: make([]apitypes.ForecastDay, 0, len(f.Series)), Events: make([]apitypes.ForecastEvent, 0, len(f.Events)),
		Lowest: day(f.Lowest), MissingRates: f.MissingRates,
		NextIncome:           nullable.NewNullNullable[apitypes.ForecastEvent](),
		FreeUntilIncomeMinor: nullable.NewNullNullable[int64](),
	}
	for _, d := range f.Series {
		out.Series = append(out.Series, day(d))
	}
	for _, e := range f.Events {
		out.Events = append(out.Events, event(e))
	}
	if f.NextIncome != nil {
		out.NextIncome = nullable.NewNullableWithValue(event(*f.NextIncome))
	}
	if f.FreeUntilIncome != nil {
		out.FreeUntilIncomeMinor = nullable.NewNullableWithValue(*f.FreeUntilIncome)
	}
	httpjson.Write(w, http.StatusOK, out)
}
