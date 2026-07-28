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
// currency, plus Rate, used to convert a whole position's amounts into the
// space's base currency (see positionInBase) while resolving the underlying
// rate at most once per currency per request. Local interface (mirroring
// journalStore/quoteStore) so tests can inject a fake in place of a real
// *marketdata.Converter to control exactly which currency pairs have a
// resolvable rate, including forcing marketdata.ErrNoRate —
// *marketdata.Converter satisfies this structurally, no conversion needed at
// the call site.
type converter interface {
	Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error)
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
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
	instruments *instrument.Store
	quotes      quoteStore
	conv        converter
	spaces      spaceStore
	auth        *family.Auth
	sm          *scs.SessionManager
}

func NewHandler(ops journalStore, instruments *instrument.Store, quotes quoteStore, conv converter, spaces spaceStore, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{ops: ops, instruments: instruments, quotes: quotes, conv: conv, spaces: spaces, auth: auth, sm: sm}
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
	// an OFZ with a USD face value). Left as-is, the cost and market-value
	// columns would sit in two different currencies in the same row, and
	// subtracting one from the other for unrealized_pnl_minor would silently
	// mix them into a meaningless number.
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

// nullableInt64 extracts the value from a nullable.Nullable[int64] API
// field, treating "unspecified" and "explicit null" the same way (nil).
// toAPI leaves market_value_minor/unrealized_pnl_minor unspecified — rather
// than explicitly null — whenever there's no usable quote or valuation (see
// toAPI), so this reads that "no value" state regardless of which of the two
// wire representations produced it.
func nullableInt64(n nullable.Nullable[int64]) *int64 {
	v, err := n.Get()
	if err != nil {
		return nil
	}
	return &v
}

// rateLookup memoizes one currency's resolved fx rate (and the date it came
// from, or the resolution error) so handleList's per-position conversion
// loop below hits the fx rate store at most once per distinct position
// currency, not once per position. Mirrors account.Handler's identically
// named type/pattern exactly (see its doc comment for the full rationale) —
// there are two copies rather than one shared type because journalStore and
// converter are already local, unexported interfaces per package, and this
// type is just as small.
type rateLookup struct {
	rate decimal.Decimal
	date time.Time
	err  error
}

// positionInBase converts a position's cost_minor, market_value_minor (when
// present), unrealized_pnl_minor (when present), and income_minor from the
// position's own currency (p.Currency) into baseCurrency, using cache to
// memoize the underlying marketdata.Converter.Rate lookup across positions
// that share a currency within a single handleList request — see
// rateLookup. marketValueMinor and unrealizedPnlMinor are the already-computed
// (and possibly nil) values from the position's top-level API fields (see
// toAPI/nullableInt64), not recomputed here.
//
// Each amount is converted and rounded independently
// (decimal.Mul(rl.rate).Round(0), the exact step marketdata.Converter.Convert
// itself uses) rather than derived from one another via a shared sum: cost,
// market value, unrealized P&L and income are four independent figures, not
// terms of one total, so rounding a combined sum once would not reproduce
// this and could drift by a minor unit from converting each figure on its
// own. fees_minor and realized_pnl_minor are deliberately excluded (owner
// feedback — not carried into PositionInBase at all, see the API contract).
//
// It returns (nil, nil) — render in_base as null, the WHOLE object, never
// partially populated — in exactly two cases: p.Currency already equals
// baseCurrency (nothing to convert), or no fx rate could be resolved for the
// pair (marketdata.ErrNoRate). This differs from marketValueMinor/
// unrealizedPnlMinor inside the returned object, which are null only when
// the corresponding input pointer itself is nil (no quote) — a missing quote
// nulls just that one figure, but a missing rate nulls the entire
// conversion, since a partially converted position (e.g. cost converted but
// market value silently left in the wrong currency) is worse than an honest
// "can't convert this position at all".
//
// A non-nil error means a genuine failure (DB error, canceled context) that
// the caller must surface as a request error — never silently rendered as
// null, which would misrepresent an outage as "nothing to convert".
func (h *Handler) positionInBase(ctx context.Context, p *Position, marketValueMinor, unrealizedPnlMinor *int64, baseCurrency string, cache map[string]*rateLookup) (*apitypes.PositionInBase, error) {
	if p.Currency == baseCurrency {
		return nil, nil
	}
	rl, ok := cache[p.Currency]
	if !ok {
		rate, date, err := h.conv.Rate(ctx, p.Currency, baseCurrency, time.Now().UTC())
		rl = &rateLookup{rate: rate, date: date, err: err}
		cache[p.Currency] = rl
	}
	if rl.err != nil {
		if errors.Is(rl.err, marketdata.ErrNoRate) {
			return nil, nil
		}
		return nil, rl.err
	}

	convert := func(minor int64) int64 {
		return decimal.NewFromInt(minor).Mul(rl.rate).Round(0).IntPart()
	}
	out := &apitypes.PositionInBase{
		CostMinor:   convert(p.CostMinor),
		IncomeMinor: convert(p.IncomeMinor),
		Currency:    baseCurrency,
		RateOn:      rl.date.Format("2006-01-02"),
	}
	if marketValueMinor != nil {
		out.MarketValueMinor = nullable.NewNullableWithValue(convert(*marketValueMinor))
	} else {
		out.MarketValueMinor = nullable.NewNullNullable[int64]()
	}
	if unrealizedPnlMinor != nil {
		out.UnrealizedPnlMinor = nullable.NewNullableWithValue(convert(*unrealizedPnlMinor))
	} else {
		out.UnrealizedPnlMinor = nullable.NewNullNullable[int64]()
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

	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
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

	// Scoped to this request only: see positionInBase/rateLookup.
	rates := make(map[string]*rateLookup)
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

		inBase, err := h.positionInBase(r.Context(), pos,
			nullableInt64(apiPos.MarketValueMinor), nullableInt64(apiPos.UnrealizedPnlMinor),
			sp.BaseCurrency, rates)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if inBase != nil {
			apiPos.InBase = nullable.NewNullableWithValue(*inBase)
		} else {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	httpjson.Write(w, http.StatusOK, apitypes.PositionsResponse{Positions: out})
}
