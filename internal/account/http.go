package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

// spaceStore is what the handler needs from family.Store: the base currency.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// converter is what the handler needs from marketdata.Converter: ConvertMany
// for the summary total, RatesOn to prefetch a request's rates and Rate for
// whatever the prefetch missed. Tests substitute one whose lookups fail with a
// real error rather than ErrNoRate.
type converter interface {
	ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (converted int64, missing []string, ratesOn time.Time, err error)
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

type Handler struct {
	store     *Store
	spaces    spaceStore
	converter converter
	// journals values brokerage accounts from their operations; nil values every
	// account by its balance.
	journals journalValuer
	auth     *family.Auth
	sm       *scs.SessionManager
}

func NewHandler(store *Store, spaces spaceStore, converter converter, journals journalValuer, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, spaces: spaces, converter: converter, journals: journals, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/accounts", view(h.handleList))
	srv.Mount("POST /api/v1/accounts", edit(h.handleCreate))
	srv.Mount("PATCH /api/v1/accounts/{accountId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/accounts/{accountId}", edit(h.handleArchive))
	srv.Mount("PUT /api/v1/accounts/{accountId}/balance", edit(h.handleSetBalance))
	srv.Mount("GET /api/v1/summary", view(h.handleSummary))
	srv.Mount("GET /api/v1/capital", view(h.handleCapital))
	srv.Mount("GET /api/v1/return", view(h.handleFamilyReturn))
}

func (h *Handler) handleCapital(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	from, err := time.Parse("2006-01-02", r.URL.Query().Get("from"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
		return
	}
	step := r.URL.Query().Get("step")
	if step != "" && step != "week" && step != "month" {
		httpjson.Error(w, http.StatusBadRequest, "step must be week or month")
		return
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days, err := capitalDays(from, today, step != "week")
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	points, err := h.capital(r.Context(), p.SpaceID, sp.BaseCurrency, days)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.CapitalSeries{Currency: sp.BaseCurrency, Points: make([]apitypes.CapitalPoint, 0, len(points))}
	for _, pt := range points {
		item := apitypes.CapitalPoint{
			Day: pt.Day.Format("2006-01-02"), TotalMinor: pt.Minor, Complete: pt.Complete,
			Accounts: make([]apitypes.CapitalAccount, 0, len(pt.Accounts)),
		}
		for _, a := range pt.Accounts {
			by := apitypes.CapitalAccountCountedByBalance
			if a.ByJournal {
				by = apitypes.CapitalAccountCountedByJournal
			}
			item.Accounts = append(item.Accounts, apitypes.CapitalAccount{
				AccountId: a.AccountID, AmountMinor: a.Minor, CountedBy: by, Complete: a.Complete,
			})
		}
		out.Points = append(out.Points, item)
	}
	httpjson.Write(w, http.StatusOK, out)
}

func toAPI(a WithBalance) apitypes.AccountWithBalance {
	var ownerID nullable.Nullable[uuid.UUID]
	if a.OwnerUserID != nil {
		ownerID = nullable.NewNullableWithValue(*a.OwnerUserID)
	} else {
		// Explicit null, so a shared account is not mistaken for a missing field.
		ownerID = nullable.NewNullNullable[uuid.UUID]()
	}
	out := apitypes.AccountWithBalance{
		Id:          a.ID,
		OwnerUserId: ownerID,
		Name:        a.Name,
		Type:        apitypes.AccountType(a.Type),
		Currency:    a.Currency,
		Institution: a.Institution,
		Status:      apitypes.AccountStatus(a.Status),
		CreatedAt:   a.CreatedAt,
		// Overwritten wherever the account is valued.
		ValuedByBalance: a.ValuedByBalance,
		CountedBy:       apitypes.AccountWithBalanceCountedByBalance,
		Journal:         nullable.NewNullNullable[apitypes.AccountJournal](),
	}
	if a.Balance != nil {
		out.Balance = &apitypes.BalancePoint{
			AsOf:        a.Balance.AsOf.Format("2006-01-02"),
			AmountMinor: a.Balance.AmountMinor,
		}
	}
	return out
}

func parseAsOf(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("as_of must be YYYY-MM-DD")
	}
	// as_of is held to the shared ceiling for hand-entered dates (see
	// dates.LatestRecordable).
	if d.After(dates.LatestRecordable()) {
		return time.Time{}, fmt.Errorf("as_of must not be in the future")
	}
	return d, nil
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// rateKey identifies one memoized rate lookup: currency, target and day. Only
// the currency varies on this screen, but the memo has two writers (the loop
// and the prefetch), and keying all three parts means a prefetched answer for
// the wrong day or target is filed where nothing looks for it — a round trip
// lost, not a wrong number. The day is a YYYY-MM-DD string so equal days are
// equal keys.
type rateKey struct {
	currency string
	target   string
	on       string
}

// newRateKey is the only place a lookup becomes a key.
func newRateKey(currency, target string, on time.Time) rateKey {
	return rateKey{currency: currency, target: target, on: on.Format("2006-01-02")}
}

// rateLookup memoizes one resolved rate, its date or its error, for the
// duration of a request.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// needsRate reports whether a's balance needs converting; both the loop and
// the prefetch ask it.
func needsRate(a WithBalance, baseCurrency string) bool {
	return a.Balance != nil && a.Currency != baseCurrency
}

// balanceInBase converts a's balance into baseCurrency at the rate of on,
// memoized per request. A memo miss is resolved here, so no figure depends on
// the prefetch.
//
// It returns (nil, nil) — a null balance_in_base — when there is no balance,
// nothing to convert, or no rate (ErrNoRate). Any other error is a real
// failure the caller must report.
func (h *Handler) balanceInBase(ctx context.Context, a WithBalance, baseCurrency string, on time.Time, cache map[rateKey]*rateLookup) (*apitypes.MoneyInBase, error) {
	if !needsRate(a, baseCurrency) {
		return nil, nil
	}
	key := newRateKey(a.Currency, baseCurrency, on)
	rl, ok := cache[key]
	if !ok {
		rate, date, err := h.converter.Rate(ctx, a.Currency, baseCurrency, on)
		rl = &rateLookup{rate: rate, date: date, err: err}
		cache[key] = rl
	}
	if rl.err != nil {
		if errors.Is(rl.err, marketdata.ErrNoRate) {
			return nil, nil
		}
		return nil, rl.err
	}
	// Rounded once; an overflow is an error, not the "no rate" null (#27).
	minor, err := money.Minor(decimal.NewFromInt(a.Balance.AmountMinor).Mul(rl.rate))
	if err != nil {
		return nil, fmt.Errorf("%w: balance of account %s in %s", err, a.ID, baseCurrency)
	}
	return &apitypes.MoneyInBase{
		AmountMinor: minor,
		Currency:    baseCurrency,
		RateOn:      rl.date.Format("2006-01-02"),
	}, nil
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accounts, err := h.store.ListWithBalance(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	// One clock reading for the whole request, so a request straddling midnight
	// does not convert at two days' rates.
	now := time.Now().UTC()

	// Scoped to this request.
	rates := make(map[rateKey]*rateLookup)
	// Prefetch every rate the loop will need in one round trip.
	h.prewarmRates(r.Context(), rateQueries(accounts, sp.BaseCurrency, now), rates)
	vals, err := h.valuations(r.Context(), p.SpaceID, accounts, sp.BaseCurrency, now, rates)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	out := make([]apitypes.AccountWithBalance, 0, len(accounts))
	for _, a := range accounts {
		api := toAPI(a)
		vals[a.ID].describe(&api)
		inBase, err := h.balanceInBase(r.Context(), a, sp.BaseCurrency, now, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if inBase != nil {
			api.BalanceInBase = nullable.NewNullableWithValue(*inBase)
		} else {
			api.BalanceInBase = nullable.NewNullNullable[apitypes.MoneyInBase]()
		}
		out = append(out, api)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// rateQueries lists every rate the loop will ask for, so one RatesOn call
// resolves them (#72). It uses the loop's own needsRate. Completeness is only
// an optimization: a missed query costs a round trip, and a wrong one is filed
// where nothing looks.
func rateQueries(accounts []WithBalance, baseCurrency string, on time.Time) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	// Deduplicated by the memo's own key.
	seen := make(map[rateKey]bool, len(accounts))
	for _, a := range accounts {
		key := newRateKey(a.Currency, baseCurrency, on)
		if !needsRate(a, baseCurrency) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, marketdata.RateQuery{From: a.Currency, To: baseCurrency, On: on})
	}
	return out
}

// prewarmRates resolves queries in one round trip and files each answer in
// the memo. Nothing here fails the request: balanceInBase resolves whatever is
// missing and tells a missing rate from an outage. Rates.Answered decides what
// is filed; a failed batch is logged where it dies (#70).
func (h *Handler) prewarmRates(ctx context.Context, queries []marketdata.RateQuery, cache map[rateKey]*rateLookup) {
	if len(queries) == 0 {
		return
	}
	resolved, err := h.converter.RatesOn(ctx, queries)
	if err != nil {
		return
	}
	for q, res := range resolved.Answered(queries) {
		cache[newRateKey(q.From, q.To, q.On)] = &rateLookup{rate: res.Rate, date: res.RateDate, err: res.Err}
	}
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateAccountRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if req.Name == "" || !Type(req.Type).Valid() || !currency.Valid(req.Currency) {
		httpjson.Error(w, http.StatusBadRequest,
			"name is required, type must be valid, currency must be ISO-4217 uppercase")
		return
	}
	institution := ""
	if req.Institution != nil {
		institution = *req.Institution
	}
	if !textsFit(w, &req.Name, &institution) {
		return
	}
	var ownerID *uuid.UUID
	if req.OwnerUserId.IsSpecified() && !req.OwnerUserId.IsNull() {
		v := req.OwnerUserId.MustGet()
		ownerID = &v
	}
	a, err := h.store.Create(r.Context(), p.SpaceID, ownerID,
		req.Name, Type(req.Type), req.Currency, institution)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(WithBalance{Account: a}))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateAccountRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	upd := Update{Name: req.Name, Institution: req.Institution}
	if req.Status != nil {
		st := Status(*req.Status)
		if st != StatusActive && st != StatusArchived {
			httpjson.Error(w, http.StatusBadRequest, "invalid status")
			return
		}
		upd.Status = &st
	}
	if req.OwnerUserId.IsSpecified() {
		if req.OwnerUserId.IsNull() {
			var cleared *uuid.UUID
			upd.OwnerUserID = &cleared
		} else {
			v := req.OwnerUserId.MustGet()
			ptr := &v
			upd.OwnerUserID = &ptr
		}
	}
	if req.Name != nil && *req.Name == "" {
		httpjson.Error(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	if !textsFit(w, req.Name, req.Institution) {
		return
	}
	upd.ValuedByBalance = req.ValuedByBalance
	a, err := h.store.Update(r.Context(), p.SpaceID, id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeOne(w, r, p.SpaceID, a)
}

// MaxNameRunes and MaxInstitutionRunes bound an account's name and
// institution in characters, as the contract's maxLength counts them.
const (
	MaxNameRunes        = 100
	MaxInstitutionRunes = 100
)

// textsFit answers 400 and false when a given text is over its ceiling; nil
// was not sent and fits.
func textsFit(w http.ResponseWriter, name, institution *string) bool {
	if name != nil && utf8.RuneCountInString(*name) > MaxNameRunes {
		httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("name must be at most %d characters", MaxNameRunes))
		return false
	}
	if institution != nil && utf8.RuneCountInString(*institution) > MaxInstitutionRunes {
		httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("institution must be at most %d characters", MaxInstitutionRunes))
		return false
	}
	return true
}

func (h *Handler) handleArchive(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	if err := h.store.Archive(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleSetBalance(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.SetBalanceRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	asOf, err := parseAsOf(req.AsOf)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	// An archived account takes no balance mark: it is out of every total, and a
	// mark would surface only on restore. The broker's own balance is recorded
	// regardless.
	current, err := h.store.ByID(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if current.Status == StatusArchived {
		httpjson.Error(w, http.StatusBadRequest, "the account is archived; bring it back from the archive to change it")
		return
	}
	// The only door into account_balances.amount_minor (#89): an unbounded value
	// made the accounts list fail for the whole space. Both ends are compared
	// explicitly, so MinInt64 is refused too; the bound is the operations' one.
	if req.AmountMinor > money.MaxAmountMinor || req.AmountMinor < -money.MaxAmountMinor {
		httpjson.Error(w, http.StatusBadRequest,
			fmt.Sprintf("amount_minor must be within ±%d", money.MaxAmountMinor))
		return
	}
	if err := h.store.SetBalance(r.Context(), p.SpaceID, id, asOf, req.AmountMinor); err != nil {
		family.WriteError(w, err)
		return
	}
	a, err := h.store.ByID(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeOne(w, r, p.SpaceID, a)
}

// writeOne answers with one account valued as the list values it.
func (h *Handler) writeOne(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID, a WithBalance) {
	sp, err := h.spaces.SpaceByID(r.Context(), spaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	vals, err := h.valuations(r.Context(), spaceID, []WithBalance{a}, sp.BaseCurrency,
		time.Now().UTC(), make(map[rateKey]*rateLookup))
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := toAPI(a)
	vals[a.ID].describe(&out)
	httpjson.Write(w, http.StatusOK, out)
}

// handleSummary totals the space's accounts by currency and converts the
// totals into the base currency.
//
// An active brokerage account is counted by its journal (holdings at market
// value plus cash) unless pinned to its balance; every other account by its
// latest balance (decision Р-2). journalSummary says what the total owes to
// journals.
//
// total_in_base_minor converts each currency at the latest rate on or before
// today. A currency with no rate goes to unconverted; the total is null only
// when nothing converted. rates_on is the date of the oldest rate used, null
// when none was.
func (h *Handler) handleSummary(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	accounts, err := h.store.ListWithBalance(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	now := time.Now().UTC()
	vals, err := h.valuations(r.Context(), p.SpaceID, accounts, sp.BaseCurrency, now, make(map[rateKey]*rateLookup))
	if err != nil {
		family.WriteError(w, err)
		return
	}
	var byJournal []uuid.UUID
	for id, v := range vals {
		if v.byJournal {
			byJournal = append(byJournal, id)
		}
	}
	totals, err := h.store.SummaryByCurrency(r.Context(), p.SpaceID, byJournal)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if totals, err = addJournals(totals, vals); err != nil {
		family.WriteError(w, err)
		return
	}
	journal, err := journalSummary(vals)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	out := apitypes.Summary{
		Totals:       make([]apitypes.CurrencyTotal, 0, len(totals)),
		BaseCurrency: sp.BaseCurrency,
		Journal:      journal,
	}
	netByCurrency := make(map[string]int64, len(totals))
	for _, t := range totals {
		out.Totals = append(out.Totals, apitypes.CurrencyTotal{
			Currency: t.Currency, AssetsMinor: t.AssetsMinor,
			LiabilitiesMinor: t.LiabilitiesMinor, NetMinor: t.NetMinor,
		})
		netByCurrency[t.Currency] = t.NetMinor
	}

	// Zero amounts need no rate, so they are dropped first and never reported
	// as unconverted.
	for currency, amount := range netByCurrency {
		if amount == 0 {
			delete(netByCurrency, currency)
		}
	}

	converted, missing, ratesOn, err := h.converter.ConvertMany(r.Context(), netByCurrency, sp.BaseCurrency, now)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if missing == nil {
		missing = []string{} // unconverted must serialize as [], never null
	}
	out.Unconverted = missing

	if len(netByCurrency) > 0 && len(missing) == len(netByCurrency) {
		out.TotalInBaseMinor = nullable.NewNullNullable[int64]()
	} else {
		out.TotalInBaseMinor = nullable.NewNullableWithValue(converted)
	}

	if ratesOn.IsZero() {
		out.RatesOn = nullable.NewNullNullable[string]()
	} else {
		out.RatesOn = nullable.NewNullableWithValue(ratesOn.Format("2006-01-02"))
	}

	httpjson.Write(w, http.StatusOK, out)
}
