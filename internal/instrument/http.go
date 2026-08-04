package instrument

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

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
// A face value had no UPPER bound either, until this paragraph's own sweep
// found it: creation and update both stopped at "positive", so math.MaxInt64
// went in as readily as a real bond's 1 000,00. That is every other money
// field this branch bounds — an operation's amount and price×quantity, an
// account's balance — and this one was the single field of its own subject
// the branch had left open. Verified end to end: POST an instrument with
// face_value_minor = math.MaxInt64, 201; buy two of it at par, 201; GET the
// account's positions, 500 forever — portfolio.marketValue multiplies the
// face value by a price and a quantity, and money.Minor refuses a product
// that does not fit in an int64 of minor units (#27) rather than publish a
// wrapped figure, so the position that holds it can never be drawn again
// without deleting the row by hand. Bounded at money.MaxAmountMinor, the same
// cap and the same reasoning as the operation amount and the account balance
// (see its doc comment): far above any bond ever issued, far enough below
// math.MaxInt64 that this factor cannot be the reason a valuation overflows on
// its own, and one constant instead of a fourth copy of a figure that has
// already drifted apart from itself once in this codebase.
//
// DELIBERATELY NOT A CHECK CONSTRAINT, unlike the pair-and-sign rule migration
// 0012 added for this same column. That constraint exists because a broken
// pair or a non-positive value makes EVERY reader of the row wrong in the same
// way, with no per-query way to guard against it — the database is the only
// place such an invariant can be made to hold for certain. This bound is a
// different kind of rule: neither operation.amount_minor nor
// account_balances.amount_minor, the two fields it is copied from, carries a
// matching CHECK either, and for the same reason — it is a write-time ceiling
// on ordinary input, not an invariant a reader depends on to be correct, and
// money.MaxAmountMinor already lives in exactly one place (this package's
// import of it). A CHECK restating it in SQL would be a second spelling of the
// same number, the very drift this constant exists to prevent one of.
//

// It also closes a second, related gap the sweep found but did not need a
// fix of its own for: face_value_minor is the only money field in this API
// that could have exceeded JavaScript's Number.MAX_SAFE_INTEGER (2^53-1), and
// the frontend's bond price↔percent conversion (bondPriceFromPercent /
// bondPercentFromPrice in web/src/lib/money.ts) both refuse to convert a face
// value that fails Number.isSafeInteger — so no WRONG number could have come
// of it — but nothing in trade-dialog.tsx's faceGapOf named that as a reason
// the field is disabled, so a bond carrying such a value would have shown an
// enabled percent field that simply produced nothing, with no caption saying
// why. money.MaxAmountMinor is, by construction, safely below
// MAX_SAFE_INTEGER (web/src/lib/money.ts's own MAX_AMOUNT_MINOR doc comment
// states the same property for the identical constant), so bounding
// face_value_minor here at the one door that writes it makes that state
// unreachable rather than merely silent: no instrument can ever carry a face
// value the browser cannot represent exactly, and no case needs adding to
// faceGapOf for a state the write side no longer produces. This was never
// live — the upper bound this comment adds and the missing bound it replaces
// were both introduced on this same unmerged branch (e9daddf), so no row in
// any deployed instance was ever written through the unbounded door.
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
	errFaceTooLarge = fmt.Errorf("face_value_minor must be at most %d", money.MaxAmountMinor)
	errFaceCurrency = errors.New("face_currency must be ISO-4217 uppercase")
)

// A FACE VALUE IS A BOND'S, and nothing enforced it (#101). The contract has
// said since the field existed that it is null for anything that is not a bond;
// both doors took one on a share, an ETF, a currency or a crypto row alike and
// published it back with a 201.
//
// Nothing computed a wrong number from that. portfolio.marketValue branches on
// the type — a share and an ETF are valued at the quote's price per unit and
// the face value is never read — and the trade dialog's faceGapOf returns early
// for a non-bond, so the percent field it guards is not even rendered. So this
// is not a broken screen; it is the document promising one thing and the server
// accepting another.
//
// THE CHECK MOVED TO THE WRITE RATHER THAN THE PROMISE BEING WEAKENED, and the
// argument is the pair's meaning rather than today's damage. An exchange quotes
// a bond as a PERCENTAGE OF FACE, which is what makes the face value the factor
// that turns a quote into money; on every other type a quote is money already
// and the pair answers no question at all. A field that means nothing on a row
// is a field a reader will one day read anyway — this program has been bitten by
// exactly that, most recently by an empty face currency that no check looked at
// (#93) — and the cheapest moment to make it unreachable is the one where a
// person is still holding the request. Weakening the description would have been
// the other honest option and buys nothing: it would leave the state reachable,
// leave every future reader to decide what a share's face value means, and cost
// the same words to write down.
//
// DELIBERATELY NOT A CHECK CONSTRAINT, unlike the pair-and-sign rule of
// migration 0012, and this is the point where the two rules genuinely differ. A
// broken pair makes every reader of the row wrong in the same way, so the
// database is the only place it can be made to hold for certain. A face value on
// a share makes no reader wrong today, and a constraint would come with a
// migration that either stops a running instance over a harmless row or clears a
// number somebody typed. Neither is a migration's decision to make (0012 says
// the same about repairing rows), so rows written before this rule are left
// exactly as they stand — and the rule is written about SETTING the pair rather
// than about carrying it, so clearing such a row through the API keeps working.
// That is the repair, and a rule that refused every PATCH of the pair on a
// non-bond would have taken it away.
//
// The type is not in an UpdateInstrumentRequest and cannot be: nothing in this
// program changes an instrument's type — the request has no such field and the
// UPDATE statement does not touch the column — so the update door reads it from
// the stored row. That read is not the kind checkFaceUpdate refuses to do: what
// it reads cannot change between the read and the write, because no writer
// exists that could change it.
func errFaceBondOnly(t Type) error {
	return fmt.Errorf("face_value_minor and face_currency belong to a bond; this instrument is a %s", t)
}

// checkFaceType refuses a face value, or its currency, on an instrument that is
// not a bond. "Present" means carrying a value, as it does in checkFacePair: an
// omitted field and an explicit null are both absent, so clearing the pair is
// never what this refuses.
//
// EITHER HALF ALONE IS REFUSED BY THIS RULE RATHER THAN BY THE PAIRING ONE, and
// that ordering is deliberate at both doors: on a non-bond neither half belongs,
// so "set them together" would answer a client that may not send them at all by
// asking it to send more. On a bond the pairing rule is reached exactly as
// before.
func checkFaceType(t Type, value nullable.Nullable[int64], code nullable.Nullable[string]) error {
	if facePairCarriesAValue(value, code) && t != TypeBond {
		return errFaceBondOnly(t)
	}
	return nil
}

// facePairCarriesAValue reports whether a request actually SETS one half of the
// pair or the other, as against leaving it alone or clearing it. It is one
// statement rather than two because the update door asks the same question
// before it goes to the database for the row's type: if these two ever came to
// answer differently, the door would fetch a row it then refuses to judge, or
// judge one it never fetched.
func facePairCarriesAValue(value nullable.Nullable[int64], code nullable.Nullable[string]) bool {
	return value.IsSpecified() && !value.IsNull() || code.IsSpecified() && !code.IsNull()
}

// checkFacePair judges the pair as the request STATES it, and is what both doors
// share: the value and its currency are present together or not at all, a
// present value is within (0, money.MaxAmountMinor], and a present currency is
// a currency code. "Present" means carrying a value — an omitted field and an
// explicit null both count as absent, which is what they mean on a creation.
//
// The value's own rules are tried before its currency's, so that the field a
// reader is told about is the one nearer the money: a face value of zero prices
// the whole holding at nothing and one past the cap breaks the very screen that
// reads it (see the doc comment above), while a misspelled currency merely
// stops it being priced at all. Between the value's own two rules, positive is
// tried first because it is the cheaper mistake to make — a negative or zero
// face value is one keystroke away from an ordinary one, math.MaxInt64 is not.
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
// and that is the deliberate part — though the row is not unread here: handleUpdate
// reads it just above this call, for its TYPE, before checkFaceType. That read
// cannot be raced, because a type is not a column any writer in this program can
// ever change. The PAIR is different, and it is what this comment is about:
// judging the RESULT would mean reading the pair's own two columns before
// writing them — and between that read and the write another PATCH can move the
// half this one is not touching, so two requests that each looked sound against
// what they read would leave a broken pair behind. This rule reads neither half
// of the pair; it judges the request alone, so it cannot be raced that way.
// Creation cannot break this particular rule at all — there is no stored half
// for it to leave behind — so between the two doors every accepted write leaves
// the pair whole, and every row is whole by induction from a catalog that starts
// whole (migration 0012).
//
// What it costs: a client changing only the face value of a bond has to repeat
// its currency. No screen does this today — nothing in the frontend PATCHes an
// instrument — and the contract says so on the field.
func checkFaceUpdate(value nullable.Nullable[int64], code nullable.Nullable[string]) error {
	if value.IsSpecified() != code.IsSpecified() {
		return errFaceMention
	}
	return checkFacePair(value, code)
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
	// The type rule first: it says the pair does not belong on this row at all,
	// which is a more useful answer than a rule about the value inside it, and
	// it is the same order the update door takes.
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
	// The type rule is the one thing here that cannot be judged from the request:
	// a PATCH carries no type, and nothing in this program can change one, so the
	// stored row answers for it (see errFaceBondOnly). The row is read only when
	// the request actually SETS a face value — a PATCH that touches other fields,
	// or one that clears the pair, costs no extra query and is never refused by
	// this rule. A missing row falls out here as the ordinary 404 rather than as
	// a sentence about bonds.
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
