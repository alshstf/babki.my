package instrument

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/money"
)

// defaultSearchLimit and maxSearchLimit bound GET /instruments as the contract
// states them. A limit above the maximum is refused, not clamped (#118): a
// silently smaller page would misreport what was applied.
const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// Handler exposes the instance-wide catalog over HTTP; it needs a valid session
// and role, no space.
type Handler struct {
	store *Store
	auth  *family.Auth
	sm    *scs.SessionManager
}

func NewHandler(store *Store, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/instruments", view(h.handleSearch))
	srv.Mount("POST /api/v1/instruments", edit(h.handleCreate))
	srv.Mount("PATCH /api/v1/instruments/{instrumentId}", edit(h.handleUpdate))
}

func toAPI(i Instrument) apitypes.Instrument {
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
	if i.Type == TypeCrypto {
		out.CoingeckoId = &i.CoinGeckoID
	}
	// Face value is omitted for non-bonds: absent means not applicable.
	if i.FaceValueMinor != nil {
		out.FaceValueMinor = nullable.NewNullableWithValue(*i.FaceValueMinor)
	}
	if i.FaceCurrency != nil {
		out.FaceCurrency = nullable.NewNullableWithValue(*i.FaceCurrency)
	}
	return out
}

func pathInstrumentID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("instrumentId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid instrumentId")
		return uuid.Nil, false
	}
	return id, true
}

// parsePage reads limit and offset within the contract's bounds. It mirrors
// the importer's parsePage but is not shared: each endpoint's bounds are tied
// to its own contract.
func parsePage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit = defaultSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxSearchLimit {
			httpjson.Error(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be a whole number from 1 to %d", maxSearchLimit))
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

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	// hasMore comes from the query; the page's length cannot tell a full last page
	// from a cut one.
	found, hasMore, err := h.store.Search(r.Context(), query, limit, offset)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.Instrument, 0, len(found))
	for _, i := range found {
		out = append(out, toAPI(i))
	}
	httpjson.Write(w, http.StatusOK, apitypes.InstrumentsResponse{Instruments: out, HasMore: hasMore})
}

// Face value rules, shared by creation and update (#93). An exchange quotes a
// bond in percent of face, so the face value turns a quote into money: zero
// values the holding at nothing, a negative below nothing, and half a pair
// cannot be priced. It is bounded above by money.MaxAmountMinor like every
// other money field: an unbounded one made the positions screen fail forever
// (the product overflows). The bound is not a CHECK constraint — it is a
// write-time ceiling on input, like the operation and balance amounts, and
// the constant lives in one place.
//

// The cap also keeps a face value within JavaScript's safe integers, which the
// web's percent conversion requires.
//
// An empty face currency is not NULL, so neither the pair check nor the CHECK
// constraint saw it; it is held to the currency shape like the instrument's
// own currency.
//
// The T-Invest resolver writes catalog rows through Store, bypassing these
// handlers, and restates the rules it could break. Migration 0012's CHECK
// covers the pair, the sign and an empty currency for every writer.
var (
	errFacePair     = errors.New("face_value_minor and face_currency must be set together or not at all")
	errFaceMention  = errors.New("face_value_minor and face_currency must be sent together, even to change one")
	errFacePositive = errors.New("face_value_minor must be positive")
	errFaceTooLarge = fmt.Errorf("face_value_minor must be at most %d", money.MaxAmountMinor)
	errFaceCurrency = errors.New("face_currency must be ISO-4217 uppercase")
)

// errFaceBondOnly: a face value belongs to a bond (#101). On other types a quote
// is already money and the pair means nothing; it is refused at the write
// rather than weakened in the contract. Not a CHECK constraint: rows written
// before the rule stay, and clearing the pair on them still works. The update
// reads the type from the stored row, which no writer can change.
func errFaceBondOnly(t Type) error {
	return fmt.Errorf("face_value_minor and face_currency belong to a bond; this instrument is a %s", t)
}

// checkFaceType refuses either half of the pair on a non-bond; clearing it is
// always allowed. It runs before the pairing rule, so a non-bond is not told
// to send more.
func checkFaceType(t Type, value nullable.Nullable[int64], code nullable.Nullable[string]) error {
	if facePairCarriesAValue(value, code) && t != TypeBond {
		return errFaceBondOnly(t)
	}
	return nil
}

// facePairCarriesAValue reports whether a request sets either half of the
// pair; the update door asks it before reading the row's type.
func facePairCarriesAValue(value nullable.Nullable[int64], code nullable.Nullable[string]) bool {
	return value.IsSpecified() && !value.IsNull() || code.IsSpecified() && !code.IsNull()
}

// checkFacePair: the value and its currency are set together or not at all,
// the value within (0, money.MaxAmountMinor], the currency a valid code. Value
// rules are checked before the currency's, positivity first.
func checkFacePair(value nullable.Nullable[int64], code nullable.Nullable[string]) error {
	valuePresent := value.IsSpecified() && !value.IsNull()
	currencyPresent := code.IsSpecified() && !code.IsNull()
	if valuePresent != currencyPresent {
		return errFacePair
	}
	if valuePresent && value.MustGet() <= 0 {
		return errFacePositive
	}
	if valuePresent && value.MustGet() > money.MaxAmountMinor {
		return errFaceTooLarge
	}
	if currencyPresent && !currency.Valid(code.MustGet()) {
		return errFaceCurrency
	}
	return nil
}

// checkFaceUpdate adds the rule only a PATCH can break: mentioning a field is
// an action (null clears it), so both halves must be mentioned together.
// Otherwise {"face_currency": null} would clear half a stored pair. It judges
// the request alone, so concurrent PATCHes cannot race it into a broken pair.
// A client changing only the face value repeats its currency.
func checkFaceUpdate(value nullable.Nullable[int64], code nullable.Nullable[string]) error {
	if value.IsSpecified() != code.IsSpecified() {
		return errFaceMention
	}
	return checkFacePair(value, code)
}

// Ceilings on a hand-made row's name, ticker and FIGI, in characters as the
// contract's maxLength counts them. Synced rows use the exchange's texts.
const (
	MaxNameRunes   = 200
	MaxTickerRunes = 32
	// MaxCoinGeckoIDRunes bounds a coin's id at CoinGecko, as the contract does.
	MaxCoinGeckoIDRunes = 100
	MaxFIGIRunes        = 32
)

// checkTexts refuses a given text over its ceiling.
func checkTexts(name, ticker, figi *string) error {
	for _, f := range []struct {
		field string
		value *string
		limit int
	}{{"name", name, MaxNameRunes}, {"ticker", ticker, MaxTickerRunes}, {"figi", figi, MaxFIGIRunes}} {
		if f.value != nil && utf8.RuneCountInString(*f.value) > f.limit {
			return fmt.Errorf("%s must be at most %d characters", f.field, f.limit)
		}
	}
	return nil
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateInstrumentRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if req.Name == "" || !Type(req.Type).Valid() || !currency.Valid(req.Currency) {
		httpjson.Error(w, http.StatusBadRequest,
			"name is required, type must be valid, currency must be ISO-4217 uppercase")
		return
	}
	if err := checkTexts(&req.Name, req.Ticker, req.Figi); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	// The type rule first, as on update.
	if err := checkFaceType(Type(req.Type), req.FaceValueMinor, req.FaceCurrency); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkFacePair(req.FaceValueMinor, req.FaceCurrency); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	hasFaceValue := req.FaceValueMinor.IsSpecified() && !req.FaceValueMinor.IsNull()
	hasFaceCurrency := req.FaceCurrency.IsSpecified() && !req.FaceCurrency.IsNull()
	inst := Instrument{
		Type:     Type(req.Type),
		Name:     req.Name,
		Currency: req.Currency,
	}
	if req.Ticker != nil {
		inst.Ticker = *req.Ticker
	}
	if req.Isin != nil {
		isin, err := NormalizeISIN(*req.Isin)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		inst.ISIN = isin
	}
	if req.Figi != nil {
		inst.FIGI = *req.Figi
	}
	if hasFaceValue {
		v := req.FaceValueMinor.MustGet()
		inst.FaceValueMinor = &v
	}
	if hasFaceCurrency {
		v := req.FaceCurrency.MustGet()
		inst.FaceCurrency = &v
	}
	created, err := h.store.Create(r.Context(), inst)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(created))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInstrumentID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateInstrumentRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if req.Name != nil && *req.Name == "" {
		httpjson.Error(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	if err := checkTexts(req.Name, req.Ticker, req.Figi); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	// A PATCH carries no type, so the stored row answers for it — read only when
	// the request sets a face value. A missing row is the ordinary 404.
	if facePairCarriesAValue(req.FaceValueMinor, req.FaceCurrency) {
		stored, err := h.store.ByID(r.Context(), id)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		if err := checkFaceType(stored.Type, req.FaceValueMinor, req.FaceCurrency); err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := checkFaceUpdate(req.FaceValueMinor, req.FaceCurrency); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.CoingeckoId != nil {
		coin := strings.ToLower(strings.TrimSpace(*req.CoingeckoId))
		if utf8.RuneCountInString(coin) > MaxCoinGeckoIDRunes {
			httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("coingecko_id must be at most %d characters", MaxCoinGeckoIDRunes))
			return
		}
		if coin != "" {
			stored, err := h.store.ByID(r.Context(), id)
			if err != nil {
				family.WriteError(w, err)
				return
			}
			if stored.Type != TypeCrypto {
				httpjson.Error(w, http.StatusBadRequest, "only a cryptocurrency has a coin at CoinGecko")
				return
			}
		}
		req.CoingeckoId = &coin
	}
	if req.Isin != nil {
		isin, err := NormalizeISIN(*req.Isin)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Isin = &isin
	}
	upd := Update{
		Name:   req.Name,
		Ticker: req.Ticker,
		ISIN:   req.Isin,
		FIGI:   req.Figi,
		Frozen: req.Frozen,
		// A coin's id at CoinGecko, as its own API spells them: lowercase.
		CoinGeckoID: req.CoingeckoId,
	}
	if req.FaceValueMinor.IsSpecified() {
		if req.FaceValueMinor.IsNull() {
			var cleared *int64
			upd.FaceValueMinor = &cleared
		} else {
			v := req.FaceValueMinor.MustGet()
			ptr := &v
			upd.FaceValueMinor = &ptr
		}
	}
	if req.FaceCurrency.IsSpecified() {
		if req.FaceCurrency.IsNull() {
			var cleared *string
			upd.FaceCurrency = &cleared
		} else {
			v := req.FaceCurrency.MustGet()
			ptr := &v
			upd.FaceCurrency = &ptr
		}
	}
	updated, err := h.store.Update(r.Context(), id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(updated))
}
