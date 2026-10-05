package instrument

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The catalog's paging bounds are stated in Go and in the contract (#118: the
// contract said maximum 200 while the server clamped). In package instrument
// so it can read the constants.

// repoFile reads a file relative to the repository root.
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

	// limit: default, floor and ceiling, as parsePage enforces them.
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

// The contract declares the 400 parsePage answers.
func TestTheContractStatesTheCatalogAnswers400(t *testing.T) {
	doc := readContract(t)
	if _, ok := doc.Paths[catalogPath].Get.Responses["400"]; !ok {
		t.Errorf("GET %s declares no 400, but parsePage answers one for a limit or an offset "+
			"outside the bounds beside it", catalogPath)
	}
}

// The response is an envelope saying whether there is more (#104).
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

// A constraint on a response field promises something about every stored row,
// which only the database can guarantee (#119). face_currency's pattern was
// backed by the writers alone, so it is dropped; the minLength the CHECK
// guarantees stays, as face_value_minor keeps minimum 1.
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
	// The floor is declarable: migration 0012's CHECK keeps '' out.
	if face.MinLength == nil || *face.MinLength != 1 {
		t.Errorf("api/openapi.yaml Instrument.face_currency minLength = %s, want 1: "+
			"migration 0012's CHECK (face_currency <> '') makes that true of every row in the table, "+
			"and '' is the value that denominates a bond's face in nothing while passing every "+
			"presence check", shown(face.MinLength))
	}

	// Its twin, the precedent.
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

// Migration 0012 still contains the clause the response's floor rests on.
func TestTheMigrationStillBacksWhatTheResponsePromises(t *testing.T) {
	const rel = "internal/platform/db/migrations/0012_instruments_face_value_sound.sql"
	body := string(repoFile(t, rel))
	// Matched as text; a reformat means updating this test.
	const clause = "face_currency IS NULL OR face_currency <> ''"
	if !strings.Contains(body, clause) {
		t.Errorf("%s no longer contains %q. Instrument.face_currency declares minLength: 1, "+
			"which is a promise about every row in the table and rests on this CHECK; if the "+
			"constraint was reworded, teach this test its new spelling, and if it was DROPPED, "+
			"the declaration has to go with it (#119)", rel, clause)
	}
}
