package instrument

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// The bounds this endpoint enforces are written down twice: once in Go, as the
// constants parsePage refuses past, and once in api/openapi.yaml, where a client
// reads them. Go cannot import a YAML literal, so the second copy is typed by
// hand — and a change that touched only one of the two would leave the contract
// promising something the server does not do, with every other test in this
// repository still green. That is #120's lesson and #118's, and this endpoint is
// where #118 was actually found: the document said `maximum: 200` while the
// server quietly clamped instead of refusing, so the ceiling was a description
// of nothing.
//
// This file does not remove the duplication. It removes the SILENCE. It is the
// instrument catalog's own copy of internal/importer/tinvest/contract_sites_test.go
// rather than a shared helper, for the reason parsePage is its own copy: the two
// paths' bounds are separate numbers that must be free to move separately.
//
// It lives in `package instrument` rather than in the `instrument_test` package
// beside it precisely so it CAN read those constants. Everything else in this
// directory tests the module through its front door.

// repoFile reads a path relative to the repository root. Tests run with their
// own package directory as the working directory, and the contract lives outside
// the Go tree entirely.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return body
}

type contractParam struct {
	Name   string `yaml:"name"`
	In     string `yaml:"in"`
	Schema struct {
		Type    string `yaml:"type"`
		Default *int   `yaml:"default"`
		Minimum *int   `yaml:"minimum"`
		Maximum *int   `yaml:"maximum"`
	} `yaml:"schema"`
}

type contractDoc struct {
	Paths map[string]struct {
		Get struct {
			Parameters []contractParam `yaml:"parameters"`
			Responses  map[string]any  `yaml:"responses"`
		} `yaml:"get"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Required []string `yaml:"required"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func shown(v *int) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(*v)
}

const catalogPath = "/api/v1/instruments"

func readContract(t *testing.T) contractDoc {
	t.Helper()
	var doc contractDoc
	if err := yaml.Unmarshal(repoFile(t, "api/openapi.yaml"), &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	return doc
}

func TestTheContractStatesTheCatalogPageBoundsTheServerEnforces(t *testing.T) {
	doc := readContract(t)
	item, ok := doc.Paths[catalogPath]
	if !ok {
		t.Fatalf("api/openapi.yaml declares no GET %s", catalogPath)
	}
	params := map[string]contractParam{}
	for _, p := range item.Get.Parameters {
		if p.In == "query" {
			params[p.Name] = p
		}
	}

	// limit: default, floor and ceiling, all three enforced in parsePage, all
	// three declared there.
	limit, ok := params["limit"]
	if !ok {
		t.Errorf("GET %s declares no `limit` query parameter, but the server reads one", catalogPath)
	} else {
		if limit.Schema.Maximum == nil || *limit.Schema.Maximum != maxSearchLimit {
			t.Errorf("GET %s limit.maximum = %s, want %d (maxSearchLimit, refused past it in parsePage): "+
				"a ceiling the contract states and the server does not apply is #118, and this "+
				"endpoint is where it was found", catalogPath, shown(limit.Schema.Maximum), maxSearchLimit)
		}
		if limit.Schema.Minimum == nil || *limit.Schema.Minimum != 1 {
			t.Errorf("GET %s limit.minimum = %s, want 1: parsePage refuses 0 and below, "+
				"and Store.Search refuses a limit under 1 behind it",
				catalogPath, shown(limit.Schema.Minimum))
		}
		if limit.Schema.Default == nil || *limit.Schema.Default != defaultSearchLimit {
			t.Errorf("GET %s limit.default = %s, want %d (defaultSearchLimit, what parsePage uses "+
				"when the parameter is absent)", catalogPath, shown(limit.Schema.Default), defaultSearchLimit)
		}
	}

	offset, ok := params["offset"]
	if !ok {
		t.Fatalf("GET %s declares no `offset` query parameter, but the server reads one — "+
			"and its absence from the document was half of #104", catalogPath)
	}
	if offset.Schema.Minimum == nil || *offset.Schema.Minimum != 0 {
		t.Errorf("GET %s offset.minimum = %s, want 0: parsePage refuses a negative offset",
			catalogPath, shown(offset.Schema.Minimum))
	}
	if offset.Schema.Default == nil || *offset.Schema.Default != 0 {
		t.Errorf("GET %s offset.default = %s, want 0", catalogPath, shown(offset.Schema.Default))
	}
}

// TestTheContractStatesTheCatalogAnswers400 ties the status code parsePage
// answers to the document that has to name it. Without the declaration a client
// meeting a 400 here has nothing to read it by — and, worse for this particular
// path, a document that declares bounds but no refusal reads as though the
// bounds were advisory, which is the state #118 was about.
func TestTheContractStatesTheCatalogAnswers400(t *testing.T) {
	doc := readContract(t)
	if _, ok := doc.Paths[catalogPath].Get.Responses["400"]; !ok {
		t.Errorf("GET %s declares no 400, but parsePage answers one for a limit or an offset "+
			"outside the bounds beside it", catalogPath)
	}
}

// TestTheContractStatesTheCatalogPageIsAnEnvelope holds the response shape to
// the one thing that makes paging usable: a page that says whether there is
// more. The bare array this used to return could not, and no `offset` in the
// world helps a client that cannot tell when to stop asking (#104).
func TestTheContractStatesTheCatalogPageIsAnEnvelope(t *testing.T) {
	doc := readContract(t)
	schema, ok := doc.Components.Schemas["InstrumentsResponse"]
	if !ok {
		t.Fatal("api/openapi.yaml has no InstrumentsResponse schema")
	}
	for _, field := range []string{"instruments", "has_more"} {
		found := false
		for _, r := range schema.Required {
			if r == field {
				found = true
			}
		}
		if !found {
			t.Errorf("InstrumentsResponse does not require %q (requires %v): the handler always "+
				"writes it, and a field a client has to treat as optional is a field it will "+
				"read as false when it is missing", field, schema.Required)
		}
	}
}
