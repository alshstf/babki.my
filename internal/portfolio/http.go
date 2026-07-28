package portfolio

import (
	"context"
	"net/http"
	"sort"

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

// Handler exposes computed account positions over HTTP.
type Handler struct {
	ops         journalStore
	instruments *instrument.Store
	quotes      quoteStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments *instrument.Store, quotes quoteStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{ops: ops, instruments: instruments, quotes: quotes, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	srv.Mount("GET /api/v1/accounts/{accountId}/positions", view(h.handleList))
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

// instrumentToAPI mirrors instrument.toAPI (unexported in package
// instrument): each module owns its own domain-to-API mapping rather than
// sharing one across packages, matching account.toAPI/operation.toAPI.
func instrumentToAPI(i instrument.Instrument) apitypes.Instrument {
	out := apitypes.Instrument{
		Id:       i.ID,
		Type:     apitypes.InstrumentType(i.Type),
		Name:     i.Name,
		Ticker:   i.Ticker,
		Isin:     i.ISIN,
		Figi:     i.FIGI,
		Currency: i.Currency,
		Frozen:   i.Frozen,
	}
	if i.FaceValueMinor != nil {
		out.FaceValueMinor = nullable.NewNullableWithValue(*i.FaceValueMinor)
	}
	if i.FaceCurrency != nil {
		out.FaceCurrency = nullable.NewNullableWithValue(*i.FaceCurrency)
	}
	return out
}

// centsPerUnit shifts a major-unit decimal amount into minor units — the
// convention (2 decimal digits) used everywhere else in this codebase for
// amountMinor, e.g. marketdata.Converter (see its doc comment). Quote.Price
// and instrument-catalog prices are always expressed in major units of
// their currency.
const centsPerUnit = 2

// marketValue computes a position's market value from the latest quote for
// its instrument, in minor units, plus the currency that value is
// denominated in. ok is false when no valuation applies — the instrument's
// type isn't priced this way (bond without both a face value and a face
// currency, or currency/crypto/metal/custom) — in which case the caller
// must leave market_value_minor/market_value_currency/price/price_on null
// rather than publish a zero or misleading figure.
//
// share/etf: price (per unit, major currency units) × quantity, in the
// quote's own currency (q.Currency) — price and instrument currency always
// agree here.
//
// bond: price is a percentage of face value (e.g. 95.20 meaning 95.20%), so
// the value is faceValueMinor × price/100 × quantity — already in minor
// units since faceValueMinor is. Crucially, that value is denominated in the
// face value's currency (faceCurrency, e.g. RUB for an OFZ), NOT the quote's
// currency: the quote's "currency" field for a bond is really just the unit
// the percentage price is quoted in, not a currency the resulting money is
// ever in. A bond with a face value but no face currency has no way to
// label that number, so it gets no valuation at all rather than a
// currency-less (and therefore meaningless) figure.
//
// Both branches multiply as decimals throughout and round exactly once at
// the end, half-away-from-zero (decimal.Decimal.Round's native behavior, the
// same rounding marketdata.Converter.Convert uses) — never float, never an
// intermediate round.
func marketValue(instType instrument.Type, faceValueMinor *int64, faceCurrency *string, quantity decimal.Decimal, q marketdata.Quote) (minor int64, currency string, ok bool) {
	switch instType {
	case instrument.TypeShare, instrument.TypeETF:
		return q.Price.Mul(quantity).Shift(centsPerUnit).Round(0).IntPart(), q.Currency, true
	case instrument.TypeBond:
		if faceValueMinor == nil || faceCurrency == nil {
			return 0, "", false
		}
		return decimal.NewFromInt(*faceValueMinor).Mul(q.Price).Shift(-centsPerUnit).Mul(quantity).Round(0).IntPart(), *faceCurrency, true
	default:
		// currency, crypto, metal, custom: no defined valuation model yet.
		return 0, "", false
	}
}

func toAPI(p *Position, inst instrument.Instrument, quotes map[uuid.UUID]marketdata.Quote) apitypes.Position {
	out := apitypes.Position{
		Instrument:       instrumentToAPI(inst),
		Quantity:         p.Quantity.String(),
		CostMinor:        p.CostMinor,
		Currency:         p.Currency,
		RealizedPnlMinor: p.RealizedPnLMinor,
		IncomeMinor:      p.IncomeMinor,
		FeesMinor:        p.FeesMinor,
	}
	if q, found := quotes[p.InstrumentID]; found {
		if minor, currency, ok := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q); ok {
			out.MarketValueMinor = nullable.NewNullableWithValue(minor)
			out.MarketValueCurrency = nullable.NewNullableWithValue(currency)
			out.Price = nullable.NewNullableWithValue(q.Price.String())
			out.PriceOn = nullable.NewNullableWithValue(q.On.Format("2006-01-02"))
			// Unrealized P&L is only meaningful when the market valuation is
			// denominated in the position's own currency: for a bond, currency
			// is market_value_currency's face_currency, which can legitimately
			// differ from the position's currency (the instrument's own
			// trading currency, e.g. RUB for an OFZ quoted/settled in RUB but
			// with a USD face value). Subtracting cost_minor (in currency)
			// from a market value in a different currency would silently mix
			// currencies into a meaningless number, so leave it null instead.
			// Both operands are already integer minor units of one currency
			// here, so this is exact integer subtraction — no rounding.
			if currency == p.Currency {
				out.UnrealizedPnlMinor = nullable.NewNullableWithValue(minor - p.CostMinor)
			}
		}
	}
	return out
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

	ops, err := h.ops.ListForEngine(r.Context(), p.SpaceID, accountID)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	positions, err := Compute(ops)
	if err != nil {
		// Practically unreachable: every write to the journal already passes
		// through this same engine (operation.Service.Create/Delete replay
		// the journal to check consistency before committing), so a stored
		// journal that fails to replay here would mean the data is already
		// corrupted rather than a normal request-time error.
		httpjson.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	instrumentIDs := make([]uuid.UUID, 0, len(positions))
	for id := range positions {
		instrumentIDs = append(instrumentIDs, id)
	}
	// One batched round trip for every position's quote, never one per
	// position (N+1) — see quoteStore's doc comment.
	quotes, err := h.quotes.LatestQuotes(r.Context(), instrumentIDs)
	if err != nil {
		family.WriteError(w, err)
		return
	}

	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, err := h.instruments.ByID(r.Context(), pos.InstrumentID)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		out = append(out, toAPI(pos, inst, quotes))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	httpjson.Write(w, http.StatusOK, apitypes.PositionsResponse{Positions: out})
}
