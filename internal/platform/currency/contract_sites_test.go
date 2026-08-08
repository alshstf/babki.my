package currency_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/platform/currency"
)

// The shape of a currency code is stated in two languages: currency.Pattern,
// which the four doors that write one are held to, and a `pattern` in
// api/openapi.yaml, which is the only form of it a client can check a request
// against before sending. Go cannot import a YAML scalar and a JSON-Schema
// validator cannot import a Go constant, so the second is typed by hand — and
// until #102 it was typed on ONE field out of five. A client validating against
// the document could check a bond's face currency and not the currency of the
// account, the instrument, the operation or the space, even though the server
// holds all five to the same rule and answers 400 alike.
//
// This test does not remove the duplication; it removes the silence. Change the
// rule in Go and it names every field in the document that still states the old
// one; add a currency field to the contract without declaring the shape and the
// list below is where a reader is told to add it.

// The fields the contract has to declare it on, spelled out rather than
// discovered by walking the document for anything named like a currency. A
// discovered list would grow silently — a new field appearing in the contract
// would simply be checked, or, worse, a RESPONSE field would be swept in, and
// this test would then demand a promise about rows already stored (see the note
// on responses below).
//
// Requests only, and that is the whole line. A `pattern` on a request field says
// what the server will refuse today, which is exactly what these four doors do.
// A `pattern` on a response field would say something else — that no row this
// API can publish has any other shape — and that is a claim about the
// database, which holds rows written before these checks existed and rows the
// seed wrote through the store rather than through a handler. The contract
// already draws this line for money: SetBalanceRequest.amount_minor carries the
// cap and BalancePoint.amount_minor, the same figure coming back, carries none.
//
// The document had one counter-example when this list was written —
// Instrument.face_currency, a RESPONSE field carrying this pattern with nothing
// but the write doors behind it — and #119 removed it. So the line is now true
// of every currency in this contract, and the assertion keeping that response
// field on the right side of it lives in
// internal/instrument/contract_sites_test.go, beside the migration it rests on.
var declaredOn = []struct {
	schema, field string
	door          string // the code that refuses at that door
}{
	{"CreateAccountRequest", "currency", "internal/account/http.go, handleCreate"},
	{"CreateInstrumentRequest", "currency", "internal/instrument/http.go, handleCreate"},
	{"CreateInstrumentRequest", "face_currency", "internal/instrument/http.go, checkFacePair"},
	{"UpdateInstrumentRequest", "face_currency", "internal/instrument/http.go, checkFaceUpdate"},
	{"CreateOperationRequest", "currency", "internal/operation/service.go, validate"},
	{"UpdateSpaceRequest", "base_currency", "internal/family/auth.go, UpdateSpace"},
}

// schemaProperty is the document read as far as this test needs it: a schema by
// name, a property by name, and the one keyword. yaml.Node keeps the rest
// unparsed, so a change anywhere else in the contract cannot break this.
type contractDoc struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Pattern *string `yaml:"pattern"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func TestTheContractStatesTheCurrencyShapeTheServerEnforces(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	var doc contractDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}

	for _, site := range declaredOn {
		schema, ok := doc.Components.Schemas[site.schema]
		if !ok {
			// A renamed schema, not a wrong pattern. Reported apart so the fix is
			// the right one: teach this list the new name rather than change any
			// declaration.
			t.Errorf("api/openapi.yaml has no schema %s; this list names the request schemas that carry a currency "+
				"(refused in %s) — if it was renamed, rename it here too", site.schema, site.door)
			continue
		}
		prop, ok := schema.Properties[site.field]
		if !ok {
			t.Errorf("api/openapi.yaml %s has no property %s (refused in %s)", site.schema, site.field, site.door)
			continue
		}
		if prop.Pattern == nil {
			t.Errorf("api/openapi.yaml %s.%s declares no pattern; the server refuses anything but %s at this door (%s), "+
				"and a client validating against the contract can only check a rule the contract carries",
				site.schema, site.field, currency.Pattern, site.door)
			continue
		}
		if *prop.Pattern != currency.Pattern {
			t.Errorf("api/openapi.yaml %s.%s pattern = %q, want %q (currency.Pattern, enforced in %s): "+
				"the document and the server would refuse different strings",
				site.schema, site.field, *prop.Pattern, currency.Pattern, site.door)
		}
	}
}

// TestValidTakesTheShapeAndNothingElse pins what Pattern actually admits,
// because the string above is compared against the contract and never against
// an example. A pattern the test only compares to itself would agree with any
// rule at all.
func TestValidTakesTheShapeAndNothingElse(t *testing.T) {
	for _, code := range []string{"RUB", "USD", "EUR", "KZT", "XYZ"} {
		if !currency.Valid(code) {
			t.Errorf("Valid(%q) = false, want true", code)
		}
	}
	// XYZ above is deliberate: the register is not consulted (see the package
	// doc), and a test that omitted the case would let a later reading of the
	// rule as "an assigned currency" pass unnoticed.
	for _, code := range []string{"", "rub", "RUBLE", "RU", "   ", "RUB ", " RUB", "RUB\nUSD", "R U"} {
		if currency.Valid(code) {
			t.Errorf("Valid(%q) = true, want false", code)
		}
	}
}
