package operation

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The listing's bounds are written in Go (parsePage) and in api/openapi.yaml,
// which Go cannot import, so these tests keep the two in step (#118). The
// catalog has its own copy: the two endpoints' numbers move separately. Package
// operation, not operation_test, so it can read the constants.

func contractFile(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	return body
}

type journalParam struct {
	Name   string `yaml:"name"`
	In     string `yaml:"in"`
	Schema struct {
		Default *int `yaml:"default"`
		Minimum *int `yaml:"minimum"`
		Maximum *int `yaml:"maximum"`
	} `yaml:"schema"`
}

type journalContract struct {
	Paths map[string]struct {
		Get struct {
			Parameters []journalParam `yaml:"parameters"`
			Responses  map[string]any `yaml:"responses"`
		} `yaml:"get"`
	} `yaml:"paths"`
}

func shownBound(v *int) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(*v)
}

const journalPath = "/api/v1/accounts/{accountId}/operations"

func readJournalContract(t *testing.T) journalContract {
	t.Helper()
	var doc journalContract
	if err := yaml.Unmarshal(contractFile(t), &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	return doc
}

func TestTheContractStatesTheJournalPageBoundsTheServerEnforces(t *testing.T) {
	doc := readJournalContract(t)
	item, ok := doc.Paths[journalPath]
	if !ok {
		t.Fatalf("api/openapi.yaml declares no GET %s", journalPath)
	}
	params := map[string]journalParam{}
	for _, p := range item.Get.Parameters {
		if p.In == "query" {
			params[p.Name] = p
		}
	}

	limit, ok := params["limit"]
	if !ok {
		t.Errorf("GET %s declares no `limit` query parameter, but the server reads one", journalPath)
	} else {
		if limit.Schema.Maximum == nil || *limit.Schema.Maximum != maxListLimit {
			t.Errorf("GET %s limit.maximum = %s, want %d (maxListLimit, refused past it in parsePage): "+
				"a ceiling the contract states and the server does not apply is #118, and this endpoint "+
				"is where it was found", journalPath, shownBound(limit.Schema.Maximum), maxListLimit)
		}
		if limit.Schema.Minimum == nil || *limit.Schema.Minimum != 1 {
			t.Errorf("GET %s limit.minimum = %s, want 1: parsePage refuses 0 and below, and "+
				"Store.ListByAccount refuses a limit under 1 behind it",
				journalPath, shownBound(limit.Schema.Minimum))
		}
		if limit.Schema.Default == nil || *limit.Schema.Default != defaultListLimit {
			t.Errorf("GET %s limit.default = %s, want %d (defaultListLimit, what parsePage uses when "+
				"the parameter is absent)", journalPath, shownBound(limit.Schema.Default), defaultListLimit)
		}
	}

	offset, ok := params["offset"]
	if !ok {
		t.Fatalf("GET %s declares no `offset` query parameter, but the server reads one", journalPath)
	}
	// minimum: 0 is declarable because parsePage refuses a negative offset.
	if offset.Schema.Minimum == nil || *offset.Schema.Minimum != 0 {
		t.Errorf("GET %s offset.minimum = %s, want 0: parsePage refuses a negative offset",
			journalPath, shownBound(offset.Schema.Minimum))
	}
	if offset.Schema.Default == nil || *offset.Schema.Default != 0 {
		t.Errorf("GET %s offset.default = %s, want 0", journalPath, shownBound(offset.Schema.Default))
	}
}

// The contract names the 400 parsePage answers; bounds with no refusal
// read as advisory (#118).
func TestTheContractStatesTheJournalAnswers400(t *testing.T) {
	doc := readJournalContract(t)
	if _, ok := doc.Paths[journalPath].Get.Responses["400"]; !ok {
		t.Errorf("GET %s declares no 400, but parsePage answers one for a limit or an offset "+
			"outside the bounds beside it", journalPath)
	}
}

// The oldest operation date lives in four places: minOccurredOn (the rule),
// api/openapi.yaml twice (one per request schema), and web/src/lib/dates.ts for
// the dialogs. Nothing else makes them agree. Checked as written (YYYY-MM-DD);
// the prose around each must be re-read by hand when the date moves.
//
// These are the two literal sites. The Operation response schema states no range:
// rows written before the floor are returned as they are.
var dateFloorLiteralSites = []string{
	"api/openapi.yaml",
	"web/src/lib/dates.ts",
}

// The shared date field takes the constant from dates.ts; a field spelling
// the date itself would be another copy. The balance dialog has no floor (see
// EARLIEST_OPERATION_DATE).
var dateFloorFormSites = []string{
	"web/src/components/form-fields.tsx",
}

// dateFloorFieldSites are the dialogs that use the shared field.
var dateFloorFieldSites = []string{
	"web/src/routes/accounts/trade-dialog.tsx",
	"web/src/routes/accounts/transfer-dialog.tsx",
	"web/src/routes/accounts/income-dialog.tsx",
	"web/src/routes/accounts/cash-dialog.tsx",
}

func TestTheContractAndTheDateFieldsStateTheFloorTheServerEnforces(t *testing.T) {
	want := minOccurredOn.Format("2006-01-02")
	for _, rel := range dateFloorLiteralSites {
		body, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not mention %s (minOccurredOn): a date field or a contract that "+
				"disagrees with the floor either refuses what the server accepts or accepts "+
				"what it refuses", rel, want)
		}
	}
	// The constant must reach the input, not merely be imported.
	for _, rel := range dateFloorFormSites {
		body, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), "min={EARLIEST_OPERATION_DATE}") {
			t.Errorf("%s does not pass min={EARLIEST_OPERATION_DATE} to its date input", rel)
		}
	}
	for _, rel := range dateFloorFieldSites {
		body, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), "<OperationDateField") {
			t.Errorf("%s does not take its date from OperationDateField", rel)
		}
	}
	// Both request schemas state it (#100, #102: a bound on one door of
	// several).
	body, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	if got := strings.Count(string(body), "NOT EARLIER THAN "+want); got != 2 {
		t.Errorf("api/openapi.yaml states the %s floor %d times, want 2 "+
			"(CreateOperationRequest.occurred_on and TransferRequest.occurred_on)", want, got)
	}
}
