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

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

// journalStore is what the handler needs from operation.Store, as a local
// interface because package operation imports this one.
type journalStore interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error)
}

// quoteStore is what the handler needs from marketdata.Store, local so tests
// can control quotes and count round trips.
type quoteStore interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// instrumentStore reads the catalog rows behind a list of positions in one
// round trip; local so a fake can count trips and produce a missing row.
type instrumentStore interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// spaceStore reads the space's base currency.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

type Handler struct {
	ops         journalStore
	instruments instrumentStore
	quotes      quoteStore
	conv        marketdata.RateSource
	spaces      spaceStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments instrumentStore, quotes quoteStore, conv marketdata.RateSource, spaces spaceStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
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

// handleList replays the account's journal into positions, closed ones
// included, sorted by instrument name. Like the journal endpoint, an unknown
// account id yields an empty list rather than a 404; data never crosses a
// space.
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
		// Practically unreachable: every write replays the journal through this
		// engine first.
		httpjson.Error(w, http.StatusUnprocessableEntity, notComputed.Error())
	case errors.Is(err, errInstrumentNotInCatalog):
		httpjson.Error(w, http.StatusNotFound, "not found")
	case err != nil:
		family.WriteError(w, err)
	default:
		httpjson.Write(w, http.StatusOK, resp)
	}
}

// journalDoesNotCompute is the engine refusing a stored journal.
type journalDoesNotCompute struct{ err error }

func (e journalDoesNotCompute) Error() string { return e.err.Error() }
func (e journalDoesNotCompute) Unwrap() error { return e.err }

// errInstrumentNotInCatalog: the journal names a paper the catalog lacks —
// unreachable behind the foreign key, answered rather than skipped.
var errInstrumentNotInCatalog = errors.New("portfolio: the journal names an instrument the catalog does not hold")

// positionsResponse is everything the positions screen shows for an account,
// and its operation count. It is the one place an account is valued; the
// family total reads it too (see ValueFromJournal).
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
		// Practically unreachable: writes replay the journal before committing.
		return apitypes.PositionsResponse{}, 0, journalDoesNotCompute{err}
	}

	instrumentIDs := make([]uuid.UUID, 0, len(positions))
	for id := range positions {
		instrumentIDs = append(instrumentIDs, id)
	}
	// One round trip for the catalog rows and one for the quotes.
	instruments, err := h.instruments.ByIDs(ctx, instrumentIDs)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	// One "today" for the whole request, shared by valuations, their conversions
	// and the prefetch.
	now := time.Now().UTC()
	book, err := h.pricesOn(ctx, instrumentIDs, now, sp.FullValuation, todayWindows)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	quotes := book.fullQuotes()

	// Both per request.
	rates := marketdata.NewRateMemo(h.conv)
	income := incomeByInstrument(ops)

	// The account's money, folded from the same journal before the warm-up so its
	// rates join the same batch.
	cashPositions, err := Cash(ops)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	rates.Prefetch(ctx, rateQueries(positions, instruments, quotes, income, cashPositions, sp.BaseCurrency, now))

	totals := newRealizedTotals(sp.BaseCurrency)
	account := newAccountTotals(sp.BaseCurrency)
	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, ok := instruments[pos.InstrumentID]
		if !ok {
			// A missing catalog row is unreachable behind the foreign key; answered with a
			// 404 rather than skipped, which would silently shrink every total.
			return apitypes.PositionsResponse{}, 0, errInstrumentNotInCatalog
		}
		apiPos, err := h.toAPI(ctx, pos, inst, quotes, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		if err := h.addLiquid(ctx, &apiPos, pos, inst, book, sp.BaseCurrency, now, rates); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		// Struck once, used twice: the position's in_base figure and a term of the
		// account total, which takes it even when the in_base object is absent.
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
		// The gap decides whether there is an object: a cause is published only
		// without one. If both ever came back, the object is dropped and the honest
		// caption kept.
		if apiGap, missing := apiInBaseGap(gap); missing {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			apiPos.InBaseGap = nullable.NewNullableWithValue(apiGap)
		} else {
			apiPos.InBaseGap = nullable.NewNullNullable[apitypes.InBaseGap]()
			// No gap: the object was struck, or the position is in the base currency;
			// the pointer tells them apart.
			if inBase != nil {
				apiPos.InBase = nullable.NewNullableWithValue(*inBase)
			} else {
				apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			}
		}

		// Added after the in_base object is decided, since the total reads it.
		if err := account.addPosition(apiPos, inBase, gap, realizedGap); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	// The account's money, valued here where the rates are.
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

	// The account's own charges, each at its own day's rate (НК РФ ст. 210 п. 5
	// for tax, consistency for the rest).
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

	// Every figure is FIFO within this account; whether that is the owner's
	// country's rule ships with the numbers, once per response.
	return apitypes.PositionsResponse{
		Positions:      out,
		CostBasisRules: family.CostBasisRulesAPI(sp.CostBasisRules()),
		// See realizedTotals and RealizedTotal in the contract.
		RealizedTotal: withAccountTax(totals.result(), ops),
		AccountTotal:  account.result(),
		Cash:          cash,
	}, len(ops), nil
}
