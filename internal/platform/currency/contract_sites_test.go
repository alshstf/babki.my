package currency_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/platform/currency"
)

// The contract states currency.Pattern as a `pattern` on every request field
// that writes a currency (#102); this test holds them to the constant.

// Request fields only, listed explicitly: a pattern on a response field would
// promise something about rows stored before the check existed (see #119).
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

// contractDoc reads only what the test needs; the rest stays unparsed.
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
			// A renamed schema: update this list, not a declaration.
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

// Pins what Pattern admits, since elsewhere it is only compared to copies.
func TestValidTakesTheShapeAndNothingElse(t *testing.T) {
	for _, code := range []string{"RUB", "USD", "EUR", "KZT", "XYZ"} {
		if !currency.Valid(code) {
			t.Errorf("Valid(%q) = false, want true", code)
		}
	}
	// XYZ above is deliberate: the register is not consulted.
	for _, code := range []string{"", "rub", "RUBLE", "RU", "   ", "RUB ", " RUB", "RUB\nUSD", "R U"} {
		if currency.Valid(code) {
			t.Errorf("Valid(%q) = true, want false", code)
		}
	}
}
