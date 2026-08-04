package instrument_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/money"
)

// An instrument's face value was written unchecked (#93). Creation asked only
// that the value and its currency arrive TOGETHER, so a face value of zero went
// in; the update asked nothing at all, so a PATCH could clear the currency and
// leave the value behind.
//
// Neither is a cosmetic gap. An exchange quotes a bond as a PERCENTAGE OF FACE,
// so the face value is what turns that quote into money: at zero, every price
// values the whole holding at nothing, and the positions screen publishes 0,00
// for a bond that is worth something. The trade dialog already says, honestly,
// which of the two is missing when it cannot convert a percentage into roubles
// (plan 11) — but that is a reader coping with a state the write let through,
// and every future reader would have to cope with it again.

// wantFaceRefusal asserts the WHOLE refusal. Both rules are about the same pair
// of fields, so a message that named the other rule would still look plausible
// beside the request that provoked it; only the whole sentence tells them apart.
func wantFaceRefusal(t *testing.T, resp *http.Response, want, sent string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%s = %d, want 400: %s", sent, resp.StatusCode, body)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode refusal %q: %v", body, err)
	}
	if got.Error != want {
		t.Errorf("refusal to %s = %q, want exactly %q", sent, got.Error, want)
	}
}

var (
	facePairRule     = "face_value_minor and face_currency must be set together or not at all"
	faceMentionRule  = "face_value_minor and face_currency must be sent together, even to change one"
	facePositiveRule = "face_value_minor must be positive"
	faceTooLargeRule = fmt.Sprintf("face_value_minor must be at most %d", money.MaxAmountMinor)
	faceCurrencyRule = "face_currency must be ISO-4217 uppercase"
)

// faceBondOnlyRule is the refusal for a face value on something that is not a
// bond, and it NAMES THE TYPE it found — spelled out here as a format so every
// assertion below compares the whole sentence including that type. A message
// that named the wrong type would send an importer to fix the wrong row.
func faceBondOnlyRule(instrumentType string) string {
	return fmt.Sprintf("face_value_minor and face_currency belong to a bond; this instrument is a %s", instrumentType)
}

// mkBond creates an ordinary bond with a sound face value and returns its id.
func mkBond(t *testing.T, url string, c *http.Client) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"ОФЗ 26238","ticker":"SU26238RMFS4","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create bond = %d: %s", resp.StatusCode, b)
	}
	var bond struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bond); err != nil {
		t.Fatalf("decode bond: %v", err)
	}
	return bond.ID
}

type facePair struct {
	FaceValueMinor *int64  `json:"face_value_minor"`
	FaceCurrency   *string `json:"face_currency"`
}

// readFacePair fetches the instrument through the catalog search and returns
// the pair as it is actually stored — the only way to tell a refusal that
// changed nothing from one that refused after writing.
func readFacePair(t *testing.T, url string, c *http.Client, id string) facePair {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/instruments", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list instruments = %d", resp.StatusCode)
	}
	var out []struct {
		ID string `json:"id"`
		facePair
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	for _, i := range out {
		if i.ID == id {
			return i.facePair
		}
	}
	t.Fatalf("instrument %s is not in the catalog", id)
	return facePair{}
}

func TestCreateRefusesAFaceValueThatIsNotAValue(t *testing.T) {
	url, c := newAPI(t)

	// Zero: the state the trade dialog names as bad_face_value, and the one that
	// prices a whole holding at nothing.
	wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"X","currency":"RUB","face_value_minor":0,"face_currency":"RUB"}`),
		facePositiveRule, "create with a face value of zero")

	// And negative, which would price the holding below nothing.
	wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"X","currency":"RUB","face_value_minor":-100000,"face_currency":"RUB"}`),
		facePositiveRule, "create with a negative face value")
}

// TestCreateTakesTheSmallestRealFaceValue fixes the edge: one minor unit is a
// face value, and a rule that refused it would withhold a perfectly recordable
// instrument.
func TestCreateTakesTheSmallestRealFaceValue(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"Однокопеечная","currency":"RUB","face_value_minor":1,"face_currency":"RUB"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with a face value of one minor unit = %d, want 201: %s", resp.StatusCode, b)
	}
}

// TestCreateTakesTheLargestRealFaceValue fixes the other edge: a face value at
// money.MaxAmountMinor is exactly as recordable as one just under it — the same
// cap an operation's amount and an account's balance are written against (see
// its doc comment), and a rule that refused the cap itself rather than what
// lies past it would be a different, stricter bound than the one this
// codebase states everywhere else.
func TestCreateTakesTheLargestRealFaceValue(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/instruments",
		fmt.Sprintf(`{"type":"bond","name":"Крупный номинал","currency":"RUB","face_value_minor":%d,"face_currency":"RUB"}`,
			money.MaxAmountMinor))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with a face value of money.MaxAmountMinor = %d, want 201: %s", resp.StatusCode, b)
	}
}

// TestCreateRefusesAFaceValueTooLarge is #93's own gap, found by this branch's
// own sweep: creation and update both bounded a face value from below and
// never from above, so math.MaxInt64 went in as readily as a real bond's
// 1 000,00. Verified end to end (see checkFacePair's doc comment): such an
// instrument bought into a position answers 500 on every later GET of that
// account's positions, forever, which is exactly the failure this whole branch
// exists to turn into a rejected field instead.
func TestCreateRefusesAFaceValueTooLarge(t *testing.T) {
	url, c := newAPI(t)

	for _, tc := range []struct {
		what  string
		value int64
	}{
		{"create with a face value one minor unit past the cap", money.MaxAmountMinor + 1},
		{"create with a face value of math.MaxInt64", math.MaxInt64},
	} {
		wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments",
			fmt.Sprintf(`{"type":"bond","name":"X","currency":"RUB","face_value_minor":%d,"face_currency":"RUB"}`, tc.value)),
			faceTooLargeRule, tc.what)
	}
}

// TestUpdateRefusesAFaceValueTooLarge is TestCreateRefusesAFaceValueTooLarge's
// twin at the other door: a PATCH is the only way an instrument created sound
// can be made to carry a face value the write side would have refused on
// creation, and it must refuse it exactly the same way.
func TestUpdateRefusesAFaceValueTooLarge(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		fmt.Sprintf(`{"face_value_minor":%d,"face_currency":"RUB"}`, money.MaxAmountMinor+1)),
		faceTooLargeRule, "update to a face value one minor unit past the cap")

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 {
		t.Errorf("face value after the refusal = %+v, want 100000 untouched", pair)
	}
}

// TestCreateStillTakesAnInstrumentWithNoFaceValue: the pair is optional, and a
// bond whose face value nobody has recorded yet is an ordinary catalog row. It
// simply cannot be priced until one is.
func TestCreateStillTakesAnInstrumentWithNoFaceValue(t *testing.T) {
	url, c := newAPI(t)

	for _, body := range []string{
		`{"type":"bond","name":"Без номинала","currency":"RUB"}`,
		`{"type":"bond","name":"Явные null","currency":"RUB","face_value_minor":null,"face_currency":null}`,
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`,
	} {
		if resp := do(t, c, "POST", url+"/api/v1/instruments", body); resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("create %s = %d, want 201: %s", body, resp.StatusCode, b)
		}
	}
}

// TestCreateRefusesAFaceCurrencyThatNamesNoCurrency is the gap the contract
// described and nothing enforced: face_currency was declared ISO-4217 and
// checked only for PRESENCE, so `"face_currency": ""` was created, stored, and
// returned with a 201.
//
// An empty string is the interesting one because it is invisible to the checks
// that were there. It is not null, so the pairing rule reads the pair as whole,
// and an empty string is not NULL either, so the CHECK constraint behind it
// passed the row too. What comes out is a bond whose face value is denominated
// in nothing: portfolio.marketValue prices it in face_currency and publishes a
// figure with an empty currency beside it — a bare number, which
// TestMarketValueNeedsBothHalvesOfTheFacePair argues is worse than no valuation
// at all — and the trade dialog captions it «Номинал в , а сделка в RUB».
//
// The instrument's own currency has been held to currencyRe at this same door
// from the start. This is that rule reaching the other currency column.
func TestCreateRefusesAFaceCurrencyThatNamesNoCurrency(t *testing.T) {
	url, c := newAPI(t)

	for _, tc := range []struct{ what, currency string }{
		{"create with an empty face currency", `""`},
		{"create with a lowercase face currency", `"rub"`},
		{"create with a currency name rather than a code", `"RUBLE"`},
		{"create with a face currency of blanks", `"   "`},
	} {
		wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments",
			`{"type":"bond","name":"X","currency":"RUB","face_value_minor":100000,"face_currency":`+tc.currency+`}`),
			faceCurrencyRule, tc.what)
	}
}

// TestUpdateRefusesAFaceCurrencyThatNamesNoCurrency: the same rule at the other
// door, and reached with BOTH halves named so that the mention rule is not what
// answers. A PATCH is the only way an instrument that was created sound can
// become one that is not.
func TestUpdateRefusesAFaceCurrencyThatNamesNoCurrency(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	for _, tc := range []struct{ what, currency string }{
		{"update to an empty face currency", `""`},
		{"update to a lowercase face currency", `"usd"`},
	} {
		wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
			`{"face_value_minor":200000,"face_currency":`+tc.currency+`}`),
			faceCurrencyRule, tc.what)
	}

	// Refused whole: the face VALUE in the same request did not land either.
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "RUB" {
		t.Errorf("face pair after the refusals = %+v, want 100000 RUB untouched", pair)
	}
}

// TestUpdateRefusesToBreakThePair is #93's second half: PATCH validated nothing
// whatsoever, so either half of the pair could be cleared or set on its own.
//
// The rule is stated about the REQUEST, not about the row it lands on: to touch
// either half, send both. That is deliberately stricter than "the result must be
// sound" — it needs no read of the stored row, so it cannot be raced by a
// concurrent PATCH that changes the other half between the read and the write.
//
// And it refuses in its OWN sentence, which is what this test pins by asserting
// the whole string. Creation's «must be set together or not at all» would be a
// true statement here and still a useless one: PATCH {"face_value_minor":200000}
// lands on a bond that stores "RUB", where the two ARE set together both before
// the request and after it, so the sentence describes nothing the client did
// wrong and names nothing for it to do. The thing to do is resend the currency,
// and only a rule about the request can say it.
func TestUpdateRefusesToBreakThePair(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	for _, tc := range []struct{ what, body string }{
		{"clear the currency and leave the value", `{"face_currency":null}`},
		{"clear the value and leave the currency", `{"face_value_minor":null}`},
		{"change the value alone", `{"face_value_minor":200000}`},
		{"change the currency alone", `{"face_currency":"USD"}`},
	} {
		wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
			faceMentionRule, tc.what)
	}

	// Mentioning both and VALUING only one is the other rule, and the one
	// creation shares: both halves are named, so the mention rule has nothing to
	// say, and what is left is a face value with no currency. Without a case here
	// the update side of the shared rule would be pinned by nothing at all.
	for _, tc := range []struct{ what, body string }{
		{"null the value while naming a currency", `{"face_value_minor":null,"face_currency":"USD"}`},
		{"null the currency while naming a value", `{"face_value_minor":200000,"face_currency":null}`},
	} {
		wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
			facePairRule, tc.what)
	}

	// Refused, not half-applied.
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "RUB" {
		t.Errorf("face pair after the refusals = %+v, want 100000 RUB untouched", pair)
	}
}

func TestUpdateRefusesAFaceValueThatIsNotAValue(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	for _, tc := range []struct{ what, body string }{
		{"update to a face value of zero", `{"face_value_minor":0,"face_currency":"RUB"}`},
		{"update to a negative face value", `{"face_value_minor":-1,"face_currency":"RUB"}`},
	} {
		wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
			facePositiveRule, tc.what)
	}

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 {
		t.Errorf("face value after the refusals = %+v, want 100000 untouched", pair)
	}
}

// TestUpdateTakesBothHalvesTogether is the other side of the pairing rule: what
// it asks for has to be possible, or it is not a rule but a wall.
func TestUpdateTakesBothHalvesTogether(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	// Both changed at once.
	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":200000,"face_currency":"USD"}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch both halves = %d, want 200: %s", resp.StatusCode, b)
	}
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 200000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "USD" {
		t.Fatalf("face pair = %+v, want 200000 USD", pair)
	}

	// And both cleared at once, which is how a face value recorded by mistake is
	// taken back.
	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":null,"face_currency":null}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch both halves to null = %d, want 200: %s", resp.StatusCode, b)
	}
	if pair := readFacePair(t, url, c, id); pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after clearing = %+v, want both null", pair)
	}
}

// TestUpdateOfSomethingElseLeavesThePairAlone: a PATCH that mentions neither
// half must not have to mention them. This is the case every other field's
// update is, and a pairing rule written about the resulting ROW rather than
// about the request would be tempted to demand them here.
func TestUpdateOfSomethingElseLeavesThePairAlone(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"name":"ОФЗ 26238 (переименована)","frozen":true}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch of another field = %d, want 200: %s", resp.StatusCode, b)
	}
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "RUB" {
		t.Errorf("face pair = %+v, want 100000 RUB untouched", pair)
	}
}

// A face value was accepted on ANY type of instrument, though the contract has
// said all along that it is null for anything that is not a bond (#101).
// Verified before this test existed: `{"type":"share", ...}` with a face value
// came back 201 and GET published the field.
//
// Nothing computed a wrong figure from it — portfolio.marketValue branches on
// the type and never reads a share's face value, and the trade dialog's
// faceGapOf returns early for a non-bond — so this is not a broken screen. It
// is the document promising one thing and the server accepting another, which
// is what the next client writes its code against.
//
// The CHECK moved to the write rather than the promise being weakened, for the
// reason a face value is already bounded, positive and pair-checked there: a
// bond's quote is a percentage of face, so the pair MEANS something on a bond
// and means nothing anywhere else, and a field that means nothing is one a
// reader will one day read anyway.

// TestCreateRefusesAFaceValueOnAnythingButABond covers every type the catalog
// has, not just the share the issue reported: nothing about the rule is about
// shares, and a check written as `!= bond` and one written as `== share` differ
// on five types nobody would have tested by hand.
func TestCreateRefusesAFaceValueOnAnythingButABond(t *testing.T) {
	url, c := newAPI(t)

	for _, kind := range []string{"share", "etf", "currency", "crypto", "metal", "custom"} {
		wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments",
			fmt.Sprintf(`{"type":%q,"name":"X","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`, kind)),
			faceBondOnlyRule(kind), "create a "+kind+" carrying a face value")
	}

	// HALF a pair is refused by this rule too, and by this rule rather than by
	// the pairing one: on a non-bond neither half belongs, so "set them
	// together" would tell the client to send MORE of what it may not send at
	// all.
	for _, tc := range []struct{ what, body string }{
		{"create a share with a face value alone", `{"type":"share","name":"X","currency":"RUB","face_value_minor":100000}`},
		{"create a share with a face currency alone", `{"type":"share","name":"X","currency":"RUB","face_currency":"RUB"}`},
	} {
		wantFaceRefusal(t, do(t, c, "POST", url+"/api/v1/instruments", tc.body),
			faceBondOnlyRule("share"), tc.what)
	}
}

// TestCreateStillTakesANonBondWithNoFaceValue is the other side: the rule
// refuses a face value on a non-bond, not the non-bond. Every ordinary catalog
// row is one of these.
func TestCreateStillTakesANonBondWithNoFaceValue(t *testing.T) {
	url, c := newAPI(t)

	for _, body := range []string{
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`,
		`{"type":"crypto","name":"Bitcoin","currency":"USD"}`,
		// Explicit nulls are absence, exactly as they are for a bond: a client
		// that sends the whole shape of the object with two nulls in it has not
		// asked for a face value.
		`{"type":"etf","name":"Фонд","currency":"RUB","face_value_minor":null,"face_currency":null}`,
	} {
		if resp := do(t, c, "POST", url+"/api/v1/instruments", body); resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("create %s = %d, want 201: %s", body, resp.StatusCode, b)
		}
	}
}

// mkShare creates an ordinary share carrying no face value and returns its id.
func mkShare(t *testing.T, url string, c *http.Client) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create share = %d: %s", resp.StatusCode, b)
	}
	var share struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&share); err != nil {
		t.Fatalf("decode share: %v", err)
	}
	return share.ID
}

// TestUpdateRefusesAFaceValueOnAnythingButABond closes the other door, and it
// is the door that matters most: an instrument's type cannot be changed
// (UpdateInstrumentRequest has no type and the UPDATE statement does not touch
// the column), so a PATCH is the only way a row created sound could have
// acquired a face value it may not have.
//
// The type comes from the STORED row, which is the one thing a PATCH cannot
// tell the server itself. That read cannot be raced by a concurrent write the
// way a read of the face pair could: there is no code path in this program that
// changes an instrument's type, so what this read sees is what the write will
// land on.
func TestUpdateRefusesAFaceValueOnAnythingButABond(t *testing.T) {
	url, c := newAPI(t)
	id := mkShare(t, url, c)

	wantFaceRefusal(t, do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":100000,"face_currency":"RUB"}`),
		faceBondOnlyRule("share"), "patch a face value onto a share")

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after the refusal = %+v, want both still null", pair)
	}
}

// TestUpdateStillClearsAFaceValueRecordedBeforeTheRule is the repair path, and
// the reason no migration comes with this rule. Rows written before it are
// untouched — the catalog is not rewritten and the server is not stopped over a
// state that computes nothing wrong — so the API has to keep the one action
// that fixes such a row: clearing the pair.
//
// It is why the rule is written about SETTING a face value rather than about
// carrying one. A check that refused any PATCH of the pair on a non-bond would
// leave every such row unfixable through the API, which is a worse state than
// the one it set out to prevent.
func TestUpdateStillClearsAFaceValueRecordedBeforeTheRule(t *testing.T) {
	url, c, store := newAPIWithCatalog(t)

	// Straight through the store, which is how such a row got there: the rule
	// lives at the HTTP door, and the door is what did not have it.
	value := int64(100000)
	code := "RUB"
	legacy, err := store.Create(t.Context(), instrument.Instrument{
		Type: instrument.TypeShare, Name: "Акция с номиналом", Currency: "RUB",
		FaceValueMinor: &value, FaceCurrency: &code,
	})
	if err != nil {
		t.Fatalf("seed the pre-existing row: %v", err)
	}

	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+legacy.ID.String(),
		`{"face_value_minor":null,"face_currency":null}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("clear the pair on a share = %d, want 200: %s", resp.StatusCode, b)
	}
	if pair := readFacePair(t, url, c, legacy.ID.String()); pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after clearing = %+v, want both null", pair)
	}

	// And every other field of such a row stays editable: the rule is about the
	// face pair, not about the instrument.
	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+legacy.ID.String(),
		`{"name":"Переименована"}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("patch another field of such a row = %d, want 200: %s", resp.StatusCode, b)
	}
}

// TestUpdateOfABondsFaceValueIsUnaffected: the rule must not have closed the
// door it exists to keep open. A bond is the one type these two fields belong
// to, and its own PATCH still works.
func TestUpdateOfABondsFaceValueIsUnaffected(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	if resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":200000,"face_currency":"USD"}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch a bond's face pair = %d, want 200: %s", resp.StatusCode, b)
	}
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 200000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "USD" {
		t.Errorf("face pair = %+v, want 200000 USD", pair)
	}
}

// TestUpdateOfAMissingInstrumentIsStill404: the type rule needs the stored row,
// and a rule that reads a row has to say the ordinary thing when there is none.
// A 400 about bonds for an id that does not exist would be a sentence about a
// row the client never had.
func TestUpdateOfAMissingInstrumentIsStill404(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+uuid.NewString(),
		`{"face_value_minor":100000,"face_currency":"RUB"}`)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("patch a face value onto a missing instrument = %d, want 404: %s", resp.StatusCode, b)
	}
}
