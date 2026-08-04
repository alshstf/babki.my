package instrument

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// defaultSearchLimit/maxSearchLimit bound the GET /instruments listing.
// A limit above maxSearchLimit is clamped down rather than rejected with
// 400: a catalog search is a best-effort listing, and clamping lets a
// client that asks for "everything" keep working instead of erroring on a
// pagination detail it doesn't need to reason about.
const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// Handler exposes the instrument catalog over HTTP. The catalog is
// instance-wide (see package doc), so handlers only require a valid family
// session and role — no space scoping.
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
	// face_value_minor/face_currency are left unspecified (omitted from the
	// response body) for non-bond instruments. Unlike account.owner_user_id
	// there is no "server didn't return it" ambiguity to resolve here: GET
	// always returns the full instrument, so "absent" unambiguously means
	// "not applicable to this instrument".
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

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	limit := defaultSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	found, err := h.store.Search(r.Context(), query, limit)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.Instrument, 0, len(found))
	for _, i := range found {
		out = append(out, toAPI(i))
	}
	httpjson.Write(w, http.StatusOK, out)
}

// What a face value has to be, in one place, so that the two doors into those
// two columns — creation and update — cannot come to refuse different things or
// in different words (#93). Creation checked only that the pair arrived whole,
// which let a face value of ZERO and a currency of "" through; the update
// checked nothing whatsoever, so a PATCH could clear the currency and leave the
// value behind. Everything they can BOTH break is stated once, below; the update
// adds one rule of its own, which only it can break (see checkFaceUpdate).
//
// None of those states is cosmetic. An exchange quotes a bond as a PERCENTAGE OF FACE
// (see portfolio.marketValue, and bondPriceFromPercent in the frontend), so the
// face value is the factor that turns a quote into money. At zero the whole
// holding is valued at 0,00 — a published figure that is not the truth, which is
// the one thing this program refuses to do — and a negative one would value it
// below nothing. Half a pair is milder, because every reader requires both
// halves before it values anything, but it is still a bond that silently cannot
// be priced.
//
// A currency that names no currency is the same failure wearing a value. The
// contract calls face_currency ISO-4217 and the readers take it at its word: a
// bond's market value is denominated in it (portfolio.marketValue), so an empty
// string there publishes a bare number with no currency on it, and the trade
// dialog writes «Номинал в , а сделка в RUB». The instrument's own currency has
// been held to currencyRe at this same door all along; its face value's currency
// simply was not — and since an empty string is not NULL, neither the pair check
// here nor the CHECK constraint behind it ever looked.
//
// The messages name the fields and the rule. The API is the only way to reach
// any of these states (no screen writes a face value, and nothing in the
// frontend PATCHes an instrument at all), so an importer's log is where these
// will be read.
var (
	errFacePair     = errors.New("face_value_minor and face_currency must be set together or not at all")
	errFaceMention  = errors.New("face_value_minor and face_currency must be sent together, even to change one")
	errFacePositive = errors.New("face_value_minor must be positive")
	errFaceCurrency = errors.New("face_currency must be ISO-4217 uppercase")
)

// checkFacePair judges the pair as the request STATES it, and is what both doors
// share: the value and its currency are present together or not at all, a
// present value is positive, and a present currency is a currency code.
// "Present" means carrying a value — an omitted field and an explicit null both
// count as absent, which is what they mean on a creation.
//
// The value's own rule is tried before its currency's, so that the field a
// reader is told about is the one nearer the money: a face value of zero prices
// the whole holding at nothing, while a misspelled currency merely stops it
// being priced at all.
func checkFacePair(value nullable.Nullable[int64], currency nullable.Nullable[string]) error {
	valuePresent := value.IsSpecified() && !value.IsNull()
	currencyPresent := currency.IsSpecified() && !currency.IsNull()
	if valuePresent != currencyPresent {
		return errFacePair
	}
	if valuePresent && value.MustGet() <= 0 {
		return errFacePositive
	}
	if currencyPresent && !currencyRe.MatchString(currency.MustGet()) {
		return errFaceCurrency
	}
	return nil
}

// checkFaceUpdate is checkFacePair plus the one rule that only an update can
// break: on a PATCH, MENTIONING a field is itself an action — an explicit null
// clears the column while an omitted field leaves it alone — so the two halves
// have to be mentioned together as well as valued together. Without this,
// {"face_currency": null} reads as "both absent", passes the shared rule, and
// clears one half of a stored pair.
//
// It refuses in a sentence of its own, and that is not decoration. Answering
// {"face_value_minor": 200000} on a bond that already stores "RUB" with the
// shared rule's "must be set together or not at all" states something TRUE of
// that row before the request and after it, which leaves the client no wiser
// about why it was turned away; what it has to do is resend the currency, and
// only a rule about the REQUEST can say so. The contract has described the
// mention rule correctly on the field all along — this is the runtime catching
// up with it.
//
// It is a rule about the REQUEST rather than about the row the request lands on,
// and that is the deliberate part. Judging the RESULT would mean reading the
// stored row first and then writing — and between the read and the write another
// PATCH can move the half this one is not touching, so two requests that each
// looked sound against what they read leave a broken pair behind. Reading the
// request alone cannot be raced. Creation cannot break this particular rule at
// all — there is no stored half for it to leave behind — so between the two
// doors every accepted write leaves the pair whole, and every row is whole by
// induction from a catalog that starts whole (migration 0012).
//
// What it costs: a client changing only the face value of a bond has to repeat
// its currency. No screen does this today — nothing in the frontend PATCHes an
// instrument — and the contract says so on the field.
func checkFaceUpdate(value nullable.Nullable[int64], currency nullable.Nullable[string]) error {
	if value.IsSpecified() != currency.IsSpecified() {
		return errFaceMention
	}
	return checkFacePair(value, currency)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateInstrumentRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if req.Name == "" || !Type(req.Type).Valid() || !currencyRe.MatchString(req.Currency) {
		httpjson.Error(w, http.StatusBadRequest,
			"name is required, type must be valid, currency must be ISO-4217 uppercase")
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
		inst.ISIN = *req.Isin
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
	if err := checkFaceUpdate(req.FaceValueMinor, req.FaceCurrency); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	upd := Update{
		Name:   req.Name,
		Ticker: req.Ticker,
		ISIN:   req.Isin,
		FIGI:   req.Figi,
		Frozen: req.Frozen,
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
