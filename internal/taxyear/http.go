package taxyear

import (
	"errors"
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

// Handler serves GET /api/v1/tax-year and the wages written by hand.
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
	srv.Mount("GET /api/v1/tax-year/foreign", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleForeign)))))
	srv.Mount("GET /api/v1/tax-year/foreign-report", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleReport)))))
	srv.Mount("GET /api/v1/accounts/{id}/income", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleIncome)))))
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("PUT /api/v1/tax-year/wages/{operationId}", edit(h.handleEnter))
	srv.Mount("DELETE /api/v1/tax-year/wages/{operationId}", edit(h.handleUnenter))
}

func (h *Handler) handleEnter(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "operationId must be a uuid")
		return
	}
	var req apitypes.TaxYearWageEntry
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.svc.Enter(r.Context(), p.SpaceID, id, Entry{Gross: req.GrossMinor, Withheld: req.WithheldMinor}); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleUnenter(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "operationId must be a uuid")
		return
	}
	if err := h.svc.Unenter(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
			NoticeBy:  time.Date(year+1, time.December, 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
			KeyRate:   nullable.NewNullNullable[string](),
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
		DeclareBy: y.DeclareBy.Format(time.DateOnly), PayBy: y.PayBy.Format(time.DateOnly), NoticeBy: y.NoticeBy.Format(time.DateOnly),
		KeyRate: nullable.NewNullNullable[string](),
	}
	if y.KeyRate != nil {
		out.KeyRate = nullable.NewNullableWithValue(y.KeyRate.StringFixed(2))
	}
	for _, p := range y.People {
		person := apitypes.TaxYearPerson{
			UserId: p.UserID, Name: p.Name, BaseMinor: p.Base, TaxMinor: p.Tax, ByAgentsMinor: p.ByAgents,
			WithheldMinor: p.Withheld, CreditMinor: p.Credit, ToPayMinor: p.ToPay + p.WagesToPay, RefundMinor: p.Refund, Unvalued: p.Unvalued,
			WagesNetMinor: p.WagesNet, WagesMinor: p.Wages, WagesTaxMinor: p.WagesTax, WagesWithheldMinor: p.WagesWithheld, WagesToPayMinor: p.WagesToPay,
			Employers:         make([]apitypes.TaxYearEmployer, 0, len(p.Employers)),
			WagePayments:      make([]apitypes.TaxYearWagePayment, 0, len(p.WagePayments)),
			PropertySoldMinor: p.PropertySold, PropertyGainMinor: p.PropertyGain, PropertyBaseMinor: p.PropertyBase, PropertyFree: p.PropertyFree,
			InterestMinor: p.Interest, InterestFreeMinor: nullable.NewNullNullable[int64](), ByNoticeMinor: p.ByNotice,
			InterestEstimatedMinor: p.InterestEstimated, InterestCountedMinor: p.Interest + p.InterestEstimated, InterestAheadMinor: p.InterestAhead, ByNoticeYearEndMinor: nullable.NewNullNullable[int64](),
			Accounts: make([]apitypes.TaxYearAccount, 0, len(p.Accounts)),
		}
		for _, a := range p.Accounts {
			acc := apitypes.TaxYearAccount{
				AccountId: a.ID, Name: a.Name, Bank: a.Bank, ForeignBroker: a.ForeignBroker,
				SalesMinor: a.Facts.Sales, SalesLongMinor: a.Facts.SalesLong, CouponsMinor: a.Facts.Coupons,
				DividendsMinor: a.Facts.Dividends, InterestMinor: a.Facts.Interest, WithheldMinor: a.Facts.Withheld,
				SecuritiesMinor: a.Securities, AgentTaxMinor: a.AgentTax, Unvalued: a.Facts.Unvalued,
				Estimate: nullable.NewNullNullable[apitypes.TaxYearEstimate](),
			}
			if e := a.Estimate; e != nil {
				acc.Estimate = nullable.NewNullableWithValue(estimateAPI(*e))
			}
			person.Accounts = append(person.Accounts, acc)
		}
		for _, e := range p.Employers {
			person.Employers = append(person.Employers, apitypes.TaxYearEmployer{Name: e.Name, NetMinor: e.Net, GrossMinor: e.Gross, WithheldMinor: e.Withheld})
		}
		for _, w := range p.WagePayments {
			person.WagePayments = append(person.WagePayments, apitypes.TaxYearWagePayment{
				OperationId: w.OperationID, On: w.On.Format(time.DateOnly), Employer: w.Employer,
				NetMinor: w.Net, GrossMinor: w.Gross, WithheldMinor: w.Withheld, Entered: w.Entered,
			})
		}
		if p.InterestFree != nil {
			person.InterestFreeMinor = nullable.NewNullableWithValue(*p.InterestFree)
		}
		if p.ByNoticeYearEnd != nil {
			person.ByNoticeYearEndMinor = nullable.NewNullableWithValue(*p.ByNoticeYearEnd)
		}
		out.People = append(out.People, person)
		out.ToPayMinor += p.ToPay + p.WagesToPay
		out.RefundMinor += p.Refund
	}
	return out
}

func (h *Handler) handleIncome(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "id must be a uuid")
		return
	}
	in, err := h.svc.IncomeOf(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, incomeAPI(in))
}

func incomeAPI(in Income) apitypes.AccountIncome {
	minor := func(v *int64) nullable.Nullable[int64] {
		if v == nil {
			return nullable.NewNullNullable[int64]()
		}
		return nullable.NewNullableWithValue(*v)
	}
	date := func(t *time.Time) nullable.Nullable[string] {
		if t == nil {
			return nullable.NewNullNullable[string]()
		}
		return nullable.NewNullableWithValue(t.Format(time.DateOnly))
	}
	out := apitypes.AccountIncome{
		Currency: in.Currency, TaxCurrency: in.TaxCurrency, Year: in.YearNo, Rated: in.Rated,
		EndsOn: date(in.Ends), Payout: nullable.NewNullNullable[apitypes.AccountIncomePayout](),
		TermMinor: minor(in.Term), AccruedMinor: minor(in.Accrued), AtEndMinor: minor(in.AtEnd),
		MonthMinor: minor(in.Month), MonthOn: date(in.MonthOn), TwelveMinor: minor(in.Twelve),
		YearEndMinor: minor(in.YearEnd), YearMinor: minor(in.Year),
		Estimate: nullable.NewNullNullable[apitypes.TaxYearEstimate](), Tax: nullable.NewNullNullable[apitypes.AccountIncomeTax](),
	}
	if pays := in.Pays; pays != nil {
		way := apitypes.AccountIncomePayoutEnd
		switch {
		case pays.Monthly && pays.Capitalize:
			way = apitypes.AccountIncomePayoutCapitalize
		case pays.Monthly:
			way = apitypes.AccountIncomePayoutMonthly
		}
		out.Payout = nullable.NewNullableWithValue(way)
	}
	if in.Estimate != nil {
		out.Estimate = nullable.NewNullableWithValue(estimateAPI(*in.Estimate))
	}
	if t := in.Tax; t != nil {
		names := t.Unestimated
		if names == nil {
			names = []string{}
		}
		out.Tax = nullable.NewNullableWithValue(apitypes.AccountIncomeTax{
			Person: t.Person, InterestMinor: t.Interest, FreeMinor: minor(t.Free), TaxMinor: t.Tax, ShareMinor: t.Share, Unestimated: names,
		})
	}
	return out
}

func estimateAPI(e Estimate) apitypes.TaxYearEstimate {
	date := func(t *time.Time) nullable.Nullable[string] {
		if t == nil {
			return nullable.NewNullNullable[string]()
		}
		return nullable.NewNullableWithValue(t.Format(time.DateOnly))
	}
	out := apitypes.TaxYearEstimate{
		PaidMinor: e.Paid, From: date(e.From), AheadMinor: e.Ahead, NoRateAhead: e.NoRateAhead,
		Missing: nullable.NewNullNullable[apitypes.TaxYearEstimateMissing](), MissingOn: date(e.MissingOn), PaysOn: date(e.PaysOn),
	}
	if e.Missing != "" {
		out.Missing = nullable.NewNullableWithValue(apitypes.TaxYearEstimateMissing(e.Missing))
	}
	return out
}

func (h *Handler) handleForeign(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	year, err := parseYear(r.URL.Query().Get("year"), h.now())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	f, err := h.svc.ForeignOf(r.Context(), p.SpaceID, year)
	if errors.Is(err, ErrNoEstimate) {
		httpjson.Write(w, http.StatusOK, apitypes.TaxYearForeign{Year: year, Accounts: []apitypes.TaxYearForeignAccount{}})
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, foreignAPI(f))
}

func foreignAPI(f Foreign) apitypes.TaxYearForeign {
	rate := func(known bool, r decimal.Decimal) nullable.Nullable[string] {
		if !known {
			return nullable.NewNullNullable[string]()
		}
		return nullable.NewNullableWithValue(r.String())
	}
	paper := func(id *uuid.UUID) nullable.Nullable[uuid.UUID] {
		if id == nil {
			return nullable.NewNullNullable[uuid.UUID]()
		}
		return nullable.NewNullableWithValue(*id)
	}
	day := func(t *time.Time) nullable.Nullable[string] {
		if t == nil {
			return nullable.NewNullNullable[string]()
		}
		return nullable.NewNullableWithValue(t.Format(time.DateOnly))
	}
	out := apitypes.TaxYearForeign{Year: f.Year, Supported: true, Currency: f.Currency, TreatiesAsOf: f.TreatiesAsOf, Accounts: make([]apitypes.TaxYearForeignAccount, 0, len(f.Accounts))}
	for _, a := range f.Accounts {
		acc := apitypes.TaxYearForeignAccount{
			AccountId: a.ID, Name: a.Name, UserId: a.Person, Country: a.Country, CountryCode: countryCode(a.Country),
			IncomeBaseMinor: a.Income, ExpensesBaseMinor: a.Expenses,
			Unvalued: a.Unvalued, Payouts: make([]apitypes.TaxYearForeignPayout, 0, len(a.Payouts)),
			Sales: make([]apitypes.TaxYearForeignSale, 0, len(a.Sales)), OtherTaxes: make([]apitypes.TaxYearForeignTax, 0, len(a.Taxes)),
		}
		for _, p := range a.Payouts {
			acc.Payouts = append(acc.Payouts, apitypes.TaxYearForeignPayout{
				On: p.On.Format(time.DateOnly), Type: apitypes.TaxYearForeignPayoutType(p.Type), Kind: p.Kind, InstrumentId: paper(p.Instrument),
				Paper: p.Paper, Isin: p.ISIN, Country: p.Country, CountryCode: countryCode(p.Country), Currency: p.Currency,
				CurrencyCode: currencyCode(p.Currency), AmountMinor: p.Minor, WithheldMinor: p.Withheld,
				Rate: rate(p.Known, p.Rate), AmountBaseMinor: p.InBase, WithheldBaseMinor: p.WithheldInBase, TaxRuMinor: p.TaxRU, CreditMinor: p.Credit,
				Treaty: apitypes.TaxYearForeignPayoutTreaty(p.Treaty), TreatyFrom: day(p.TreatyFrom),
			})
		}
		for _, s := range a.Sales {
			sale := apitypes.TaxYearForeignSale{
				On: s.On.Format(time.DateOnly), SettledOn: s.RateOn.Format(time.DateOnly), InstrumentId: s.Instrument, Paper: s.Paper, Isin: s.ISIN,
				Quantity: s.Quantity.String(), Currency: s.Currency, CurrencyCode: currencyCode(s.Currency), ProceedsMinor: s.Proceeds, FeeMinor: s.Fee, Rate: rate(!s.Rate.IsZero(), s.Rate),
				ProceedsBaseMinor: s.ProceedsInBase, FeeBaseMinor: s.FeeInBase, Lots: make([]apitypes.TaxYearForeignLot, 0, len(s.Lots)),
				ResultBaseMinor: nullable.NewNullNullable[int64](), LongBaseMinor: s.Long,
			}
			if s.Known {
				sale.ResultBaseMinor = nullable.NewNullableWithValue(s.Result)
			}
			for _, l := range s.Lots {
				sale.Lots = append(sale.Lots, apitypes.TaxYearForeignLot{
					AcquiredOn: day(l.AcquiredOn), Quantity: l.Quantity.String(), Currency: l.Currency, CostMinor: l.Cost,
					Rate: rate(!l.Rate.IsZero(), l.Rate), CostBaseMinor: l.CostInBase,
				})
			}
			acc.Sales = append(acc.Sales, sale)
		}
		for _, t := range a.Taxes {
			acc.OtherTaxes = append(acc.OtherTaxes, apitypes.TaxYearForeignTax{
				On: t.On.Format(time.DateOnly), InstrumentId: paper(t.Instrument), Paper: t.Paper, Currency: t.Currency, WithheldMinor: t.Withheld,
				Rate: rate(t.Known, t.Rate), WithheldBaseMinor: t.WithheldInBase,
			})
		}
		out.Accounts = append(out.Accounts, acc)
	}
	return out
}

func (h *Handler) handleReport(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	year, err := parseYear(r.URL.Query().Get("year"), h.now())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	rep, err := h.svc.ReportOf(r.Context(), p.SpaceID, year)
	if errors.Is(err, ErrNoEstimate) {
		httpjson.Write(w, http.StatusOK, apitypes.TaxYearForeignReport{
			Year: year, Accounts: []apitypes.TaxYearReportAccount{},
			DueBy: time.Date(year+1, time.June, 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
			Until: time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
		})
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, reportAPI(rep))
}

// thousands is a sum in thousands of its currency to three decimals — whole
// units — as the form КНД 1112520 asks it.
func thousands(minor int64) string {
	return decimal.NewFromInt(minor).Div(decimal.NewFromInt(100_000)).StringFixed(3)
}

func reportAPI(r Report) apitypes.TaxYearForeignReport {
	papers := func(m map[string]int64) []apitypes.TaxYearReportPapers {
		out := make([]apitypes.TaxYearReportPapers, 0, len(m))
		for c, minor := range m {
			out = append(out, apitypes.TaxYearReportPapers{Currency: c, CurrencyCode: currencyCode(c), Minor: minor, Thousands: thousands(minor)})
		}
		slices.SortFunc(out, func(a, b apitypes.TaxYearReportPapers) int { return strings.Compare(a.Currency, b.Currency) })
		return out
	}
	out := apitypes.TaxYearForeignReport{
		Year: r.Year, Supported: true, Currency: r.Currency, FreeUpToMinor: r.FreeUpTo, DueBy: r.DueBy.Format(time.DateOnly), Until: r.Until.Format(time.DateOnly),
		Accounts: make([]apitypes.TaxYearReportAccount, 0, len(r.Accounts)),
	}
	for _, a := range r.Accounts {
		acc := apitypes.TaxYearReportAccount{
			AccountId: a.ID, Name: a.Name, UserId: a.Person, Country: a.Country, CountryCode: countryCode(a.Country),
			Cash: make([]apitypes.TaxYearReportCash, 0, len(a.Moves.Cash)), PapersStart: papers(a.Moves.PapersStart),
			PapersEnd: papers(a.Moves.PapersEnd), Unpriced: a.Moves.Unpriced, InMinor: a.In, NothingIn: a.NothingIn,
			Verdict: apitypes.TaxYearReportAccountVerdict(a.Verdict),
		}
		for _, c := range a.Moves.Cash {
			acc.Cash = append(acc.Cash, apitypes.TaxYearReportCash{
				Currency: c.Currency, CurrencyCode: currencyCode(c.Currency), StartMinor: c.Start, InMinor: c.In, OutMinor: c.Out, EndMinor: c.End,
				StartThousands: thousands(c.Start), InThousands: thousands(c.In), OutThousands: thousands(c.Out), EndThousands: thousands(c.End),
			})
		}
		out.Accounts = append(out.Accounts, acc)
	}
	return out
}
