package instrument_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
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

const (
	facePairRule     = "face_value_minor and face_currency must be set together or not at all"
	faceMentionRule  = "face_value_minor and face_currency must be sent together, even to change one"
	facePositiveRule = "face_value_minor must be positive"
	faceCurrencyRule = "face_currency must be ISO-4217 uppercase"
)

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
