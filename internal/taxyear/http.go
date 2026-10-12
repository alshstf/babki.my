package taxyear

import (
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves GET /api/v1/tax-year.
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
	srv.Mount("GET /api/v1/tax-year", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleGet)))))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	year, err := parseYear(r.URL.Query().Get("year"), h.now())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	y, err := h.svc.Of(r.Context(), p.SpaceID, year)
	if errors.Is(err, ErrNoEstimate) {
		httpjson.Write(w, http.StatusOK, apitypes.TaxYear{
			Year: year, People: []apitypes.TaxYearPerson{},
			DeclareBy: time.Date(year+1, time.April, 30, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
			PayBy:     time.Date(year+1, time.July, 15, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
		})
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, YearAPI(y))
}

// YearAPI is the year as the API writes it.
func YearAPI(y Year) apitypes.TaxYear {
	out := apitypes.TaxYear{
		Year: y.Year, Supported: true, Currency: y.Currency, People: make([]apitypes.TaxYearPerson, 0, len(y.People)),
		DeclareBy: y.DeclareBy.Format(time.DateOnly), PayBy: y.PayBy.Format(time.DateOnly),
	}
	for _, p := range y.People {
		person := apitypes.TaxYearPerson{
			UserId: p.UserID, Name: p.Name, BaseMinor: p.Base, TaxMinor: p.Tax, ByAgentsMinor: p.ByAgents,
			WithheldMinor: p.Withheld, CreditMinor: p.Credit, ToPayMinor: p.ToPay, RefundMinor: p.Refund, Unvalued: p.Unvalued,
			Accounts: make([]apitypes.TaxYearAccount, 0, len(p.Accounts)),
		}
		for _, a := range p.Accounts {
			person.Accounts = append(person.Accounts, apitypes.TaxYearAccount{
				AccountId: a.ID, Name: a.Name, ForeignBroker: a.ForeignBroker,
				SalesMinor: a.Facts.Sales, SalesLongMinor: a.Facts.SalesLong, CouponsMinor: a.Facts.Coupons,
				DividendsMinor: a.Facts.Dividends, InterestMinor: a.Facts.Interest, WithheldMinor: a.Facts.Withheld,
				SecuritiesMinor: a.Securities, AgentTaxMinor: a.AgentTax, Unvalued: a.Facts.Unvalued,
			})
		}
		out.People = append(out.People, person)
		out.ToPayMinor += p.ToPay
		out.RefundMinor += p.Refund
	}
	return out
}
