package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// spaceStore is the subset of family.Store this handler needs: reading the
// space's base currency for GET /summary. Local interface (mirroring
// portfolio's journalStore/quoteStore) so tests can inject a fake or a real
// *family.Store interchangeably — Go interface assignability is structural,
// so *family.Store satisfies this with no conversion needed at the call
// site.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// converter is the subset of *marketdata.Converter this handler needs:
// ConvertMany for GET /summary's per-currency totals, Rate for converting each
// account's balance into the base currency at most once per currency per
// request (see balanceInBase), and RatesOn, which resolves many such pairs in a
// single round trip and answers each exactly as Rate would have (see
// marketdata.RatesOn). GET /accounts uses the last two, and not
// interchangeably: RatesOn fills the request's memo up front (prewarmRates) and
// Rate resolves whatever is not in it (balanceInBase), which is what keeps the
// figures independent of the prefetch.
//
// GET /summary needs no RatesOn of its own: ConvertMany owns both the
// resolution and the arithmetic behind total_in_base_minor, and batches its own
// lookups internally (see marketdata.ConvertMany). Splitting that here would
// mean restating the summing rule beside it, and two computations of one
// published figure drift.
//
// Local interface (mirroring spaceStore, and operation's and portfolio's
// identically named ones) so tests can inject a double in place of a real
// *marketdata.Converter — in particular one whose lookups fail with a genuine
// error rather than marketdata.ErrNoRate, which a real, DB-backed Converter
// cannot be made to do on demand and which the handler must treat completely
// differently (see balanceInBase). *marketdata.Converter satisfies this
// structurally, so no call site changes.
type converter interface {
	ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (converted int64, missing []string, ratesOn time.Time, err error)
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// Handler exposes the account module over HTTP.
type Handler struct {
	store     *Store
	spaces    spaceStore
	converter converter
	auth      *family.Auth
	sm        *scs.SessionManager
}

func NewHandler(store *Store, spaces spaceStore, converter converter, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, spaces: spaces, converter: converter, auth: auth, sm: sm}
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
}

func toAPI(a WithBalance) apitypes.AccountWithBalance {
	var ownerID nullable.Nullable[uuid.UUID]
	if a.OwnerUserID != nil {
		ownerID = nullable.NewNullableWithValue(*a.OwnerUserID)
	} else {
		// Explicit null (not omitted) so clients can tell "shared account" apart
		// from a field the server simply didn't return.
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
	// as_of is a date-only field with no timezone attached. A day of slack
	// past the UTC "today" boundary is intentional: a user anywhere from
	// UTC+3 to UTC+12 must be able to record "today" as of their own local
	// date, and their tomorrow-in-UTC is still today somewhere on Earth.
	// Anything further out than that is rejected as a genuine future date.
	maxAllowedUTC := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	if d.After(maxAllowedUTC) {
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

// rateKey identifies one memoized fx rate lookup: the currency being
// converted, the currency it is converted INTO, and the date its rate must come
// from. Mirrors operation.rateKey and portfolio.rateKey, which carry the same
// three fields.
//
// ONLY THE CURRENCY VARIES ON THIS SCREEN. Everything here is converted into
// the space's base currency at TODAY's rate, both read once per request and
// handed to every lookup and every prefetched query alike (see handleList), so
// the other two fields never change what any lookup finds and the memo would
// collapse identically without them. They are in the key because the memo now
// has TWO writers — the loop asking for a rate, and the prefetch filing answers
// before it (prewarmRates) — and a key of the currency alone cannot tell "USD
// at today's rate into RUB" from "USD at any other day's rate, or into anything
// else". An enumeration that named a different day or a different target would
// have its answer filed exactly where the loop looks, and read as the answer to
// a question it was never asked: a balance converted at yesterday's rate under
// a rate_on that says yesterday, on a screen where nothing looks wrong, and
// disagreeing with the total beside it — which converts at today's (see
// handleSummary). That is not hypothetical here; it is what a mutation of the
// enumeration's date did, invisibly to every test, until this key grew the
// field. With all three in the key the same mistake files the answer where
// nothing looks for it, and costs a round trip instead of a number.
//
// The date is held as its YYYY-MM-DD string rather than a time.Time so the key
// is a value comparison on the calendar date itself, immune to two
// otherwise-equal time.Time values differing in monotonic clock reading or
// *time.Location pointer — which would merely cost extra lookups, but would do
// so invisibly.
type rateKey struct {
	currency string
	target   string
	on       string
}

// newRateKey is the only place a lookup becomes a key. Both halves of the memo
// build one — the loop asking for a rate (balanceInBase) and the prefetch
// filing the answers before it (prewarmRates) — and a key spelled two ways
// would file every prefetched answer where nothing looks for it: no wrong
// number, just a batch paid for and then ignored, which is precisely the kind
// of failure that leaves no trace.
func newRateKey(currency, target string, on time.Time) rateKey {
	return rateKey{currency: currency, target: target, on: on.Format("2006-01-02")}
}

// rateLookup memoizes one lookup's resolved fx rate (and the date it came
// from, or the resolution error) so handleList's per-account conversion
// loop below hits the fx rate store at most once per distinct source
// currency, not once per account. See balanceInBase.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// needsRate reports whether a's balance has to be converted at all — stated
// once, as a fact about the account, because two things ask it: the loop that
// converts (balanceInBase, where a "no" is the null balance_in_base of an
// account with nothing to convert) and the enumeration that prefetches the
// rates that loop is about to want (rateQueries). Written twice, the two would
// drift, and the way they would drift is silent: the prefetch would stop
// covering a case the loop still converts, and nothing but the round-trip count
// would show it.
func needsRate(a WithBalance, baseCurrency string) bool {
	return a.Balance != nil && a.Currency != baseCurrency
}

// balanceInBase converts a's balance into baseCurrency at the fx rate of date
// on, using cache to memoize the underlying marketdata.Converter.Rate lookup
// across accounts that share a's currency within a single handleList request: N
// accounts denominated in the same non-base currency resolve that currency's
// rate once, not N times (cache, keyed by the (from, to, day) triple, lives
// for the lifetime of one request — see rateKey and handleList). Applying the
// same cached rate to each
// account's own amountMinor via decimal.Mul(...).Round(0) reproduces
// exactly what calling Converter.Convert per account would, per Rate's doc.
//
// The cache is normally already full when this is called: handleList prefetches
// every currency on the screen in one round trip (see prewarmRates). A MISS IS
// NOT AN ERROR — it is the whole safety net. Whatever the enumeration failed to
// predict, or the batch failed to fetch, is resolved here one currency at a time
// exactly as it was before any of that existed, so no figure on the screen
// depends on the prefetch being complete or even on its having succeeded. Only
// the cost does.
//
// It returns (nil, nil) — render balance_in_base as null — in exactly the
// three expected cases: the account has no balance at all, its currency
// already equals baseCurrency (nothing to convert), or no fx rate could be
// resolved for the pair (marketdata.ErrNoRate, e.g. an obscure currency the
// rate provider doesn't cover). A non-nil error means a genuine failure (DB
// error, canceled context) that the caller must surface as a request
// error — never silently rendered as null, which would misrepresent an
// outage as "nothing to convert".
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
	// Rounded once and refused rather than wrapped if the converted balance is
	// not an int64 of minor units (money.ErrOverflow, #27). The refusal is an
	// error and not the (nil, nil) above: that null says the provider covers no
	// rate for this currency, and a balance too large to state says something
	// else entirely.
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

	// One clock reading for the whole request, handed to the enumeration and to
	// the loop alike. Two readings would be two dates for a request that
	// straddles midnight: the accounts converted before it at one day's rates
	// and those after it at another's, on one screen, with nothing to say which
	// is which. The memo would notice (the day is in its key — see rateKey) and
	// pay for a second lookup; the screen would not.
	now := time.Now().UTC()

	// Scoped to this request only: see balanceInBase/rateLookup.
	rates := make(map[rateKey]*rateLookup)
	// Every currency on the screen, resolved in one round trip, so the loop
	// below finds its answer already in the memo. Nothing here is required for
	// the figures — see rateQueries, prewarmRates and balanceInBase.
	h.prewarmRates(r.Context(), rateQueries(accounts, sp.BaseCurrency, now), rates)

	out := make([]apitypes.AccountWithBalance, 0, len(accounts))
	for _, a := range accounts {
		api := toAPI(a)
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

// rateQueries enumerates every fx rate the loop in handleList is about to ask
// for, so one RatesOn call can resolve them all and every balanceInBase below
// finds its answer already in the memo. A space holding six currencies costs
// one round trip for the lot instead of one per currency — and a currency that
// needs a RUB bridge costs up to six on its own (#72).
//
// The axis is the number of distinct CURRENCIES, not the number of accounts:
// the memo has always collapsed same-currency accounts onto one lookup, which
// is exactly why this screen was left out of the earlier batching work and why
// the fixture behind it grows currencies rather than rows.
//
// IT IS DERIVED FROM THE CODE THAT CONSUMES THE RATES, NEVER WRITTEN BESIDE IT.
// Which accounts need a rate is asked here through the same needsRate the loop
// short-circuits on, so there is no second statement of that condition to keep
// in step with the first. This codebase has been bitten more than once by two
// computations of one value drifting apart, and a prefetch is the worst place
// for it: the two disagree in silence.
//
// The target and the date are not derived from anything, because there is
// nothing to derive them from: baseCurrency and on are read ONCE per request
// and handed to this function and to every balanceInBase call in the same
// breath (see handleList). What keeps a mistake there harmless is not that
// argument but the memo's key, which carries all three parts of the triple
// (rateKey): a query naming another day or another target is filed where
// nothing looks for it, and the loop resolves the rate it actually wanted.
//
// Completeness is an optimization, not a correctness condition, and the
// asymmetry is deliberate. Asking for a rate the loop turns out not to need
// costs one row in a query that was happening anyway. FAILING to ask for one
// costs a round trip and nothing else, because balanceInBase resolves whatever
// it does not find, exactly as it did before this existed. What would be
// dangerous is naming a DIFFERENT triple than the loop asks for and having its
// answer read as the loop's — and that cannot happen, for the reason above.
func rateQueries(accounts []WithBalance, baseCurrency string, on time.Time) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	// Deduplicated by the memo's own key — the whole triple, built by the same
	// newRateKey the loop below files its answers under — rather than by a
	// second, independent notion of "the same query" that could disagree with
	// it. On this screen the target and the date are the same for every
	// account, so the currency alone would dedupe identically today; using the
	// memo's key means the two cannot come apart the day that stops being true.
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

// prewarmRates resolves queries in one round trip and files each answer in the
// request's memo under the key balanceInBase will look it up by.
//
// NOTHING HERE FAILS THE REQUEST, and that is not laziness. This call buys
// speed, not truth: every figure on the screen is struck by balanceInBase,
// which resolves whatever it does not find in the memo. A batch that fails
// leaves the memo empty and the screen is computed exactly as it was before any
// prefetch existed. An outage is met again by the very next lookup and reported
// from the code that knows which figure it was resolving and can tell a missing
// rate (a null balance_in_base) from an outage (a 500). Failing here would move
// that judgement to a place that cannot make it, and would turn into an error
// page every request the fallback could have served correctly.
//
// WHICH ANSWERS GET FILED IS NOT DECIDED HERE. Rates.Answered walks the
// queries and hands back only the ones the batch resolved, so a query it never
// answered leaves no entry and balanceInBase resolves it itself, while a query
// answered with "no rate" arrives carrying marketdata.ErrNoRate and is filed as
// the honest gap it is. That rule is one statement for all three screens that
// warm a memo this way (see marketdata.Rates.Answered); only the key an answer
// is filed under is this package's own.
//
// KNOWN BLIND SPOT, the same one operation.Handler.prewarmRates and
// portfolio.Handler.prewarmRates carry: a failure specific to the BATCH
// statement is met by nothing, because the per-pair fallback then succeeds. The
// screen is correct and slow, and no one is told the optimization stopped
// working. No handler here holds a logger, so closing it is a change of shape
// rather than a line, and it is filed.
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
	if req.Name == "" || !Type(req.Type).Valid() || !currencyRe.MatchString(req.Currency) {
		httpjson.Error(w, http.StatusBadRequest,
			"name is required, type must be valid, currency must be ISO-4217 uppercase")
		return
	}
	institution := ""
	if req.Institution != nil {
		institution = *req.Institution
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
	a, err := h.store.Update(r.Context(), p.SpaceID, id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(a))
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
	if err := h.store.SetBalance(r.Context(), p.SpaceID, id, asOf, req.AmountMinor); err != nil {
		family.WriteError(w, err)
		return
	}
	a, err := h.store.ByID(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(a))
}

// handleSummary totals the space's accounts by currency, then converts and
// sums those per-currency totals into the space's base currency.
//
// The totals come exclusively from manually entered balances
// (account_balances, see Store.SummaryByCurrency). Positions computed by the
// portfolio engine from the operations journal are deliberately NOT added
// here: a brokerage account's securities are already reflected in the balance
// the user records for it, so counting positions on top would double count.
// Valuing brokerage accounts as "positions + cash" — and folding that
// valuation into this summary — has deliberately not been done: it would
// silently change what every balance the user has already recorded means.
// So the two views stay separate, and total_in_base_minor below is a sum of
// manual balances only, not full net worth including live market values.
//
// total_in_base_minor converts each currency's net_minor into base_currency
// using the latest fx rate on or before today (ConvertMany, on
// time.Now().UTC()) and sums the results. A currency with no resolvable
// rate is dropped into unconverted rather than failing the request — a
// partial total beats an error page. total_in_base_minor is null only when
// there's at least one currency and none of them converted (unconverted
// covers every currency in totals); an empty space, or a space whose
// accounts are already all in base_currency, yields 0, not null.
//
// rates_on discloses how stale that conversion might be: it's the date of
// the OLDEST fx rate ConvertMany actually used, which — since FxRateOn
// falls back to the nearest earlier date — can be well before today (e.g. a
// weekend or a currency the rate provider hasn't refreshed in a while). It
// is null only when nothing needed a rate at all (every currency already in
// base_currency, or every currency ended up unconverted).
func (h *Handler) handleSummary(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	totals, err := h.store.SummaryByCurrency(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	out := apitypes.Summary{
		Totals:       make([]apitypes.CurrencyTotal, 0, len(totals)),
		BaseCurrency: sp.BaseCurrency,
	}
	netByCurrency := make(map[string]int64, len(totals))
	for _, t := range totals {
		out.Totals = append(out.Totals, apitypes.CurrencyTotal{
			Currency: t.Currency, AssetsMinor: t.AssetsMinor,
			LiabilitiesMinor: t.LiabilitiesMinor, NetMinor: t.NetMinor,
		})
		netByCurrency[t.Currency] = t.NetMinor
	}

	// Filter out zero-amount currencies before conversion: zero minor units
	// convert to zero at any rate, so there's no need for an fx rate at all.
	// This prevents zero-amount currencies from incorrectly appearing in the
	// unconverted list when their rate is unavailable. The filtering preserves
	// the correct semantics: an empty space or all-zero balances yields
	// total_in_base_minor=0 (not null), while a space with only non-zero
	// currencies that all lack rates yields null.
	for currency, amount := range netByCurrency {
		if amount == 0 {
			delete(netByCurrency, currency)
		}
	}

	converted, missing, ratesOn, err := h.converter.ConvertMany(r.Context(), netByCurrency, sp.BaseCurrency, time.Now().UTC())
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
