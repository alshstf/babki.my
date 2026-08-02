package operation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	"babki.my/babki/internal/portfolio"
)

// defaultListLimit/maxListLimit bound GET .../operations. A limit above
// maxListLimit is clamped rather than rejected with 400, matching the
// instrument catalog search: pagination is a best-effort detail, not
// something a client should get an error for overshooting.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// spaceStore is the subset of family.Store this handler needs: reading the
// space's base currency to convert each journal entry into it (see
// operationInBase). Local interface (identical to account's and portfolio's
// spaceStore) so tests can inject a fake or a real *family.Store
// interchangeably — Go interface assignability is structural, so
// *family.Store satisfies this with no conversion needed at the call site.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// converter is the subset of *marketdata.Converter this handler needs: Rate,
// which resolves a currency pair's rate on a given DATE and reports the date
// the rate actually came from. Local interface (mirroring account's and
// portfolio's identically named ones) so tests can inject a double whose
// lookups fail with a genuine error rather than marketdata.ErrNoRate — a
// real, DB-backed Converter cannot be made to do that on demand, and the
// handler must treat the two completely differently (see operationInBase).
// *marketdata.Converter satisfies this structurally, no call site changes.
type converter interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// Handler exposes the operations journal (and transfers) over HTTP.
type Handler struct {
	svc    *Service
	store  *Store
	spaces spaceStore
	conv   converter
	auth   *family.Auth
	sm     *scs.SessionManager
}

func NewHandler(svc *Service, store *Store, spaces spaceStore, conv converter, auth *family.Auth, sm *scs.SessionManager) *Handler {
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
	srv.Mount("DELETE /api/v1/operations/{operationId}", edit(h.handleDelete))
	srv.Mount("POST /api/v1/operations/transfer", edit(h.handleTransfer))
}

// writeError maps operation-specific errors to HTTP responses, falling back
// to family.WriteError for everything else. ErrInconsistent gets its own
// branch (409, with the engine's explanation in the body) since it isn't one
// of the family package's sentinel errors.
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInconsistent) {
		httpjson.Error(w, http.StatusConflict, err.Error())
		return
	}
	family.WriteError(w, err)
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
		Source:      o.Source,
		CreatedAt:   o.CreatedAt,
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

// rateKey identifies one memoized fx rate lookup. Unlike account's cache —
// which converts everything at today's rate and can therefore key by
// currency alone — the journal resolves a rate per operation DATE, so the
// date has to be part of the key. A single page of the journal routinely
// holds the same currency on a dozen different dates with a dozen different
// rates; keying by currency alone would silently reuse the first operation's
// rate for all of them, producing wrong numbers that look entirely plausible
// on screen. portfolio's cache (portfolio.rateKey) is keyed the same way and
// for the same reason: its per-lot and per-income-operation conversions are,
// like the journal's, valued at each item's own date rather than one rate
// for everything — only account still converts everything at today's rate.
//
// The date is held as its YYYY-MM-DD string rather than a time.Time so the
// key is a value comparison on the calendar date itself, immune to two
// otherwise-equal time.Time values differing in monotonic clock reading or
// *time.Location pointer (which would merely cost extra lookups, but would
// do so invisibly).
type rateKey struct {
	currency string
	on       string
}

// rateLookup memoizes one (currency, date) pair's resolved fx rate — the
// rate itself, the date it actually came from, and the resolution error — so
// handleListByAccount's per-operation conversion loop hits the fx rate store
// at most once per distinct pair, not once per operation. Mirrors account's
// and portfolio's identically named type; only the cache key differs (see
// rateKey).
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// rateFor resolves currency->baseCurrency on date on, memoized in cache for
// the rest of the request (see rateKey). The resolution error rides along in
// the returned rateLookup rather than being returned, because callers have to
// tell marketdata.ErrNoRate — an expected outcome that nulls in_base — apart
// from a genuine failure, which fails the request. Mirrors portfolio's
// identically named method.
func (h *Handler) rateFor(ctx context.Context, currency, baseCurrency string, on time.Time, cache map[rateKey]*rateLookup) *rateLookup {
	key := rateKey{currency: currency, on: on.Format("2006-01-02")}
	rl, ok := cache[key]
	if !ok {
		rate, date, err := h.conv.Rate(ctx, currency, baseCurrency, on)
		rl = &rateLookup{rate: rate, date: date, err: err}
		cache[key] = rl
	}
	return rl
}

// datedMinor is one amount denominated in the operation's own currency,
// together with the date whose fx rate values it. An ordinary operation is a
// single such amount dated on the day it happened; a transfer carrying a
// breakdown is one per piece it moved (see operationInBase). Mirrors
// portfolio's identically named type, which splits a position's basis the same
// way and for the same reason.
type datedMinor struct {
	minor int64
	on    time.Time
}

// amountTerms decides what an operation's amount_minor is a sum OF, for the
// purpose of expressing it in another currency, and which single date best
// describes the result.
//
// Almost always the answer is trivial: the amount is one figure and it belongs
// to the day the operation happened. A transfer that carries a FIFO breakdown
// is the exception, and not a cosmetic one. Its amount_minor is not money that
// moved on the transfer date at all — it is the cost basis of shares bought on
// other days, carried across to another account of the same family, and the
// breakdown is precisely the record of which days those were. Valuing that
// number at the transfer day's rate prices a 2019 purchase at a 2026 rate and
// makes the journal print, for the same shares, a figure the positions screen
// contradicts (the positions screen having converted each lot at its own
// purchase date's rate all along — see portfolio.Handler.positionInBase).
//
// It applies to BOTH legs of a transfer pair. The breakdown is stored once,
// next to the arriving leg, but it is read onto the departing one as well (see
// Store.attachTransferLots), because both legs record one parcel: the same
// shares, the same basis, the same purchases behind it. While only the arriving
// leg carried it, the source account's journal was the last place in the system
// still valuing that basis at the rate of the day the shares changed brokers —
// the very figure README.md and the seed call an invented one — one screen away
// from a destination journal and a destination position that both said
// something else about the same shares.
//
// The headline date is the newest date among the terms: the single date
// rate_on can name. For an ordinary operation that is its own date, exactly as
// before. For a transfer it is the most recent purchase in the parcel — one of
// the several rates behind the figure rather than a rate that had no part in
// it. No single date can describe a sum struck at several, and the API
// contract says so; the alternative, publishing the transfer day, would name
// the one rate deliberately NOT used. Because such a date reads exactly like an
// ordinary rate_on and means something else, the published object says which of
// the two it is (assembled_from_lots — see operationInBase) instead of leaving
// a reader to infer it from a date comparison that cannot answer the question.
//
// Transfers without a breakdown are unchanged: a basis typed in by hand has no
// purchase dates behind it, and one recorded before breakdowns were kept has
// lost them. There is one figure and one date on the row, so the row's own date
// is what values it — on both legs alike, since neither has anything else.
// That is not a claim about when the shares were bought; it is the only date
// the operation contains. (The POSITION such a transfer creates makes the
// opposite choice and holds no date at all, because a lot's date IS a claim
// about a purchase — see portfolio.Lot.AcquiredOn.)
//
// A breakdown WITH a dateless piece is the third case, and the one this
// function cannot answer at all: ok is false and the caller publishes nothing.
// It arises when a parcel contains shares that arrived by an earlier
// date-less transfer and are then moved on again, so some pieces name a
// purchase day and others cannot. Converting the datable pieces alone would
// understate the basis; converting the rest at the transfer's date would
// reintroduce exactly the invented figure this mechanism removed. See
// amountTerms' body for why the whole row goes null instead.
//
// A breakdown that has drifted from the sum it must equal is refused before
// it becomes a ruble figure at all, via portfolio.CheckTransferLots — the
// same check portfolio.Compute already runs on the arriving leg while
// folding the journal into positions (see its TypeTransferIn branch),
// reused rather than re-implemented, because "does this breakdown describe
// the operation carrying it" is one invariant regardless of who is asking.
// It is safe to reuse unmodified for the departing leg too:
// Service.CreateTransfer writes Quantity, AmountMinor and OccurredOn
// identically on both legs of a pair, and Store.attachTransferLots reads the
// very same stored pieces onto both (see its doc), so a departing operation
// and its arriving twin are, as far as this check can tell, indistinguishable
// from one another. Before this, the engine's own check covered only the
// arriving leg's POSITION — a different endpoint answering a different
// question — and this journal path had no check of its own at all, on
// either leg: a breakdown that ever drifted from its sum would fail loudly
// on the receiver's positions screen while silently misleading every reader
// of the journal, on both accounts, forever.
func amountTerms(o Operation) (terms []datedMinor, headline time.Time, ok bool, err error) {
	if len(o.TransferLots) == 0 {
		return []datedMinor{{minor: o.AmountMinor, on: o.OccurredOn}}, o.OccurredOn, true, nil
	}
	if err := portfolio.CheckTransferLots(o); err != nil {
		return nil, time.Time{}, false, err
	}
	terms = make([]datedMinor, 0, len(o.TransferLots))
	for _, pc := range o.TransferLots {
		if pc.AcquiredOn == nil {
			// One piece of this parcel does not know when it was bought (see
			// portfolio.Lot.AcquiredOn): the shares behind it arrived by an
			// earlier transfer that carried no dates, and this move cannot
			// invent what that one already lacked.
			//
			// ok is false, so the row publishes no base-currency figure at all
			// — not the transfer date's rate applied to the undatable piece,
			// and not a total assembled from the pieces that happen to have
			// dates. Both would print a ruble amount that looks exactly like
			// the correct ones on the rows above it. The position built from
			// these same pieces goes null for the identical reason (see
			// portfolio.Handler.positionInBase), so the two screens agree about
			// what they do not know.
			return nil, time.Time{}, false, nil
		}
		terms = append(terms, datedMinor{minor: pc.CostMinor, on: *pc.AcquiredOn})
		if len(terms) == 1 || pc.AcquiredOn.After(headline) {
			headline = *pc.AcquiredOn
		}
	}
	return terms, headline, true, nil
}

// operationInBase converts an operation's amount_minor and fee_minor from
// its own currency into baseCurrency at the fx rate in effect ON THE DAY THE
// OPERATION HAPPENED — not today's rate. This is the deliberate difference
// from account.balanceInBase, which converts at time.Now(): that answers
// "what is this worth now", the journal answers "what did this cost then". A
// 2019 purchase must keep being reported at its 2019 rate however the ruble
// moves afterwards.
//
// "The day the operation happened" is the day its money moved, which for a
// transfer carrying a breakdown is not the day it is dated: that amount is a
// basis assembled on other days, and each piece is converted at the rate of
// the day it was bought. See amountTerms — that is the whole of the
// difference, and it is what makes this row agree with the position built from
// the very same pieces.
//
// portfolio.positionInBase now sits between the two rather than matching
// either: its cost_minor and income_minor are historical exactly like this
// function (each lot and each income operation at its own date's rate), but
// its market_value_minor still converts at time.Now() like
// account.balanceInBase, because a position — unlike a plain journal entry —
// has an ongoing market value, and "what is it worth now" is a real question
// for it. unrealized_pnl_minor is the difference between that today-valued
// figure and the historical basis, so the position's own currency's
// appreciation or depreciation since each purchase ends up baked into the
// base-currency profit.
//
// rate_on is the date of a rate ACTUALLY used, which is o.OccurredOn only
// when a rate exists for that exact day. The CBR publishes nothing on
// weekends and holidays, so Store.FxRateOn resolves the nearest EARLIER date
// and Rate reports it back here; publishing o.OccurredOn instead would claim
// a rate that never existed, and the journal's tooltip reads this field
// verbatim. For a transfer converted piece by piece it is the newest of the
// several rates that make up the figure — see amountTerms.
//
// The amount and the fee are converted and rounded independently
// (decimal.Mul(rate).Round(0), the exact step marketdata.Converter.Convert
// itself uses, so the result matches a per-amount Convert call bit for bit).
// The fee is a figure of its own, not a term of the amount: converting their
// sum and splitting it afterwards would round differently. Rounding is
// half-away-from-zero, so the sign is preserved and a purchase's negative
// amount never shrinks in magnitude. Within the amount, when it has several
// terms, every term is multiplied as a decimal and only the total is rounded,
// once — the same single final rounding, and the same rule
// portfolio.Handler.sumInBase follows for a position's basis, so the two
// figures land on the same minor unit rather than a unit apart. (The fee
// follows rate_on's rate; a transfer carries no broker fee, so this term is in
// practice zero times whatever rate names it.)
//
// It returns (nil, nil) — render in_base as null, the WHOLE object, never
// partially populated — in exactly three cases: the operation is already
// denominated in baseCurrency (nothing to convert), no rate could be resolved
// for one of the dates it needs nor any earlier one (marketdata.ErrNoRate), or
// the amount cannot be split into dated terms at all because a piece of the
// parcel does not know when it was bought (see amountTerms). An operation with
// a converted amount but an unconverted fee, or a basis summed from only the
// pieces that happened to convert, would be worse than an honest "can't
// convert this one".
//
// A non-nil error means a genuine failure — a DB error, a canceled context,
// or (see amountTerms) a transfer's stored breakdown no longer summing to
// the operation it describes — that the caller must surface as a request
// error, never silently rendered as null or, worse, as a wrong number built
// from a broken breakdown. An outage read as "no rate that far back" and
// corrupted data read as a plausible ruble figure are the same category of
// mistake: both hide a genuine failure behind an answer that looks routine.
func (h *Handler) operationInBase(ctx context.Context, o Operation, baseCurrency string, cache map[rateKey]*rateLookup) (*apitypes.OperationInBase, error) {
	if o.Currency == baseCurrency {
		return nil, nil
	}
	terms, headline, ok, err := amountTerms(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The amount cannot be broken into terms that all carry a date, so
		// there is no honest set of rates to strike it at (see amountTerms).
		return nil, nil
	}

	// The headline rate values the fee and supplies rate_on, so without it
	// there is no object to publish at all.
	rl := h.rateFor(ctx, o.Currency, baseCurrency, headline, cache)
	if rl.err != nil {
		if errors.Is(rl.err, marketdata.ErrNoRate) {
			return nil, nil
		}
		return nil, rl.err
	}

	amount := decimal.Zero
	for _, t := range terms {
		tr := h.rateFor(ctx, o.Currency, baseCurrency, t.on, cache)
		if tr.err != nil {
			if errors.Is(tr.err, marketdata.ErrNoRate) {
				return nil, nil
			}
			return nil, tr.err
		}
		amount = amount.Add(decimal.NewFromInt(t.minor).Mul(tr.rate))
	}

	return &apitypes.OperationInBase{
		AmountMinor: amount.Round(0).IntPart(),
		FeeMinor:    decimal.NewFromInt(o.FeeMinor).Mul(rl.rate).Round(0).IntPart(),
		Currency:    baseCurrency,
		RateOn:      rl.date.Format("2006-01-02"),
		// Whether rate_on is THE rate behind the figure or merely the newest of
		// several. A reader cannot work this out from the dates: rate_on equal
		// to occurred_on normally means "converted on the operation's own day",
		// and different from it means "no rate that day, so the nearest earlier
		// one" — but for a basis assembled from purchases both readings are
		// false, and either can happen to be the case a date comparison lands
		// on. The screen that reads this field out loud (the journal's amount
		// tooltip) would then state something untrue about a perfectly correct
		// number, which is worse than saying nothing.
		AssembledFromLots: len(o.TransferLots) > 0,
	}, nil
}

// parseDate parses a YYYY-MM-DD date, matching account.parseAsOf's format.
// Business-rule checks (e.g. "not in the future") are left to the service,
// which already enforces them when replaying the journal.
func parseDate(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("must be YYYY-MM-DD")
	}
	return d, nil
}

// parseNullableDecimal converts an optional decimal-as-string field. An
// absent or explicit-null field yields (nil, true). A present-but-garbage
// string writes a 400 response and returns ok=false so the caller can bail
// out before ever reaching the service.
func parseNullableDecimal(w http.ResponseWriter, n nullable.Nullable[string], field string) (*decimal.Decimal, bool) {
	if !n.IsSpecified() || n.IsNull() {
		return nil, true
	}
	d, err := decimal.NewFromString(n.MustGet())
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, field+" must be a decimal string")
		return nil, false
	}
	return &d, true
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
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

	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}

	var settledOn *time.Time
	if req.SettledOn.IsSpecified() && !req.SettledOn.IsNull() {
		t, err := parseDate(req.SettledOn.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "settled_on "+err.Error())
			return
		}
		settledOn = &t
	}

	quantity, ok := parseNullableDecimal(w, req.Quantity, "quantity")
	if !ok {
		return
	}
	price, ok := parseNullableDecimal(w, req.Price, "price")
	if !ok {
		return
	}
	splitRatio, ok := parseNullableDecimal(w, req.SplitRatio, "split_ratio")
	if !ok {
		return
	}

	var instrumentID *uuid.UUID
	if req.InstrumentId.IsSpecified() && !req.InstrumentId.IsNull() {
		v := req.InstrumentId.MustGet()
		instrumentID = &v
	}

	feeMinor := int64(0)
	if req.FeeMinor != nil {
		feeMinor = *req.FeeMinor
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	op := Operation{
		AccountID:    req.AccountId,
		InstrumentID: instrumentID,
		Type:         Type(req.Type),
		OccurredOn:   occurredOn,
		SettledOn:    settledOn,
		Quantity:     quantity,
		Price:        price,
		AmountMinor:  req.AmountMinor,
		Currency:     req.Currency,
		FeeMinor:     feeMinor,
		Note:         note,
		SplitRatio:   splitRatio,
	}

	created, err := h.svc.Create(r.Context(), p.SpaceID, op)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(created))
}

func (h *Handler) handleListByAccount(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}

	limit := defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}

	ops, err := h.store.ListByAccount(r.Context(), p.SpaceID, accountID, limit, offset)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	// Scoped to this request only: see operationInBase/rateKey.
	rates := make(map[rateKey]*rateLookup)
	out := make([]apitypes.Operation, 0, len(ops))
	for _, o := range ops {
		api := toAPI(o)
		inBase, err := h.operationInBase(r.Context(), o, sp.BaseCurrency, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if inBase != nil {
			api.InBase = nullable.NewNullableWithValue(*inBase)
		} else {
			api.InBase = nullable.NewNullNullable[apitypes.OperationInBase]()
		}
		out = append(out, api)
	}
	httpjson.Write(w, http.StatusOK, out)
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
