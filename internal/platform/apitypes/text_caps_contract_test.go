package apitypes_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// The ceilings on free text are written down twice, as the credential rules
// are (see internal/family/contract_sites_test.go): once in Go, where the
// refusal is, and once in api/openapi.yaml as maxLength, where a client reads
// it before sending anything. This test is the one place both copies meet, so
// it lives in a package every module's constant can be imported into.
//
// Changing a ceiling in Go names the declaration that still states the old
// one; a new text field on a request is added here along with its ceiling.
func TestTheContractStatesTheTextCeilingsTheServerEnforces(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					MaxLength *int `yaml:"maxLength"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}

	sites := []struct {
		schema, field string
		limit         int
		rule          string
	}{
		{"CreateOperationRequest", "note", operation.MaxNoteRunes, "operation.MaxNoteRunes"},
		{"CreateArrivalRequest", "note", operation.MaxNoteRunes, "operation.MaxNoteRunes"},
		{"TransferRequest", "note", operation.MaxNoteRunes, "operation.MaxNoteRunes"},
		{"CreateAccountRequest", "name", account.MaxNameRunes, "account.MaxNameRunes"},
		{"UpdateAccountRequest", "name", account.MaxNameRunes, "account.MaxNameRunes"},
		{"CreateAccountRequest", "institution", account.MaxInstitutionRunes, "account.MaxInstitutionRunes"},
		{"UpdateAccountRequest", "institution", account.MaxInstitutionRunes, "account.MaxInstitutionRunes"},
		{"CreateInstrumentRequest", "name", instrument.MaxNameRunes, "instrument.MaxNameRunes"},
		{"UpdateInstrumentRequest", "name", instrument.MaxNameRunes, "instrument.MaxNameRunes"},
		{"CreateInstrumentRequest", "ticker", instrument.MaxTickerRunes, "instrument.MaxTickerRunes"},
		{"UpdateInstrumentRequest", "ticker", instrument.MaxTickerRunes, "instrument.MaxTickerRunes"},
		{"CreateInstrumentRequest", "figi", instrument.MaxFIGIRunes, "instrument.MaxFIGIRunes"},
		{"UpdateInstrumentRequest", "figi", instrument.MaxFIGIRunes, "instrument.MaxFIGIRunes"},
		{"CreateInstrumentEventRequest", "note", corporateaction.MaxNoteRunes, "corporateaction.MaxNoteRunes"},
		{"CreateInstrumentEventRequest", "source_ref", corporateaction.MaxSourceRefRunes, "corporateaction.MaxSourceRefRunes"},
		{"SetupRequest", "space_name", family.MaxNameRunes, "family.MaxNameRunes"},
		{"SetupRequest", "display_name", family.MaxNameRunes, "family.MaxNameRunes"},
		{"CreateMemberRequest", "display_name", family.MaxNameRunes, "family.MaxNameRunes"},
	}
	for _, s := range sites {
		schema, ok := doc.Components.Schemas[s.schema]
		if !ok {
			t.Errorf("api/openapi.yaml has no %s schema", s.schema)
			continue
		}
		prop, ok := schema.Properties[s.field]
		if !ok {
			t.Errorf("%s has no %q property, but the server reads one", s.schema, s.field)
			continue
		}
		shown := "absent"
		if prop.MaxLength != nil {
			shown = strconv.Itoa(*prop.MaxLength)
		}
		if prop.MaxLength == nil || *prop.MaxLength != s.limit {
			t.Errorf("%s.%s maxLength = %s, want %d (%s, refused past it)", s.schema, s.field, shown, s.limit, s.rule)
		}
	}
}
