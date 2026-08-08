package instrument

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			Required   []string `yaml:"required"`
			Properties map[string]struct {
				Pattern   *string `yaml:"pattern"`
				MinLength *int    `yaml:"minLength"`
				Minimum   *int    `yaml:"minimum"`
				Maximum   *int    `yaml:"maximum"`
			} `yaml:"properties"`
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

// TestTheStoredInstrumentPromisesOnlyWhatTheTableGuarantees is #119, and it is
// a rule about the DIRECTION a constraint travels rather than about any one
// keyword.
//
// A `pattern` or a floor on a REQUEST field says what the server will refuse
// today, and the doors are the whole of what makes it true. The same keyword on
// a RESPONSE field says something much larger: that no row this API can publish
// has any other shape — a claim about the table, which holds rows written before
// the doors did, and rows the seed wrote through the store rather than through a
// handler. Only a database constraint can make such a claim true.
//
// Instrument.face_currency carried `^[A-Z]{3}$` and nothing backed it. Every
// writer does check that shape — the two doors here and the T-Invest resolver,
// which writes catalog rows against Store with no handler on its path — but
// each checks it separately, none of them from before #93, and migration 0012
// says in as many words that it deliberately left the SHAPE to the writers and
// constrained the emptiness alone. A row written earlier as "rub" would still
// be published against a pattern promising it could not exist.
//
// What replaces it is the part the table really does guarantee. The line is the
// one already drawn on face_value_minor beside it, which keeps `minimum: 1`
// (CHECK: face_value_minor > 0) and drops the `maximum` its door alone
// enforces.
func TestTheStoredInstrumentPromisesOnlyWhatTheTableGuarantees(t *testing.T) {
	doc := readContract(t)
	stored, ok := doc.Components.Schemas["Instrument"]
	if !ok {
		t.Fatal("api/openapi.yaml has no Instrument schema")
	}

	face, ok := stored.Properties["face_currency"]
	if !ok {
		t.Fatal("api/openapi.yaml Instrument has no face_currency property")
	}
	if face.Pattern != nil {
		t.Errorf("api/openapi.yaml Instrument.face_currency declares pattern %q, want none (#119): "+
			"that is a promise about every row already stored, and only the write doors check the "+
			"shape — migration 0012 constrains this column's emptiness and deliberately not its shape, "+
			"so a row written before #93 as \"rub\" would be published against it",
			*face.Pattern)
	}
	// The floor IS declarable, because migration 0012's CHECK keeps '' out of
	// the column. Asserted rather than assumed: dropping the pattern and
	// declaring nothing at all would lose a guarantee the table really gives.
	if face.MinLength == nil || *face.MinLength != 1 {
		t.Errorf("api/openapi.yaml Instrument.face_currency minLength = %s, want 1: "+
			"migration 0012's CHECK (face_currency <> '') makes that true of every row in the table, "+
			"and '' is the value that denominates a bond's face in nothing while passing every "+
			"presence check", shown(face.MinLength))
	}

	// Its twin, which is the precedent this follows and would be a silent
	// counter-example if it ever drifted.
	value, ok := stored.Properties["face_value_minor"]
	if !ok {
		t.Fatal("api/openapi.yaml Instrument has no face_value_minor property")
	}
	if value.Minimum == nil || *value.Minimum != 1 {
		t.Errorf("api/openapi.yaml Instrument.face_value_minor minimum = %s, want 1 "+
			"(migration 0012's CHECK: face_value_minor > 0)", shown(value.Minimum))
	}
	if value.Maximum != nil {
		t.Errorf("api/openapi.yaml Instrument.face_value_minor declares maximum %d, want none: "+
			"the ceiling is a write-time check with no CHECK constraint behind it, so it is not "+
			"something this response can say about a row already stored", *value.Maximum)
	}
}

// TestTheMigrationStillBacksWhatTheResponsePromises reads the constraint the
// test above rests its floor on.
//
// Without this, `minLength: 1` on a response is tied to a sentence in a comment.
// Migration 0012 could have its empty-string clause dropped — the whole file
// could be rewritten — and nothing in this repository would notice that the
// contract had gone back to promising something about the table that the table
// no longer enforces. That is #119 in the other direction, and it would be
// invisible for exactly the same reason the first direction was.
func TestTheMigrationStillBacksWhatTheResponsePromises(t *testing.T) {
	const rel = "internal/platform/db/migrations/0012_instruments_face_value_sound.sql"
	body := string(repoFile(t, rel))
	// The clause as the migration writes it. Matched as text rather than parsed:
	// this fails the way a human diff would, and the fix for a reformat is to
	// teach this test the new spelling rather than to change any declaration.
	const clause = "face_currency IS NULL OR face_currency <> ''"
	if !strings.Contains(body, clause) {
		t.Errorf("%s no longer contains %q. Instrument.face_currency declares minLength: 1, "+
			"which is a promise about every row in the table and rests on this CHECK; if the "+
			"constraint was reworded, teach this test its new spelling, and if it was DROPPED, "+
			"the declaration has to go with it (#119)", rel, clause)
	}
}
