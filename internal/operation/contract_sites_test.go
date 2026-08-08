package operation

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The bounds this listing enforces are written down twice: once in Go, as the
// constants parsePage refuses past, and once in api/openapi.yaml, where a client
// reads them. Go cannot import a YAML literal, so the second copy is typed by
// hand — and a change that touched only one of the two would leave the contract
// promising something the server does not do, with every other test in this
// repository still green.
//
// THAT IS NOT A HYPOTHETICAL HERE. It is what #118 was: the document stated
// `maximum: 200` and the server clamped to 200 instead of refusing, so a
// schema-aware client would not send limit=250 that the server would have
// answered, and one that sent it anyway got 200 rows back with nothing saying
// the number it sent was not the number applied. The catalog's copy of this
// file (internal/instrument/contract_sites_test.go) is where that was first
// closed; this is the endpoint it was first FOUND on.
//
// This file does not remove the duplication. It removes the SILENCE. It is its
// own copy rather than a shared helper for the reason parsePage is: the two
// endpoints' bounds are separate numbers that must be free to move separately.
//
// It lives in `package operation` rather than in the `operation_test` package
// beside it precisely so it CAN read those constants. Everything else in this
// directory tests the module through its front door.

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
	// The floor is declarable at all only because parsePage now refuses a
	// negative offset. It used to IGNORE one and answer the first page, and
	// #118 said in as many words that `minimum: 0` could not be declared while
	// that was so — a stated minimum would have described a refusal that did
	// not exist, which is the same defect as the unstated ceiling, pointing the
	// other way.
	if offset.Schema.Minimum == nil || *offset.Schema.Minimum != 0 {
		t.Errorf("GET %s offset.minimum = %s, want 0: parsePage refuses a negative offset",
			journalPath, shownBound(offset.Schema.Minimum))
	}
	if offset.Schema.Default == nil || *offset.Schema.Default != 0 {
		t.Errorf("GET %s offset.default = %s, want 0", journalPath, shownBound(offset.Schema.Default))
	}
}

// TestTheContractStatesTheJournalAnswers400 ties the status code parsePage
// answers to the document that has to name it. This endpoint declared 401 and
// nothing else, which was accurate while it clamped and ignored; a document
// that states bounds and no refusal reads as though the bounds were advisory,
// and that reading is exactly what #118 was.
func TestTheContractStatesTheJournalAnswers400(t *testing.T) {
	doc := readJournalContract(t)
	if _, ok := doc.Paths[journalPath].Get.Responses["400"]; !ok {
		t.Errorf("GET %s declares no 400, but parsePage answers one for a limit or an offset "+
			"outside the bounds beside it", journalPath)
	}
}

// The oldest date an operation may carry is written down four times, in three
// languages, and only one of them is the rule. minOccurredOn is what the
// service refuses past; api/openapi.yaml states it twice, once per request
// schema a client can validate against; and web/src/lib/dates.ts holds a copy
// so the four dialogs that write an operation refuse it in the date field
// instead of after a round trip.
//
// Nothing makes them agree — Go cannot import a YAML literal and TypeScript
// cannot import a Go constant — so a change to the floor that touched only some
// of them would leave a date field refusing what the server takes, or a
// contract promising a range the server does not apply, with every other test
// in this repository still green. That is the shape of gap
// TestTheAmountFieldRefusesAtTheBoundTheServerEnforces closed for
// money.MaxAmountMinor and TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces
// for currency.Pattern; this is the same closure for this bound.
//
// The check is on the DATE AS WRITTEN, not on a parsed structure: all four
// sites spell it YYYY-MM-DD and a reader compares them by eye that way. What it
// cannot check is the prose around it — each site also explains in words what
// the floor is for, and those sentences are read by people. Whoever moves this
// number re-reads them by hand.
// The two sites that write the date out as a literal. In the contract it is
// prose a client reads; in dates.ts it is the frontend's single copy of the
// number, which the dialogs then take by name.
//
// The contract states it on the two REQUEST schemas only. The Operation
// RESPONSE schema's occurred_on deliberately says nothing about a range: it
// describes a row already stored, and rows written before the floor existed are
// untouched and still returned as they stand.
var dateFloorLiteralSites = []string{
	"api/openapi.yaml",
	"web/src/lib/dates.ts",
}

// The four dialogs that write an operation. They are checked for the CONSTANT
// and not for the date: each takes it from dates.ts, which is where the copy
// lives and what the list above ties to the server. A dialog spelling the date
// out itself would be a fifth copy, and this test would not want it.
//
// The balance dialog is deliberately absent — a balance mark has no floor. See
// EARLIEST_OPERATION_DATE in dates.ts for why the two differ.
var dateFloorFormSites = []string{
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
	// Each dialog must actually hand the constant to its date input, not merely
	// import it: a file that names EARLIEST_OPERATION_DATE in an import line and
	// passes nothing would satisfy a check for the name while its field still
	// took any year.
	for _, rel := range dateFloorFormSites {
		body, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), "min={EARLIEST_OPERATION_DATE}") {
			t.Errorf("%s does not pass min={EARLIEST_OPERATION_DATE} to its date input", rel)
		}
	}
	// And the contract states it on BOTH request schemas, not on one of the
	// two: a bound declared at one door and not the other is #100 and #102,
	// where the money cap was declared on a single schema out of four.
	body, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	if got := strings.Count(string(body), "NOT EARLIER THAN "+want); got != 2 {
		t.Errorf("api/openapi.yaml states the %s floor %d times, want 2 "+
			"(CreateOperationRequest.occurred_on and TransferRequest.occurred_on)", want, got)
	}
}
