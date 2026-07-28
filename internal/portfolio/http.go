package portfolio

import (
	"context"
	"errors"
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

// converter is the subset of *marketdata.Converter this handler needs: a
// single fx conversion, used to bring a market valuation denominated in a
// different currency than the position (see toAPI) into the position's own
// currency. Local interface (mirroring journalStore/quoteStore) so tests can
// inject a fake in place of a real *marketdata.Converter to control exactly
// which currency pairs have a resolvable rate, including forcing
// marketdata.ErrNoRate — *marketdata.Converter satisfies this structurally,
// no conversion needed at the call site.
type converter interface {
	Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error)
}

// Handler exposes computed account positions over HTTP.
type Handler struct {
	ops         journalStore
	instruments *instrument.Store
	quotes      quoteStore
	conv        converter
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments *instrument.Store, quotes quoteStore, conv converter, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{ops: ops, instruments: instruments, quotes: quotes, conv: conv, auth: auth, sm: sm}
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

// toAPI builds one position's API representation, including its market
// valuation and — when that valuation isn't already in the position's own
// currency — an fx conversion into it (conv.Convert is only ever called
// when that conversion is actually needed).
//
// err is non-nil only for a genuine conversion failure (a Store/DB error, a
// canceled context) — never for marketdata.ErrNoRate, which is an expected,
// handled outcome (see below), not a request failure.
func toAPI(ctx context.Context, conv converter, p *Position, inst instrument.Instrument, quotes map[uuid.UUID]marketdata.Quote) (apitypes.Position, error) {
	out := apitypes.Position{
		Instrument:       instrumentToAPI(inst),
		Quantity:         p.Quantity.String(),
		CostMinor:        p.CostMinor,
		Currency:         p.Currency,
		RealizedPnlMinor: p.RealizedPnLMinor,
		IncomeMinor:      p.IncomeMinor,
		FeesMinor:        p.FeesMinor,
	}
	q, found := quotes[p.InstrumentID]
	if !found {
		return out, nil
	}
	minor, currency, ok := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q)
	if !ok {
		return out, nil
	}
	out.Price = nullable.NewNullableWithValue(q.Price.String())
	out.PriceOn = nullable.NewNullableWithValue(q.On.Format("2006-01-02"))

	// A raw market valuation can be denominated in a currency other than the
	// position's own (for a bond, market_value_currency is the face value's
	// currency — see marketValue's doc comment — which can legitimately
	// differ from the position's trading/settlement currency, e.g. RUB for
	// an OFZ with a USD face value). Left as-is, "Cost" and "Рыночная" would
	// sit in two different currencies in the same row, and subtracting one
	// from the other for unrealized_pnl_minor would silently mix them into a
	// meaningless number.
	//
	// Rather than leave that valuation unusable, convert it into the
	// position's own currency (never the space's base currency — the goal is
	// comparability with cost_minor, which is always in p.Currency, not with
	// other rows) using today's fx rate (time.Now().UTC(): "today" is the
	// only sensible answer for "what is this holding worth right now", as
	// opposed to some historical rate). The original figure is preserved in
	// market_value_source_currency/_minor purely for transparency (e.g. a UI
	// tooltip) — it plays no further part in the computation below.
	if currency != p.Currency {
		converted, err := conv.Convert(ctx, minor, currency, p.Currency, time.Now().UTC())
		switch {
		case err == nil:
			out.MarketValueSourceCurrency = nullable.NewNullableWithValue(currency)
			out.MarketValueSourceMinor = nullable.NewNullableWithValue(minor)
			minor, currency = converted, p.Currency
		case errors.Is(err, marketdata.ErrNoRate):
			// No rate to convert with: fall back to publishing the raw,
			// unconverted figure (as before this change) rather than hiding
			// it. market_value_source_* stay null — nothing was converted.
			// unrealized_pnl_minor is left null below, same as any other
			// currency mismatch.
		default:
			// A genuine failure (DB error, canceled context) — not "no rate
			// available" — must not be silently swallowed into "no
			// conversion happened", which would misrepresent an outage as a
			// normal missing-rate case. Propagate it like any other
			// request-time error (see handleList).
			return apitypes.Position{}, err
		}
	}

	out.MarketValueMinor = nullable.NewNullableWithValue(minor)
	out.MarketValueCurrency = nullable.NewNullableWithValue(currency)
	// Both operands are now guaranteed to be in the same currency whenever
	// this fires — either they always agreed (share/etf), or a successful
	// conversion just brought market_value into p.Currency above — so this
	// is exact integer subtraction on minor units, never a mix of
	// currencies and never a rounding operation.
	if currency == p.Currency {
		out.UnrealizedPnlMinor = nullable.NewNullableWithValue(minor - p.CostMinor)
	}
	return out, nil
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
		apiPos, err := toAPI(r.Context(), h.conv, pos, inst, quotes)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	httpjson.Write(w, http.StatusOK, apitypes.PositionsResponse{Positions: out})
}
