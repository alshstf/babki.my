package operation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
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
	"babki.my/babki/internal/platform/tradingmode"
	"babki.my/babki/internal/portfolio"
)

// defaultListLimit and maxListLimit are the default and maximum openapi states
// for GET .../operations (contract_sites_test.go keeps them in step). A limit
// outside them, or one that is not a number, is refused rather than clamped or
// defaulted: a ceiling the contract states and the server does not apply is no
// rule at all (#118).
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// spaceStore is the part of family.Store this handler needs: the space's base
// currency.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Handler exposes the operations journal (and transfers) over HTTP.
type Handler struct {
	svc    *Service
	store  *Store
	spaces spaceStore
	conv   marketdata.RateSource
	auth   *family.Auth
	sm     *scs.SessionManager
}

func NewHandler(svc *Service, store *Store, spaces spaceStore, conv marketdata.RateSource, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, store: store, spaces: spaces, conv: conv, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("POST /api/v1/operations", edit(h.handleCreate))
	srv.Mount("GET /api/v1/accounts/{accountId}/operations", view(h.handleListByAccount))
	srv.Mount("PUT /api/v1/operations/{operationId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/operations/{operationId}", edit(h.handleDelete))
	srv.Mount("POST /api/v1/operations/transfer", edit(h.handleTransfer))
	srv.Mount("POST /api/v1/operations/money-transfer", edit(h.handleMoneyTransfer))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/operations", view(h.handleListByInstrument))
	srv.Mount("PUT /api/v1/operations/{operationId}/purchases", edit(h.handleStatePurchases))
	srv.Mount("POST /api/v1/operations/arrivals", edit(h.handleCreateArrival))
	srv.Mount("GET /api/v1/accounts/{accountId}/instruments/{instrumentId}/arrivals", view(h.handleListArrivals))
}

// writeError maps operation errors to HTTP responses: ErrInconsistent is a 409
// with the engine's explanation; everything else goes to family.WriteError.
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInconsistent) {
		httpjson.Error(w, http.StatusConflict, err.Error())
		return
	}
	family.WriteError(w, err)
}

// carriesCostBasis reports whether amount_minor is a cost basis that travelled
// with shares rather than money that moved on its own date: what
// portfolio.MovesCash excludes, minus split, whose amount is always zero. Derived
// from MovesCash so the two lists cannot drift.
func carriesCostBasis(o Operation) bool {
	return !portfolio.MovesCash(o) && o.Type != TypeSplit
}

// hasUndatedLots reports whether amount_minor is a cost basis with at least one
// piece of unknown purchase date. Unlike a missing rate, which the backfill
// closes, this never resolves, so the row says so instead of leaving a reader
// to infer it from a null in_base. The journal's counterpart of
// portfolio.hasUndatedLots.
func hasUndatedLots(o Operation) bool {
	if !carriesCostBasis(o) {
		return false
	}
	// No breakdown means every piece is dateless (see amountTerms), unless
	// the basis is zero (#226).
	if len(o.TransferLots) == 0 {
		return portfolio.DatelessBasis(nil, o.AmountMinor)
	}
	return slices.ContainsFunc(o.TransferLots, func(l ReleasedLot) bool {
		return portfolio.DatelessBasis(l.AcquiredOn, l.CostMinor)
	})
}

// assembledFromLots reports whether amount_minor is a basis assembled from the
// purchases in a stored breakdown. It is a fact about the row, published on every
// operation whether or not in_base could be computed (#67). A breakdown with
// dateless pieces answers yes here and to hasUndatedLots too.
func assembledFromLots(o Operation) bool {
	return len(o.TransferLots) > 0
}

func toAPI(o Operation) apitypes.Operation {
	out := apitypes.Operation{
		Id:          o.ID,
		AccountId:   o.AccountID,
		Type:        apitypes.OperationType(o.Type),
		OccurredOn:  o.OccurredOn.Format("2006-01-02"),
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		FeeMinor:    o.FeeMinor,
		Note:        o.Note,
		// The column's CHECK constraint closes the set the contract enumerates.
		Source:            apitypes.OperationSource(o.Source),
		CreatedAt:         o.CreatedAt,
		HasUndatedLots:    hasUndatedLots(o),
		AssembledFromLots: assembledFromLots(o),
	}
	// The mode and its kind are set together and are both absent when
	// nobody said where the operation happened.
	if o.TradingMode != nil {
		out.TradingMode = nullable.NewNullableWithValue(*o.TradingMode)
		out.TradingModeKind = nullable.NewNullableWithValue(
			apitypes.TradingModeKind(tradingmode.Of(*o.TradingMode)))
	}
	if o.InstrumentID != nil {
		out.InstrumentId = nullable.NewNullableWithValue(*o.InstrumentID)
	}
	if o.SettledOn != nil {
		out.SettledOn = nullable.NewNullableWithValue(o.SettledOn.Format("2006-01-02"))
	}
	if o.Quantity != nil {
		out.Quantity = nullable.NewNullableWithValue(o.Quantity.String())
	}
	if o.Price != nil {
		out.Price = nullable.NewNullableWithValue(o.Price.String())
	}
	if o.SplitRatio != nil {
		out.SplitRatio = nullable.NewNullableWithValue(o.SplitRatio.String())
	}
	if o.TransferGroupID != nil {
		out.TransferGroupId = nullable.NewNullableWithValue(*o.TransferGroupID)
	}
	return out
}

// inBaseGap names which term stopped an operation's in_base object, published
// as Operation.in_base_gap. The journal's counterpart of portfolio.inBaseGap.
type inBaseGap uint8

const (
	// inBaseStruck: nothing was missing.
	inBaseStruck inBaseGap = iota
	// inBaseSameCurrency: the operation is already in the base currency. Kept
	// apart from inBaseStruck only for readability; apiInBaseGap publishes
	// neither.
	inBaseSameCurrency
	// inBaseUndatedLot: the amount is a transferred basis with at least one
	// piece of unknown purchase date. The one gap that never closes on its own.
	inBaseUndatedLot
	// inBaseNoRateOperationDate: no rate for the day the money moved, nor any
	// earlier day.
	inBaseNoRateOperationDate
	// inBaseNoRateLotDate: no rate for one of the purchase days of a dated
	// breakdown, nor any earlier day (#79).
	inBaseNoRateLotDate
)

// apiInBaseGap maps a gap onto the contract; ok is false for inBaseStruck and
// inBaseSameCurrency, which publish no cause. Only one cause is ever published.
// inBaseUndatedLot is settled before any rate is asked for, so a row with both an
// undated piece and a missing rate reports the one no backfill will fix. The two
// no-rate gaps cannot both arise on one row.
func apiInBaseGap(g inBaseGap) (apitypes.OperationInBaseGap, bool) {
	switch g {
	case inBaseUndatedLot:
		return apitypes.OperationInBaseGapUndatedLot, true
	case inBaseNoRateOperationDate:
		return apitypes.OperationInBaseGapNoRateOperationDate, true
	case inBaseNoRateLotDate:
		return apitypes.OperationInBaseGapNoRateLotDate, true
	default:
		return "", false
	}
}

// rateDate is one date the conversion needs a rate for, with the gap to publish
// if there is none. The gap travels with the date because whoever picks the date
// knows what kind of date it is; working it out again at the failure would be a
// second computation that could name the wrong cause.
type rateDate struct {
	on  time.Time
	gap inBaseGap
}

// datedMinor is one amount in the operation's currency with the date whose rate
// values it: one per ordinary row, one per piece of a transfer's breakdown.
type datedMinor struct {
	minor int64
	date  rateDate
}

// amountTerms splits amount_minor into the dated terms it is a sum of, for
// conversion into another currency, and picks the headline date.
//
// An ordinary row is one term on its own day. A transfer or conversion with a
// breakdown is the basis of shares bought on other days, so each piece is valued
// at its purchase day's rate, as the positions screen does; the transfer day's
// rate would price a 2019 purchase at a 2026 rate. Both legs carry the breakdown
// (see Store.attachTransferLots), so both journals agree.
//
// The headline is the newest term date, the one rate_on can name;
// AssembledFromLots says when it is one of several.
//
// ok is false when any piece is dateless, including a transfer with no breakdown
// at all: converting part, or using the transfer date, would publish an invented
// figure the position refuses to publish. A breakdown that no longer sums to the
// row is an error (checkStoredLots).
func amountTerms(o Operation) (terms []datedMinor, headline rateDate, ok bool, err error) {
	if len(o.TransferLots) == 0 {
		if carriesCostBasis(o) {
			// No breakdown: a basis given by hand or recorded before breakdowns
			// were kept. There is no purchase date behind it, and the lot it created
			// has none either, so ok is false on both legs, unless the basis is zero
			// (see costless). A conversion leg never gets here; Compute refuses an
			// empty breakdown on one.
			if portfolio.DatelessBasis(nil, o.AmountMinor) {
				return nil, rateDate{}, false, nil
			}
			return costless(o)
		}
		// For a trade the money moves on its settlement day when known (Р-3).
		own := rateDate{on: portfolio.RateDay(o), gap: inBaseNoRateOperationDate}
		return []datedMinor{{minor: o.AmountMinor, date: own}}, own, true, nil
	}
	if err := checkStoredLots(o); err != nil {
		return nil, rateDate{}, false, err
	}
	terms = make([]datedMinor, 0, len(o.TransferLots))
	for _, pc := range o.TransferLots {
		if pc.AcquiredOn == nil && !portfolio.DatelessBasis(pc.AcquiredOn, pc.CostMinor) {
			// Bought for nothing: zero at any rate, so no term
			// (see portfolio.DatelessBasis).
			continue
		}
		if pc.AcquiredOn == nil {
			// A piece with no purchase date: the row publishes no base-currency
			// figure at all, matching the position built from the same pieces (see
			// portfolio.Handler.positionInBase).
			return nil, rateDate{}, false, nil
		}
		// A piece is dated by its purchase, so a missing rate here is a
		// purchase-date gap.
		paidOn := *pc.AcquiredOn
		if pc.RateOn != nil {
			paidOn = *pc.RateOn
		}
		bought := rateDate{on: paidOn, gap: inBaseNoRateLotDate}
		terms = append(terms, datedMinor{minor: pc.CostMinor, date: bought})
		if len(terms) == 1 || bought.on.After(headline.on) {
			headline = bought
		}
	}
	if len(terms) == 0 {
		return costless(o)
	}
	return terms, headline, true, nil
}

// costless answers for a parcel bought for nothing: no term, and the
// operation's own date for the fee and rate_on (#226).
func costless(o Operation) ([]datedMinor, rateDate, bool, error) {
	own := rateDate{on: portfolio.RateDay(o), gap: inBaseNoRateOperationDate}
	return nil, own, true, nil
}

// operationInBase converts amount_minor and fee_minor into baseCurrency at the
// rate of the day the money moved, not today's: the journal answers "what did
// this cost then". For a transfer with a breakdown that means each piece at its
// purchase day (see amountTerms), which keeps the row in step with its position.
//
// rate_on is the date of a rate actually used; CBR publishes nothing on weekends
// and holidays, so it is often earlier than dated_on, the date the headline rate
// was asked for (#80).
//
// Amount and fee are converted and rounded separately, half away from zero, as
// marketdata.Converter.Convert does. Several terms are summed as decimals and
// rounded once, as portfolio.Handler.sumInBase does, so the two screens agree to
// the minor unit.
//
// No object (in_base null as a whole) when the row is already in baseCurrency,
// when a needed rate is missing, or when a piece is undated. Each return names
// the object and the gap together, so the caption cannot describe a different
// failure from the one that happened (#79). An error is a genuine failure (DB,
// cancelled context, broken breakdown) and must fail the request; the gap beside
// it means nothing.
func (h *Handler) operationInBase(ctx context.Context, o Operation, baseCurrency string, rates *marketdata.RateMemo) (*apitypes.OperationInBase, inBaseGap, error) {
	if o.Currency == baseCurrency {
		return nil, inBaseSameCurrency, nil
	}
	terms, headline, ok, err := amountTerms(o)
	if err != nil {
		return nil, inBaseStruck, err
	}
	if !ok {
		// Settled before any rate is asked for, so this cause wins over a
		// missing rate (see apiInBaseGap).
		return nil, inBaseUndatedLot, nil
	}

	// The headline rate values the fee and supplies rate_on.
	rl := rates.Rate(ctx, o.Currency, baseCurrency, headline.on)
	if rl.Err != nil {
		if errors.Is(rl.Err, marketdata.ErrNoRate) {
			return nil, headline.gap, nil
		}
		return nil, inBaseStruck, rl.Err
	}

	amount := decimal.Zero
	for _, t := range terms {
		tr := rates.Rate(ctx, o.Currency, baseCurrency, t.date.on)
		if tr.Err != nil {
			if errors.Is(tr.Err, marketdata.ErrNoRate) {
				return nil, t.date.gap, nil
			}
			return nil, inBaseStruck, tr.Err
		}
		amount = amount.Add(decimal.NewFromInt(t.minor).Mul(tr.Rate))
	}

	// Each figure is refused rather than wrapped if it does not fit an int64
	// (#27). That is an error, not a gap: nothing will fix it later.
	amountMinor, err := money.Minor(amount)
	if err != nil {
		return nil, inBaseStruck, fmt.Errorf("%w: amount of operation %s in %s", err, o.ID, baseCurrency)
	}
	feeMinor, err := money.Minor(decimal.NewFromInt(o.FeeMinor).Mul(rl.Rate))
	if err != nil {
		return nil, inBaseStruck, fmt.Errorf("%w: fee of operation %s in %s", err, o.ID, baseCurrency)
	}
	return &apitypes.OperationInBase{
		AmountMinor: amountMinor,
		FeeMinor:    feeMinor,
		Currency:    baseCurrency,
		RateOn:      rl.RateDate.Format("2006-01-02"),
		// The date the headline rate was asked for, beside the one it came
		// from (#80).
		DatedOn: headline.on.Format("2006-01-02"),
	}, inBaseStruck, nil
}

// rateQueries lists every rate the handleListByAccount loop is about to ask
// for, so one RatesOn call fills the memo (#45). Dates come from amountTerms, the
// same function operationInBase uses, so there is no second list to drift. Its
// error is ignored: the loop calls it again and reports it where the figure is
// built. A missed query only costs a round trip, since the memo resolves
// misses, and a wrong one is filed under a key nothing reads.
func rateQueries(ops []Operation, baseCurrency string) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	add := func(currency string, on time.Time) {
		out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: on})
	}
	for _, o := range ops {
		if o.Currency == baseCurrency {
			// operationInBase converts nothing for this row.
			continue
		}
		terms, headline, ok, _ := amountTerms(o)
		if !ok {
			// Undatable or broken: the loop publishes nothing for this row.
			continue
		}
		// Asked for in its own right, as operationInBase does; the memo drops
		// it when it is already a term date.
		add(o.Currency, headline.on)
		for _, t := range terms {
			add(o.Currency, t.date.on)
		}
	}
	return out
}

// parseDate parses a YYYY-MM-DD date; business rules belong to the service.
func parseDate(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("must be YYYY-MM-DD")
	}
	return d, nil
}

// nullableDecimal is the same conversion without a response writer, for
// OperationFromCreateRequest.
func nullableDecimal(n nullable.Nullable[string], field string) (*decimal.Decimal, error) {
	if !n.IsSpecified() || n.IsNull() {
		return nil, nil
	}
	d, err := decimal.NewFromString(n.MustGet())
	if err != nil {
		return nil, BadFieldError{Message: field + " must be a decimal string"}
	}
	return &d, nil
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// parsePage reads limit and offset and refuses anything outside the bounds the
// contract states (see defaultListLimit). The catalog and the importer have their
// own copies on purpose: they share a shape, not the numbers.
func parsePage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit = defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			httpjson.Error(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be a whole number from 1 to %d", maxListLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpjson.Error(w, http.StatusBadRequest, "offset must be a whole number of at least 0")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// parseJournalFilter reads the journal listing's filters; a malformed one is
// a 400 naming it.
func parseJournalFilter(w http.ResponseWriter, r *http.Request) (JournalFilter, bool) {
	q := r.URL.Query()
	var f JournalFilter
	for _, raw := range q["type"] {
		t := Type(raw)
		if !apitypes.OperationType(t).Valid() {
			httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("type %q is not an operation type", raw))
			return JournalFilter{}, false
		}
		f.Types = append(f.Types, t)
	}
	if raw := q.Get("instrument_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "instrument_id must be a uuid")
			return JournalFilter{}, false
		}
		f.InstrumentID = &id
	}
	for _, d := range []struct {
		name string
		into **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if raw := q.Get(d.name); raw != "" {
			day, err := time.Parse(time.DateOnly, raw)
			if err != nil {
				httpjson.Error(w, http.StatusBadRequest, d.name+" must be YYYY-MM-DD")
				return JournalFilter{}, false
			}
			*d.into = &day
		}
	}
	return f, true
}

func pathOperationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid operationId")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateOperationRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}

	op, err := OperationFromCreateRequest(req)
	if err != nil {
		var bad BadFieldError
		if errors.As(err, &bad) {
			httpjson.Error(w, http.StatusBadRequest, bad.Message)
			return
		}
		writeError(w, err)
		return
	}

	created, err := h.svc.Create(r.Context(), p.SpaceID, op)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(created))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	var req apitypes.CreateOperationRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	op, err := OperationFromCreateRequest(req)
	if err != nil {
		var bad BadFieldError
		if errors.As(err, &bad) {
			httpjson.Error(w, http.StatusBadRequest, bad.Message)
			return
		}
		writeError(w, err)
		return
	}
	updated, err := h.svc.Update(r.Context(), p.SpaceID, id, op)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(updated))
}

func (h *Handler) handleListByAccount(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}

	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}

	// hasMore comes from the query: a full page cannot tell whether the
	// journal continues (#86).
	filter, ok := parseJournalFilter(w, r)
	if !ok {
		return
	}
	ops, hasMore, err := h.store.ListByAccount(r.Context(), p.SpaceID, accountID, limit, offset, filter)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	h.writeJournalPage(w, r, p.SpaceID, ops, hasMore)
}

// handleListByInstrument is one paper's rows across every account, a page at a
// time, in the same shape as an account's journal.
func (h *Handler) handleListByInstrument(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	instrumentID, err := uuid.Parse(r.PathValue("instrumentId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid instrumentId")
		return
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	ops, hasMore, err := h.store.ListByInstrument(r.Context(), p.SpaceID, instrumentID, limit, offset)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeJournalPage(w, r, p.SpaceID, ops, hasMore)
}

// writeJournalPage answers a page of rows with what the journal publishes
// beside each: the other account of a move, and the figure in the base
// currency or why there is none.
func (h *Handler) writeJournalPage(w http.ResponseWriter, r *http.Request, spaceID uuid.UUID, ops []Operation, hasMore bool) {
	sp, err := h.spaces.SpaceByID(r.Context(), spaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	// Per request only.
	rates := marketdata.NewRateMemo(h.conv)

	// A warm-up only: the loop resolves whatever it misses.
	rates.Prefetch(r.Context(), rateQueries(ops, sp.BaseCurrency))

	ids := make([]uuid.UUID, 0, len(ops))
	for _, o := range ops {
		if o.TransferGroupID != nil {
			ids = append(ids, o.ID)
		}
	}
	counterparts, err := h.store.CounterpartAccounts(r.Context(), spaceID, ids)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	stated, err := h.svc.StatedBasisChanges(r.Context(), spaceID, ops)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	page := make([]apitypes.Operation, 0, len(ops))
	for _, o := range ops {
		api := toAPI(o)
		api.CounterpartAccountId = nullable.NewNullNullable[uuid.UUID]()
		if account, ok := counterparts[o.ID]; ok {
			api.CounterpartAccountId = nullable.NewNullableWithValue(account)
		}
		api.StatedBasisChangeMinor = nullable.NewNullNullable[int64]()
		if change, ok := stated[o.ID]; ok {
			api.StatedBasisChangeMinor = nullable.NewNullableWithValue(change)
		}
		inBase, gap, err := h.operationInBase(r.Context(), o, sp.BaseCurrency, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if inBase != nil {
			api.InBase = nullable.NewNullableWithValue(*inBase)
		} else {
			api.InBase = nullable.NewNullNullable[apitypes.OperationInBase]()
		}
		// A cause is published only when operationInBase reports one.
		if apiGap, missing := apiInBaseGap(gap); missing {
			api.InBaseGap = nullable.NewNullableWithValue(apiGap)
		} else {
			api.InBaseGap = nullable.NewNullNullable[apitypes.OperationInBaseGap]()
		}
		page = append(page, api)
	}
	httpjson.Write(w, http.StatusOK, apitypes.OperationsResponse{Operations: page, HasMore: hasMore})
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p.SpaceID, id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleStatePurchases(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	var req apitypes.StatePurchasesRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	stated, err := statedPurchases(req.Purchases)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	op, err := h.svc.StatePurchases(r.Context(), p.SpaceID, id, stated)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(op))
}

// statedPurchases reads the purchases of a request. A malformed field is named
// by the purchase it is in.
func statedPurchases(in []apitypes.StatedPurchase) ([]StatedPurchase, error) {
	out := make([]StatedPurchase, 0, len(in))
	for i, sp := range in {
		quantity, err := decimal.NewFromString(sp.Quantity)
		if err != nil {
			return nil, fmt.Errorf("purchase %d: quantity must be a decimal string", i+1)
		}
		one := StatedPurchase{Quantity: quantity}
		if sp.Price.IsSpecified() && !sp.Price.IsNull() {
			price, err := decimal.NewFromString(sp.Price.MustGet())
			if err != nil {
				return nil, fmt.Errorf("purchase %d: price must be a decimal string", i+1)
			}
			one.Price = &price
		}
		if sp.CostMinor.IsSpecified() && !sp.CostMinor.IsNull() {
			cost := sp.CostMinor.MustGet()
			one.CostMinor = &cost
		}
		if sp.FeeMinor != nil {
			one.FeeMinor = *sp.FeeMinor
		}
		if sp.AcquiredOn.IsSpecified() && !sp.AcquiredOn.IsNull() {
			on, err := parseDate(sp.AcquiredOn.MustGet())
			if err != nil {
				return nil, fmt.Errorf("purchase %d: acquired_on %v", i+1, err)
			}
			one.AcquiredOn = &on
		}
		out = append(out, one)
	}
	return out, nil
}

func (h *Handler) handleCreateArrival(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateArrivalRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}
	quantity, err := decimal.NewFromString(req.Quantity)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "quantity must be a decimal string")
		return
	}
	var purchases []StatedPurchase
	if req.Purchases != nil {
		if purchases, err = statedPurchases(*req.Purchases); err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	op, err := h.svc.CreateArrival(r.Context(), p.SpaceID, ArrivalParams{
		AccountID: req.AccountId, InstrumentID: req.InstrumentId, OccurredOn: occurredOn,
		Quantity: quantity, Currency: req.Currency, Note: note, Purchases: purchases,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(op))
}

func (h *Handler) handleListArrivals(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return
	}
	instrumentID, err := uuid.Parse(r.PathValue("instrumentId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid instrumentId")
		return
	}
	arrivals, err := h.svc.Arrivals(r.Context(), p.SpaceID, accountID, instrumentID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]apitypes.Arrival, 0, len(arrivals))
	for _, a := range arrivals {
		o := a.Operation
		purchases := make([]apitypes.ArrivalPurchase, 0, len(o.TransferLots))
		for _, pc := range o.TransferLots {
			bought := nullable.NewNullNullable[string]()
			if pc.AcquiredOn != nil {
				bought = nullable.NewNullableWithValue(pc.AcquiredOn.Format("2006-01-02"))
			}
			purchases = append(purchases, apitypes.ArrivalPurchase{
				Quantity: pc.Quantity.String(), CostMinor: pc.CostMinor, AcquiredOn: bought,
			})
		}
		from := nullable.NewNullNullable[uuid.UUID]()
		if a.FromAccountID != nil {
			from = nullable.NewNullableWithValue(*a.FromAccountID)
		}
		quantity := ""
		if o.Quantity != nil {
			quantity = o.Quantity.String()
		}
		out = append(out, apitypes.Arrival{
			OperationId: o.ID, OccurredOn: o.OccurredOn.Format("2006-01-02"), Quantity: quantity,
			Currency: o.Currency, CostMinor: o.AmountMinor, Source: o.Source, Note: o.Note,
			FromAnotherBroker: a.FromAnotherBroker, FromAccountId: from, Purchases: purchases,
		})
	}
	httpjson.Write(w, http.StatusOK, apitypes.ArrivalsResponse{Arrivals: out})
}

func (h *Handler) handleTransfer(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.TransferRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}

	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}
	quantity, err := decimal.NewFromString(req.Quantity)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "quantity must be a decimal string")
		return
	}

	var costOverride *int64
	if req.CostMinor.IsSpecified() && !req.CostMinor.IsNull() {
		v := req.CostMinor.MustGet()
		costOverride = &v
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	out, in, err := h.svc.CreateTransfer(r.Context(), p.SpaceID, TransferParams{
		FromAccountID:     req.FromAccountId,
		ToAccountID:       req.ToAccountId,
		InstrumentID:      req.InstrumentId,
		Quantity:          quantity,
		OccurredOn:        occurredOn,
		CostMinorOverride: costOverride,
		Note:              note,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, apitypes.TransferResponse{Out: toAPI(out), In: toAPI(in)})
}

func (h *Handler) handleMoneyTransfer(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.MoneyTransferRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}
	params := MoneyTransferParams{
		FromAccountID: req.FromAccountId, ToAccountID: req.ToAccountId, OccurredOn: occurredOn,
		AmountMinor: req.AmountMinor, Currency: req.Currency,
	}
	if req.Note != nil {
		params.Note = *req.Note
	}
	minor, minorErr := req.ReceivedMinor.Get()
	currency, currencyErr := req.ReceivedCurrency.Get()
	switch {
	case minorErr == nil && currencyErr == nil:
		params.Received = &Money{Minor: minor, Currency: currency}
	case minorErr == nil || currencyErr == nil:
		httpjson.Error(w, http.StatusBadRequest, "received_minor and received_currency are sent together or not at all")
		return
	}
	out, in, err := h.svc.CreateMoneyTransfer(r.Context(), p.SpaceID, params)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, apitypes.TransferResponse{Out: toAPI(out), In: toAPI(in)})
}
