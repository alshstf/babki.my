package tinvest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every bound and every vocabulary this module publishes is written down twice:
// once in Go, where the server enforces it, and once in api/openapi.yaml, where
// a client reads it. Go cannot import a YAML literal, so the second copy is
// typed by hand — and a change that touched only one of the two would leave the
// contract promising something the server does not do, with every other test in
// this repository still green. That is #120's lesson, and #118's: a ceiling the
// document states and the server does not apply is a defect in its own right.
//
// This file does not remove the duplication. It removes the SILENCE.
//
// TWO OF THE CHECKS BELOW READ THE GO SOURCE RATHER THAN THE GO CONSTANTS, and
// that is on purpose: comparing the contract against a list of constants spelled
// out here would say nothing about a THIRTEENTH reason code added tomorrow — the
// list here would simply not mention it, and the contract's enum would go on
// looking complete. Reading the declarations out of the file that declares them
// makes an unlisted value a failure.

// repoFile reads a path relative to the repository root. Tests run with their
// own package directory as the working directory, and the contract lives outside
// the Go tree entirely.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
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

type contractProperty struct {
	MinLength *int `yaml:"minLength"`
	MinItems  *int `yaml:"minItems"`
}

type contractSchema struct {
	Enum          []string                    `yaml:"enum"`
	MinProperties *int                        `yaml:"minProperties"`
	Properties    map[string]contractProperty `yaml:"properties"`
}

// contractOperation is one verb of one path: the parameters it takes and the
// status codes it says it can answer with. The statuses are read as a bare map
// because nothing here looks inside a response — only at which codes are named.
type contractOperation struct {
	Parameters []contractParam `yaml:"parameters"`
	Responses  map[string]any  `yaml:"responses"`
}

type contractPath struct {
	Get    contractOperation `yaml:"get"`
	Post   contractOperation `yaml:"post"`
	Patch  contractOperation `yaml:"patch"`
	Delete contractOperation `yaml:"delete"`
}

type contractDoc struct {
	Paths      map[string]contractPath `yaml:"paths"`
	Components struct {
		Schemas map[string]contractSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func readContract(t *testing.T) contractDoc {
	t.Helper()
	var doc contractDoc
	if err := yaml.Unmarshal(repoFile(t, "api/openapi.yaml"), &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	return doc
}

func shown(v *int) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(*v)
}

// pagedPaths are the two endpoints that take limit and offset. Spelled out
// rather than discovered, so that a third paged endpoint added without a line
// here is a gap this test reports by absence rather than one it silently skips.
var pagedPaths = []string{
	"/api/v1/tinvest/connections/{connectionId}/runs",
	"/api/v1/tinvest/connections/{connectionId}/unparsed",
}

func TestTheContractStatesThePageBoundsTheServerEnforces(t *testing.T) {
	doc := readContract(t)
	for _, path := range pagedPaths {
		item, ok := doc.Paths[path]
		if !ok {
			t.Errorf("api/openapi.yaml declares no GET %s", path)
			continue
		}
		params := map[string]contractParam{}
		for _, p := range item.Get.Parameters {
			if p.In == "query" {
				params[p.Name] = p
			}
		}

		// limit: default, floor and ceiling, all three enforced in parsePage
		// (internal/importer/tinvest/http.go), all three declared here.
		limit, ok := params["limit"]
		if !ok {
			t.Errorf("GET %s declares no `limit` query parameter, but the server reads one", path)
		} else {
			if limit.Schema.Maximum == nil || *limit.Schema.Maximum != maxPageLimit {
				t.Errorf("GET %s limit.maximum = %s, want %d (maxPageLimit, refused past it in parsePage): "+
					"a ceiling the contract states and the server does not apply is #118",
					path, shown(limit.Schema.Maximum), maxPageLimit)
			}
			if limit.Schema.Minimum == nil || *limit.Schema.Minimum != 1 {
				t.Errorf("GET %s limit.minimum = %s, want 1: parsePage refuses 0 and below, "+
					"and the store refuses a limit under 1 behind it",
					path, shown(limit.Schema.Minimum))
			}
			if limit.Schema.Default == nil || *limit.Schema.Default != defaultPageLimit {
				t.Errorf("GET %s limit.default = %s, want %d (defaultPageLimit, what parsePage uses "+
					"when the parameter is absent)", path, shown(limit.Schema.Default), defaultPageLimit)
			}
		}

		offset, ok := params["offset"]
		if !ok {
			t.Errorf("GET %s declares no `offset` query parameter, but the server reads one", path)
			continue
		}
		if offset.Schema.Minimum == nil || *offset.Schema.Minimum != 0 {
			t.Errorf("GET %s offset.minimum = %s, want 0: parsePage refuses a negative offset",
				path, shown(offset.Schema.Minimum))
		}
		if offset.Schema.Default == nil || *offset.Schema.Default != 0 {
			t.Errorf("GET %s offset.default = %s, want 0", path, shown(offset.Schema.Default))
		}
	}
}

// TestTheContractStatesWhichPathCanAnswer422 ties the one status code this
// module answers on a single path to the document that declares it.
//
// 422 is the answer to a picked broker account this token cannot import, and
// ONLY POST /api/v1/tinvest/connections can produce it: it is the only call that
// takes picks (Service.CreateConnection, ErrBrokerAccountNotImportable). Two
// things could go wrong silently, and this checks both. The create path could
// stop declaring what it answers, leaving a client with no name for the code it
// meets. Or a later path could copy the declaration without being able to
// produce it — a promise nothing keeps, which is #120's whole lesson in reverse.
//
// The count is asserted for the reason goConstantValues asserts its own: a
// renamed prefix or a moved path would otherwise leave this test iterating over
// nothing and passing.
func TestTheContractStatesWhichPathCanAnswer422(t *testing.T) {
	doc := readContract(t)
	const createPath = "/api/v1/tinvest/connections"
	const wantOperations = 11 // what Handler.Mount registers
	const explainPath = "/api/v1/tinvest/links/{linkId}/explanations"

	seen := 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/api/v1/tinvest/") {
			continue
		}
		for verb, op := range map[string]contractOperation{
			"GET": item.Get, "POST": item.Post, "PATCH": item.Patch, "DELETE": item.Delete,
		} {
			if len(op.Responses) == 0 {
				continue
			}
			seen++
			_, declares422 := op.Responses["422"]
			isCreate := verb == "POST" && path == createPath
			if isCreate && !declares422 {
				t.Errorf("%s %s declares no 422, but writeError answers one for "+
					"ErrBrokerAccountNotImportable: a client meeting it has nothing to read it by",
					verb, path)
			}
			// The explain path answers a 422 of its own, and a different one:
			// the manual operation it is given goes through the journal's own
			// service, whose refusals reach the client unchanged. Both paths
			// have to declare it, and no third one may — a 422 nothing can
			// answer is a promise a client would wait for forever.
			isExplain := verb == "POST" && path == explainPath
			if isExplain && !declares422 {
				t.Errorf("%s %s declares no 422, but the journal's own refusals travel through it "+
					"unchanged: a client meeting one has nothing to read it by", verb, path)
			}
			if !isCreate && !isExplain && declares422 {
				t.Errorf("%s %s declares a 422 it cannot answer: only the create path takes "+
					"broker account picks and only the explain path forwards the journal's "+
					"refusals, so no other one can produce one", verb, path)
			}
			if isCreate {
				// The other leg of the same split, declared beside it: a refused
				// token stays a 400 here, and a document naming only one of the
				// two would leave the pair looking like one answer.
				if _, ok := op.Responses["400"]; !ok {
					t.Errorf("%s %s declares no 400, but a token the broker refuses is answered "+
						"with one (ErrTokenRejected)", verb, path)
				}
			}
		}
	}
	if seen != wantOperations {
		t.Fatalf("found %d operations under /api/v1/tinvest/ in the contract, want %d "+
			"(what Handler.Mount registers): if a route was added or moved, say so here rather "+
			"than leaving this test looking at a set it no longer covers", seen, wantOperations)
	}
}

// TestTheContractStatesTheFieldsTheServerRefusesEmpty covers the bounds that are
// not numbers: the request fields whose emptiness the service turns into a 400.
// Each one below names where that refusal lives, so a reader can go and check
// the claim rather than take this table's word for it.
func TestTheContractStatesTheFieldsTheServerRefusesEmpty(t *testing.T) {
	doc := readContract(t)
	for _, site := range []struct {
		schema, property, enforced string
	}{
		{"TinvestTokenCheckRequest", "token", "Service.checkToken"},
		{"CreateTinvestConnectionRequest", "token", "Service.checkToken"},
		{"UpdateTinvestConnectionRequest", "token", "Service.checkToken"},
		{"TinvestAccountPick", "broker_account_id", "validatePicks"},
		{"TinvestAccountPick", "account_name", "validatePicks"},
	} {
		prop, ok := doc.Components.Schemas[site.schema].Properties[site.property]
		if !ok {
			t.Errorf("api/openapi.yaml has no %s.%s", site.schema, site.property)
			continue
		}
		if prop.MinLength == nil || *prop.MinLength != 1 {
			t.Errorf("%s.%s minLength = %s, want 1: %s refuses it empty with a 400, "+
				"and a client validating against the contract can only check a bound the contract carries",
				site.schema, site.property, shown(prop.MinLength), site.enforced)
		}
	}

	accounts, ok := doc.Components.Schemas["CreateTinvestConnectionRequest"].Properties["accounts"]
	if !ok {
		t.Fatal("api/openapi.yaml has no CreateTinvestConnectionRequest.accounts")
	}
	if accounts.MinItems == nil || *accounts.MinItems != 1 {
		t.Errorf("CreateTinvestConnectionRequest.accounts minItems = %s, want 1: "+
			"validatePicks refuses an empty list", shown(accounts.MinItems))
	}

	update := doc.Components.Schemas["UpdateTinvestConnectionRequest"]
	if update.MinProperties == nil || *update.MinProperties != 1 {
		t.Errorf("UpdateTinvestConnectionRequest minProperties = %s, want 1: "+
			"Service.UpdateConnection refuses a request that changes nothing",
			shown(update.MinProperties))
	}
}

// goConstantValues reads the string values of every constant of the named Go
// type out of the files that declare it. Reading the SOURCE rather than the
// constants is what makes a value added in Go and forgotten in the contract a
// failure here — see the note at the top of this file.
//
// want is how many declarations the pattern must find. It is checked because a
// rename or a reformat would otherwise leave this matching nothing at all, and a
// test comparing two empty sets passes.
func goConstantValues(t *testing.T, typeName string, want int, files ...string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w+\s+` + typeName + `\s*=\s*"([^"]*)"`)
	var out []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			out = append(out, m[1])
		}
	}
	if len(out) != want {
		t.Fatalf("found %d declarations of %s in %v (%v), want %d: if the declarations were "+
			"renamed or reformatted, teach this pattern their new shape rather than leaving the "+
			"contract untied from them", len(out), typeName, files, out, want)
	}
	sort.Strings(out)
	return out
}

func TestTheContractStatesTheVocabulariesTheServerUses(t *testing.T) {
	doc := readContract(t)
	for _, site := range []struct {
		schema string
		values []string
	}{
		{"TinvestConnectionStatus", goConstantValues(t, "ConnectionStatus", 3, "store.go")},
		{"TinvestSyncTrigger", goConstantValues(t, "SyncTrigger", 4, "store.go")},
		{"TinvestSyncRunStatus", goConstantValues(t, "RunStatus", 3, "store.go")},
		{"TinvestReconcileStatus", goConstantValues(t, "ReconcileStatus", 3, "store.go", "reconcile.go")},
		{"TinvestUnparsedReason", goConstantValues(t, "UnparsedReason", 18, "projection.go")},
	} {
		declared := append([]string(nil), doc.Components.Schemas[site.schema].Enum...)
		sort.Strings(declared)
		if !slices.Equal(declared, site.values) {
			t.Errorf("api/openapi.yaml %s.enum = %v, want %v (the values the Go declarations carry): "+
				"a client switching on this enum would meet a value the contract never named, "+
				"or handle one the server never sends", site.schema, declared, site.values)
		}
	}

	// The mismatch kinds are plain string constants with no named type of their
	// own (they travel through a jsonb column), so they are matched by their own
	// declaration shape rather than by a type name.
	kindRe := regexp.MustCompile(`(?m)^\s*Mismatch\w+\s*=\s*"([^"]*)"`)
	body, err := os.ReadFile("reconcile.go")
	if err != nil {
		t.Fatalf("read reconcile.go: %v", err)
	}
	var kinds []string
	for _, m := range kindRe.FindAllStringSubmatch(string(body), -1) {
		kinds = append(kinds, m[1])
	}
	if len(kinds) != 4 {
		t.Fatalf("found %d Mismatch* constants in reconcile.go (%v), want 4", len(kinds), kinds)
	}
	sort.Strings(kinds)
	declared := append([]string(nil), doc.Components.Schemas["TinvestReconcileMismatchKind"].Enum...)
	sort.Strings(declared)
	if !slices.Equal(declared, kinds) {
		t.Errorf("api/openapi.yaml TinvestReconcileMismatchKind.enum = %v, want %v", declared, kinds)
	}
}
