package operation

import (
	"os"
	"path/filepath"
	"strconv"
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
