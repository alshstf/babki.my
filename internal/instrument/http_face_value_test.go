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
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/money"
)

// Face value rules at both write doors (#93): a bond's quote is a percentage
// of face, so a zero face value values the holding at nothing, and a pair
// broken by a PATCH cannot be priced.

// wantFaceRefusal asserts the whole message: the rules concern the same
// fields, so only the full sentence tells them apart.
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

// faceBondOnlyRule is the non-bond refusal, naming the type it found.
func faceBondOnlyRule(instrumentType string) string {
	return fmt.Sprintf("face_value_minor and face_currency belong to a bond; this instrument is a %s", instrumentType)
}

// mkBond creates an ordinary bond with a sound face value and returns its id.
func mkBond(t *testing.T, url string, c *http.Client) string {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url+"/api/v1/instruments",
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

// readFacePair reads the stored pair, to tell a refusal that changed nothing
// from one that wrote first.
func readFacePair(t *testing.T, url string, c *http.Client, id string) facePair {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/instruments", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list instruments = %d", resp.StatusCode)
	}
	var out struct {
		Instruments []struct {
			ID string `json:"id"`
			facePair
		} `json:"instruments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	for _, i := range out.Instruments {
		if i.ID == id {
			return i.facePair
		}
	}
	t.Fatalf("instrument %s is not in the catalog", id)
	return facePair{}
}

func TestCreateRefusesAFaceValueThatIsNotAValue(t *testing.T) {
	url, c := newAPI(t)

	// Zero prices the whole holding at nothing.
	wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"X","currency":"RUB","face_value_minor":0,"face_currency":"RUB"}`),
		facePositiveRule, "create with a face value of zero")

	// And negative, which would price the holding below nothing.
	wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"X","currency":"RUB","face_value_minor":-100000,"face_currency":"RUB"}`),
		facePositiveRule, "create with a negative face value")
}

// One minor unit is a valid face value.
func TestCreateTakesTheSmallestRealFaceValue(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"bond","name":"Однокопеечная","currency":"RUB","face_value_minor":1,"face_currency":"RUB"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with a face value of one minor unit = %d, want 201: %s", resp.StatusCode, b)
	}
}

// money.MaxAmountMinor itself is accepted, as for amounts and balances.
func TestCreateTakesTheLargestRealFaceValue(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		fmt.Sprintf(`{"type":"bond","name":"Крупный номинал","currency":"RUB","face_value_minor":%d,"face_currency":"RUB"}`,
			money.MaxAmountMinor))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create with a face value of money.MaxAmountMinor = %d, want 201: %s", resp.StatusCode, b)
	}
}

// A face value above money.MaxAmountMinor is refused: unbounded, it made the
// positions screen fail forever.
func TestCreateRefusesAFaceValueTooLarge(t *testing.T) {
	url, c := newAPI(t)

	for _, tc := range []struct {
		what  string
		value int64
	}{
		{"create with a face value one minor unit past the cap", money.MaxAmountMinor + 1},
		{"create with a face value of math.MaxInt64", math.MaxInt64},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments",
			fmt.Sprintf(`{"type":"bond","name":"X","currency":"RUB","face_value_minor":%d,"face_currency":"RUB"}`, tc.value)),
			faceTooLargeRule, tc.what)
	}
}

// The same bound on update.
func TestUpdateRefusesAFaceValueTooLarge(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		fmt.Sprintf(`{"face_value_minor":%d,"face_currency":"RUB"}`, money.MaxAmountMinor+1)),
		faceTooLargeRule, "update to a face value one minor unit past the cap")

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 {
		t.Errorf("face value after the refusal = %+v, want 100000 untouched", pair)
	}
}

// An instrument with no face value, a bond with none yet included, is an
// ordinary row; explicit nulls are absence.
func TestCreateStillTakesAnInstrumentWithNoFaceValue(t *testing.T) {
	url, c := newAPI(t)

	for _, body := range []string{
		`{"type":"bond","name":"Без номинала","currency":"RUB"}`,
		`{"type":"bond","name":"Явные null","currency":"RUB","face_value_minor":null,"face_currency":null}`,
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`,
		`{"type":"crypto","name":"Bitcoin","currency":"USD"}`,
		`{"type":"etf","name":"Фонд","currency":"RUB","face_value_minor":null,"face_currency":null}`,
	} {
		if resp := apitest.Do(t, c, "POST", url+"/api/v1/instruments", body); resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("create %s = %d, want 201: %s", body, resp.StatusCode, b)
		}
	}
}

// An empty face currency is refused: it is neither null nor NULL, so the
// pair rule and the CHECK both passed it, and a bond would be valued in no
// currency.
func TestCreateRefusesAFaceCurrencyThatNamesNoCurrency(t *testing.T) {
	url, c := newAPI(t)

	for _, tc := range []struct{ what, currency string }{
		{"create with an empty face currency", `""`},
		{"create with a lowercase face currency", `"rub"`},
		{"create with a currency name rather than a code", `"RUBLE"`},
		{"create with a face currency of blanks", `"   "`},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments",
			`{"type":"bond","name":"X","currency":"RUB","face_value_minor":100000,"face_currency":`+tc.currency+`}`),
			faceCurrencyRule, tc.what)
	}
}

// The same rule on update, with both halves named so the mention rule does not
// answer.
func TestUpdateRefusesAFaceCurrencyThatNamesNoCurrency(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	for _, tc := range []struct{ what, currency string }{
		{"update to an empty face currency", `""`},
		{"update to a lowercase face currency", `"usd"`},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
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

// A PATCH touching either half must send both, in a message of its own:
// creation's "set together" would be true and useless here. The rule is about
// the request, so a concurrent PATCH cannot race it.
func TestUpdateRefusesToBreakThePair(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	for _, tc := range []struct{ what, body string }{
		{"clear the currency and leave the value", `{"face_currency":null}`},
		{"clear the value and leave the currency", `{"face_value_minor":null}`},
		{"change the value alone", `{"face_value_minor":200000}`},
		{"change the currency alone", `{"face_currency":"USD"}`},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
			faceMentionRule, tc.what)
	}

	// Both named, one valued: the shared value-and-currency rule answers.
	for _, tc := range []struct{ what, body string }{
		{"null the value while naming a currency", `{"face_value_minor":null,"face_currency":"USD"}`},
		{"null the currency while naming a value", `{"face_value_minor":200000,"face_currency":null}`},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
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
		wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id, tc.body),
			facePositiveRule, tc.what)
	}

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 100000 {
		t.Errorf("face value after the refusals = %+v, want 100000 untouched", pair)
	}
}

// Both halves together are accepted.
func TestUpdateTakesBothHalvesTogether(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	// Both changed at once.
	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":200000,"face_currency":"USD"}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch both halves = %d, want 200: %s", resp.StatusCode, b)
	}
	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor == nil || *pair.FaceValueMinor != 200000 ||
		pair.FaceCurrency == nil || *pair.FaceCurrency != "USD" {
		t.Fatalf("face pair = %+v, want 200000 USD", pair)
	}

	// Both cleared at once is accepted.
	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":null,"face_currency":null}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch both halves to null = %d, want 200: %s", resp.StatusCode, b)
	}
	if pair := readFacePair(t, url, c, id); pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after clearing = %+v, want both null", pair)
	}
}

// A PATCH of other fields need not mention the pair.
func TestUpdateOfSomethingElseLeavesThePairAlone(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
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

// A face value belongs only to a bond (#101): it was accepted on any type,
// though the contract says null otherwise. The check sits at the write.

// Every non-bond type is refused, not just a share.
func TestCreateRefusesAFaceValueOnAnythingButABond(t *testing.T) {
	url, c := newAPI(t)

	for _, kind := range []string{"share", "etf", "currency", "crypto", "metal", "custom"} {
		wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments",
			fmt.Sprintf(`{"type":%q,"name":"X","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`, kind)),
			faceBondOnlyRule(kind), "create a "+kind+" carrying a face value")
	}

	// Half a pair on a non-bond gets the bond-only refusal, not "set them
	// together".
	for _, tc := range []struct{ what, body string }{
		{"create a share with a face value alone", `{"type":"share","name":"X","currency":"RUB","face_value_minor":100000}`},
		{"create a share with a face currency alone", `{"type":"share","name":"X","currency":"RUB","face_currency":"RUB"}`},
	} {
		wantFaceRefusal(t, apitest.Do(t, c, "POST", url+"/api/v1/instruments", tc.body),
			faceBondOnlyRule("share"), tc.what)
	}
}

// mkShare creates an ordinary share carrying no face value and returns its id.
func mkShare(t *testing.T, url string, c *http.Client) string {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url+"/api/v1/instruments",
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

// The update refuses it too, judging the type from the stored row, which no
// writer can change.
func TestUpdateRefusesAFaceValueOnAnythingButABond(t *testing.T) {
	url, c := newAPI(t)
	id := mkShare(t, url, c)

	wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":100000,"face_currency":"RUB"}`),
		faceBondOnlyRule("share"), "patch a face value onto a share")

	pair := readFacePair(t, url, c, id)
	if pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after the refusal = %+v, want both still null", pair)
	}

	// Half a pair on a share pins the order of the checks: the type rule answers
	// before the mention rule.
	wantFaceRefusal(t, apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
		`{"face_value_minor":100000}`),
		faceBondOnlyRule("share"), "patch a face value alone onto a share")

	pair = readFacePair(t, url, c, id)
	if pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after the refusal = %+v, want both still null", pair)
	}
}

// A row written before the rule can still have its pair cleared: the rule is
// about setting a face value, so the repair stays possible without a
// migration.
func TestUpdateStillClearsAFaceValueRecordedBeforeTheRule(t *testing.T) {
	url, c, store := newAPIWithCatalog(t)

	// Through the store, as such rows were written.
	value := int64(100000)
	code := "RUB"
	legacy, err := store.Create(t.Context(), instrument.Instrument{
		Type: instrument.TypeShare, Name: "Акция с номиналом", Currency: "RUB",
		FaceValueMinor: &value, FaceCurrency: &code,
	})
	if err != nil {
		t.Fatalf("seed the pre-existing row: %v", err)
	}

	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+legacy.ID.String(),
		`{"face_value_minor":null,"face_currency":null}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("clear the pair on a share = %d, want 200: %s", resp.StatusCode, b)
	}
	if pair := readFacePair(t, url, c, legacy.ID.String()); pair.FaceValueMinor != nil || pair.FaceCurrency != nil {
		t.Errorf("face pair after clearing = %+v, want both null", pair)
	}

	// Other fields of such a row stay editable.
	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+legacy.ID.String(),
		`{"name":"Переименована"}`); resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("patch another field of such a row = %d, want 200: %s", resp.StatusCode, b)
	}
}

// A bond's own face value update still works.
func TestUpdateOfABondsFaceValueIsUnaffected(t *testing.T) {
	url, c := newAPI(t)
	id := mkBond(t, url, c)

	if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id,
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

// A missing instrument is still a 404, not a bond rule.
func TestUpdateOfAMissingInstrumentIsStill404(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+uuid.NewString(),
		`{"face_value_minor":100000,"face_currency":"RUB"}`)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("patch a face value onto a missing instrument = %d, want 404: %s", resp.StatusCode, b)
	}
}
