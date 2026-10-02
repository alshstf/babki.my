package portfolio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

// journalStore is the subset of operation.Store this handler needs. It is a
// local interface rather than a direct dependency on package operation
// because package operation imports this package (portfolio.Operation and
// portfolio.Compute back the journal-consistency checks in
// operation.Service) — importing operation.Store here would create an
// import cycle. operation.Operation is a type alias for portfolio.Operation
// (see operation/operation.go), so *operation.Store satisfies this
// interface structurally with no conversion needed at the call site.
type journalStore interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error)
}

// quoteStore is the subset of marketdata.Store this handler needs. Unlike
// journalStore, there's no import cycle forcing this into a local interface
// (package marketdata does not import portfolio) — it's local anyway so
// tests can inject a fake in place of a real Postgres-backed Store, both to
// control which instruments have quotes and to assert LatestQuotes is
// called once per request (a single batched round trip), never once per
// position.
type quoteStore interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// instrumentStore is the subset of *instrument.Store this handler needs: the
// catalog rows behind a whole list of positions, read in one round trip rather
// than one per position (see instrument.Store.ByIDs). Local interface for the
// same reasons quoteStore is one — a fake can count the round trips, and can
// produce the missing-row case a foreign key otherwise makes unreachable — and
// *instrument.Store satisfies it structurally, so nothing changes at the call
// site.
type instrumentStore interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// converter is the subset of *marketdata.Converter this handler needs, in the
// two shapes it needs it.
//
// RatesOn resolves every rate the whole screen is about to want in a single
// round trip, and its answers are filed in the request's memo before the
// per-position loop starts (see prewarmRates). Rate resolves one pair on one
// date, and stays because the memo has to be able to answer anything the
// prefetch did not think to ask for: each lot and each income operation is
// valued at the rate of its own date, and a market valuation at the rate of
// today, so one request wants many rates and the enumeration that predicts
// them is an optimization, never a precondition (see rateQueries and rateKey).
//
// Convert is deliberately absent although *marketdata.Converter has it: every
// conversion here now resolves its rate through the memo and applies it
// itself (see rateLookup.applyTo), which is Convert's own arithmetic, so
// calling it would be the one lookup on the page that could not be shared.
//
// Local interface (mirroring journalStore/quoteStore) so tests can inject a
// fake in place of a real *marketdata.Converter to control exactly which
// currency pairs have a resolvable rate, including forcing
// marketdata.ErrNoRate — *marketdata.Converter satisfies this structurally, no
// conversion needed at the call site.
type converter interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// spaceStore is the subset of family.Store this handler needs: reading the
// space's base currency to convert positions into it (see positionInBase).
// Local interface (mirroring journalStore/quoteStore, and identical to
// account's spaceStore) so tests can inject a fake or a real *family.Store
// interchangeably — Go interface assignability is structural, so
// *family.Store satisfies this with no conversion needed at the call site.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Handler exposes computed account positions over HTTP.
type Handler struct {
	ops         journalStore
	instruments instrumentStore
	quotes      quoteStore
	conv        converter
	spaces      spaceStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments instrumentStore, quotes quoteStore, conv converter, spaces spaceStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{ops: ops, instruments: instruments, quotes: quotes, conv: conv, spaces: spaces, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	srv.Mount("GET /api/v1/accounts/{accountId}/positions", view(h.handleList))
	srv.Mount("GET /api/v1/accounts/{accountId}/return", view(h.handleReturn))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/holdings", view(h.handleHoldings))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/prices", view(h.handlePrices))
	srv.Mount("GET /api/v1/instruments/{instrumentId}/return", view(h.handlePaperReturn))
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("POST /api/v1/instruments/{instrumentId}/prices", edit(h.handleStatePrice))
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// handleList computes an account's positions by replaying its full
// operations journal through Compute. Closed positions (quantity zero) are
// included: realized P&L and income on them remain meaningful history.
// Results are sorted by instrument name for a stable, human-friendly order.
//
// Known MVP behavior: like GET .../operations, the account is looked up by
// (space_id, account_id) only, with no separate existence/ownership check.
// A wrong or nonexistent accountId therefore yields an empty journal and an
// empty position list — the same 200 response as an account with no
// activity — rather than a 404. This is single-tenant software, so no data
// ever crosses a space boundary; the caller just can't distinguish "no
// positions" from "not your account".
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	resp, _, err := h.positionsResponse(r.Context(), p.SpaceID, accountID)
	var notComputed journalDoesNotCompute
	switch {
	case errors.As(err, &notComputed):
		// Practically unreachable: every write to the journal already passes
		// through this same engine, so a stored journal that fails to replay
		// here would mean the data is already corrupted.
		httpjson.Error(w, http.StatusUnprocessableEntity, notComputed.Error())
	case errors.Is(err, errInstrumentNotInCatalog):
		httpjson.Error(w, http.StatusNotFound, "not found")
	case err != nil:
		family.WriteError(w, err)
	default:
		httpjson.Write(w, http.StatusOK, resp)
	}
}

// journalDoesNotCompute is the engine refusing a stored journal, carried with
// its own words (see handleList).
type journalDoesNotCompute struct{ err error }

func (e journalDoesNotCompute) Error() string { return e.err.Error() }
func (e journalDoesNotCompute) Unwrap() error { return e.err }

// errInstrumentNotInCatalog is a journal naming a paper the catalog has no row
// for — unreachable behind the foreign key, and answered rather than skipped.
var errInstrumentNotInCatalog = errors.New("portfolio: the journal names an instrument the catalog does not hold")

// positionsResponse is the whole of what the positions screen shows for one
// account, and the number of operations behind it. It is the one place an
// account is valued: the screen writes it out, and the family total reads the
// account's worth from it (see ValueFromJournal).
func (h *Handler) positionsResponse(ctx context.Context, spaceID, accountID uuid.UUID) (apitypes.PositionsResponse, int, error) {
	sp, err := h.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}

	ops, err := h.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}

	positions, err := Compute(ops)
	if err != nil {
		// Practically unreachable: every write to the journal already passes
		// through this same engine (operation.Service.Create/Delete replay
		// the journal to check consistency before committing), so a stored
		// journal that fails to replay here would mean the data is already
		// corrupted rather than a normal request-time error.
		return apitypes.PositionsResponse{}, 0, journalDoesNotCompute{err}
	}

	instrumentIDs := make([]uuid.UUID, 0, len(positions))
	for id := range positions {
		instrumentIDs = append(instrumentIDs, id)
	}
	// One batched round trip for every position's catalog row and one for
	// every position's quote, never one per position (N+1) — see
	// instrumentStore and quoteStore.
	instruments, err := h.instruments.ByIDs(ctx, instrumentIDs)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	quotes, err := h.quotes.LatestQuotes(ctx, instrumentIDs)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}

	// One reading of "today" for the whole request: the market valuations, the
	// base-currency figures struck from them and the prefetch that resolves
	// both must name the same calendar day, even for the one request a year
	// that starts before UTC midnight and finishes after it.
	now := time.Now().UTC()

	// Both scoped to this request only: see positionInBase/rateKey and
	// incomeByInstrument. The journal is already in hand, so grouping its
	// income entries costs one pass and no extra round trip.
	rates := make(map[rateKey]*rateLookup)
	income := incomeByInstrument(ops)

	// One round trip for every rate the loop below is about to want. It is a
	// cache warm-up and nothing more: what it misses, and everything it
	// resolves if it fails outright, the loop resolves for itself (see
	// rateQueries, prewarmRates and rateFor).
	// The money the account holds, folded from the same journal by the same pure
	// function the positions come from. Built BEFORE the warm-up so its rates
	// are asked for in the same batch as everything else's.
	cashPositions, err := Cash(ops)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	h.prewarmRates(ctx, rateQueries(positions, instruments, quotes, income, cashPositions, sp.BaseCurrency, now), rates)

	totals := newRealizedTotals(sp.BaseCurrency)
	account := newAccountTotals(sp.BaseCurrency)
	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, ok := instruments[pos.InstrumentID]
		if !ok {
			// The journal names an instrument the catalog has no row for. A
			// foreign key (operations.instrument_id, ON DELETE RESTRICT) makes
			// this unreachable, and it is answered anyway rather than skipped:
			// a skipped position is a holding missing from the portfolio with
			// every total quietly smaller and nothing on screen saying so. The
			// same 404 the one-at-a-time read published through
			// family.WriteError(pgx.ErrNoRows) before it was batched.
			return apitypes.PositionsResponse{}, 0, errInstrumentNotInCatalog
		}
		apiPos, err := h.toAPI(ctx, pos, inst, quotes, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		// Struck once and used twice: it is this position's
		// in_base.realized_pnl_minor below and one term of the account's total
		// beside it. The total takes it even when the in_base object turns out
		// to be absent — a settled result is not made unknowable by a missing
		// quote or by a lot still held whose purchase date nobody wrote down.
		realizedMinor, realizedGap, err := h.realizedInBase(ctx, pos, sp.BaseCurrency, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		if err := totals.add(apiPos.Currency, apiPos.RealizedPnlMinor, realizedMinor, realizedGap, soldUnknownCost(pos)); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		inBase, gap, err := h.positionInBase(ctx, pos, apiPos, income[pos.InstrumentID], sp.BaseCurrency, realizedMinor, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		// THE GAP DECIDES WHETHER THERE IS AN OBJECT TO PUBLISH, not a second
		// look at the pointer. positionInBase names both in one statement, and
		// this reads the naming: a cause is published only with no object beside
		// it, and an object only with no cause. Should the two ever be returned
		// together by mistake, this drops the object and keeps the honest
		// caption — the wrong way round would put a converted figure under a
		// sentence saying it could not be converted, which is the one outcome
		// worse than today's vague-but-true phrase.
		if apiGap, missing := apiInBaseGap(gap); missing {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			apiPos.InBaseGap = nullable.NewNullableWithValue(apiGap)
		} else {
			apiPos.InBaseGap = nullable.NewNullNullable[apitypes.InBaseGap]()
			// No gap covers two answers — the object was struck, or the position
			// is already in the base currency and there was nothing to convert
			// — and the pointer is what tells them apart. Both publish a null
			// gap: nothing is missing in either, and a client separates them by
			// comparing the position's currency with the base one.
			if inBase != nil {
				apiPos.InBase = nullable.NewNullableWithValue(*inBase)
			} else {
				apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			}
		}

		// Folded in AFTER the in_base object is decided, because the account's
		// total reads that object: it is the one place the row's converted
		// figures exist, and re-striking them here would be the second
		// computation of one value this package keeps warning about.
		if err := account.addPosition(apiPos, inBase, gap, realizedGap); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	// THE MONEY, beside the papers. Valued here rather than in the fold for the
	// reason every rate on this page is applied here: the calculating core holds
	// none.
	cash := make([]apitypes.CashPosition, 0, len(cashPositions))
	for _, p := range CashByCurrency(cashPositions) {
		one, err := h.cashToAPI(ctx, p, sp.BaseCurrency, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		cash = append(cash, one)
		if err := account.addCash(one); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
	}

	// The account's own charges, each converted at the rate of the day it was
	// charged — the same rule every other past event on this screen follows
	// (НК РФ ст. 210 п. 5 for the money that is tax, and plain consistency for
	// the rest: a commission taken in 2019 was that many rubles in 2019).
	for _, o := range accountCharges(ops) {
		minor, err := money.Sub(o.AmountMinor, o.FeeMinor)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, fmt.Errorf("%w: a charge of %d less its own fee of %d", err, o.AmountMinor, o.FeeMinor)
		}
		base := nullable.NewNullableWithValue(minor)
		if o.Currency != sp.BaseCurrency {
			converted, ok, err := h.sumInBase(ctx, []datedMinor{{minor: minor, from: o.Currency, on: o.OccurredOn}}, sp.BaseCurrency, rates)
			if err != nil {
				return apitypes.PositionsResponse{}, 0, err
			}
			if !ok {
				base = nullable.NewNullNullable[int64]()
			} else {
				base = nullable.NewNullableWithValue(converted)
			}
		}
		if err := account.addCharge(o.Currency, minor, base); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
	}

	// Every figure above was computed FIFO within this one account, which is
	// the only rule this application implements. Whether that is the rule the
	// owner's country actually applies is a separate fact, and it ships with
	// the numbers rather than only in the session: this response is where a
	// reader takes cost_minor, realized_pnl_minor and unrealized_pnl_minor
	// from, so it is where the statement of what they are — and are not — has
	// to be available. It describes the computation, not any one row, so it is
	// attached once to the response and is present even when the account holds
	// nothing at all.
	return apitypes.PositionsResponse{
		Positions:      out,
		CostBasisRules: family.CostBasisRulesAPI(sp.CostBasisRules()),
		// Added here rather than by whoever renders the list: see
		// realizedTotals, and RealizedTotal in the API contract.
		RealizedTotal: withAccountTax(totals.result(), ops),
		AccountTotal:  account.result(),
		Cash:          cash,
	}, len(ops), nil
}
