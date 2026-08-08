package family_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/family"
)

// The rules the two doors that create a user apply are written down twice: once
// in Go, where validateCredentials refuses, and once in api/openapi.yaml, where
// a client reads them before sending anything. Go cannot import a YAML scalar
// and a JSON-Schema validator cannot import a Go constant, so the second copy is
// typed by hand — and until #117 it was not typed at all. The document said
// `type: string` for a username the server holds to [a-z0-9_]{3,32}, for a
// password it refuses under eight characters, and for four names it refuses
// empty; a client validating against the contract could check none of them.
//
// This file does not remove the duplication. It removes the SILENCE. Change the
// rule in Go and it names the declaration that still states the old one; add a
// door that writes a credential without declaring its rule and this list is
// where a reader is told to add it.
//
// It is the family module's own copy of internal/instrument/contract_sites_test.go
// rather than a shared helper, for the reason that file gives about its own
// bounds: what these tests have in common is a shape, and what they do not have
// in common is the rules.

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

// The document read as far as these tests need it. A yaml.Node would keep the
// rest unparsed either way; spelling out only these three keywords means a
// change anywhere else in the contract cannot break this file.
type contractDoc struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Pattern   *string `yaml:"pattern"`
				MinLength *int    `yaml:"minLength"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
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

func shownInt(v *int) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(*v)
}

// TestTheContractStatesTheUsernameShapeTheServerEnforces ties
// family.UsernamePattern to the two request schemas that carry a username. Both,
// not one: the rule is applied at Setup and at CreateMember alike, and a client
// that could check the first and not the second would offer a Save button that
// enables for a name the server refuses.
func TestTheContractStatesTheUsernameShapeTheServerEnforces(t *testing.T) {
	doc := readContract(t)
	for _, site := range []struct{ schema, door string }{
		{"SetupRequest", "internal/family/auth.go, Setup"},
		{"CreateMemberRequest", "internal/family/auth.go, CreateMember"},
	} {
		prop, ok := doc.Components.Schemas[site.schema].Properties["username"]
		if !ok {
			t.Errorf("api/openapi.yaml %s has no `username` property, but %s reads one", site.schema, site.door)
			continue
		}
		if prop.Pattern == nil {
			t.Errorf("api/openapi.yaml %s.username declares no pattern; the server refuses anything but %s at this door (%s), "+
				"and a client validating against the contract can only check a rule the contract carries",
				site.schema, family.UsernamePattern, site.door)
			continue
		}
		if *prop.Pattern != family.UsernamePattern {
			t.Errorf("api/openapi.yaml %s.username pattern = %q, want %q (family.UsernamePattern, enforced in %s): "+
				"the document and the server would refuse different names",
				site.schema, *prop.Pattern, family.UsernamePattern, site.door)
		}
	}
}

// TestTheContractStatesThePasswordLengthTheServerEnforces ties
// family.MinPasswordRunes to the same two schemas.
//
// This declaration is the reason the count moved from bytes to runes at all:
// `minLength` counts characters, so with a byte count the only honest options
// were to declare nothing or to declare a floor stricter than the server's
// (#117). It can be stated now because the two count the same thing.
func TestTheContractStatesThePasswordLengthTheServerEnforces(t *testing.T) {
	doc := readContract(t)
	for _, schema := range []string{"SetupRequest", "CreateMemberRequest"} {
		prop, ok := doc.Components.Schemas[schema].Properties["password"]
		if !ok {
			t.Errorf("api/openapi.yaml %s has no `password` property", schema)
			continue
		}
		if prop.MinLength == nil || *prop.MinLength != family.MinPasswordRunes {
			t.Errorf("api/openapi.yaml %s.password minLength = %s, want %d (family.MinPasswordRunes, "+
				"enforced in validateCredentials): a client validating against this document would send "+
				"a password the server refuses, or refuse to send one it takes",
				schema, shownInt(prop.MinLength), family.MinPasswordRunes)
		}
	}
}

// TestTheContractStatesTheNamesTheServerRefusesEmpty covers every name in this
// API whose only rule is that it is not "".
//
// ONE LIST, THOUGH THREE MODULES REFUSE THEM. Unlike the bounds above, this is
// not the family module's rule — it is the SAME rule spelled seven times in
// three packages, and the way it goes wrong is that somebody adds an eighth
// name and declares nothing. A list per package would be three places to forget
// instead of one, and each of them would look complete. The same shape as
// internal/platform/currency/contract_sites_test.go, which names doors in four
// modules from one file for the same reason.
//
// Each is written out with the door that refuses it rather than discovered by
// walking the document for anything called a name: a discovered list would
// silently grow, and — worse — could sweep in a RESPONSE field, where a floor
// says something else entirely (see Instrument.face_currency and #119).
//
// The floor is 1 and not more because the server compares against "" and does
// not trim. Declaring 2, or a pattern demanding a non-blank character, would be
// a document stricter than the code, which is the same defect as a document
// looser than it, pointing the other way.
func TestTheContractStatesTheNamesTheServerRefusesEmpty(t *testing.T) {
	doc := readContract(t)
	for _, site := range []struct{ schema, field, door string }{
		{"SetupRequest", "space_name", "internal/family/auth.go, Setup"},
		{"SetupRequest", "display_name", "internal/family/auth.go, Setup"},
		{"CreateMemberRequest", "display_name", "internal/family/auth.go, CreateMember"},
		{"CreateAccountRequest", "name", "internal/account/http.go, handleCreate"},
		{"UpdateAccountRequest", "name", "internal/account/http.go, handleUpdate"},
		{"CreateInstrumentRequest", "name", "internal/instrument/http.go, handleCreate"},
		{"UpdateInstrumentRequest", "name", "internal/instrument/http.go, handleUpdate"},
	} {
		schema, ok := doc.Components.Schemas[site.schema]
		if !ok {
			t.Errorf("api/openapi.yaml has no schema %s; this list names the request schemas carrying a name "+
				"the server refuses empty (in %s) — if it was renamed, rename it here too", site.schema, site.door)
			continue
		}
		prop, ok := schema.Properties[site.field]
		if !ok {
			t.Errorf("api/openapi.yaml %s has no property %s (refused empty in %s)", site.schema, site.field, site.door)
			continue
		}
		if prop.MinLength == nil || *prop.MinLength != 1 {
			t.Errorf("api/openapi.yaml %s.%s minLength = %s, want 1: %s answers 400 for \"\", "+
				"and a client validating against the contract can only check a rule the contract carries",
				site.schema, site.field, shownInt(prop.MinLength), site.door)
		}
	}
}

// TestTheContractDeclaresNoRuleOnTheLoginCredentials is the negative half, and
// it is the one that needs an assertion rather than a comment.
//
// Login judges neither the shape of the username nor the length of the password
// (internal/family/auth.go, Login): it looks the name up, compares the password
// against the stored hash, and answers 401 to every failure alike so that a
// caller cannot enumerate users. Declaring either rule here would describe a
// refusal that does not exist — and would lock out the very users this change
// left alone, since a password accepted when the count was in bytes is one
// `minLength: 8` now calls too short.
//
// Without this test, "the two schemas above carry it, so this one should too"
// is a tidy-looking edit nothing would stop.
func TestTheContractDeclaresNoRuleOnTheLoginCredentials(t *testing.T) {
	doc := readContract(t)
	login, ok := doc.Components.Schemas["LoginRequest"]
	if !ok {
		t.Fatal("api/openapi.yaml has no LoginRequest schema")
	}
	for _, field := range []string{"username", "password"} {
		prop, ok := login.Properties[field]
		if !ok {
			t.Errorf("api/openapi.yaml LoginRequest has no %s property", field)
			continue
		}
		if prop.Pattern != nil {
			t.Errorf("api/openapi.yaml LoginRequest.%s declares pattern %q, want none: Login checks no shape at all, "+
				"and a schema-aware client would refuse to send credentials the server would have accepted",
				field, *prop.Pattern)
		}
		if prop.MinLength != nil {
			t.Errorf("api/openapi.yaml LoginRequest.%s declares minLength %d, want none: Login checks no length at all, "+
				"and a password accepted while the count was in bytes is one this floor would lock its owner out of",
				field, *prop.MinLength)
		}
	}
}

// The two dialogs that decide, before any request is sent, whether their Save
// button lights up at all. They hold hand-typed copies of both rules, and until
// this test existed neither copy was tied to anything: widening the username or
// lowering the password floor in Go would have left every Go test and the
// contract tests above green while both forms kept refusing what the server had
// started to accept — no error, nothing red, a button that never enables.
//
// Same shape of gap as TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces
// (internal/platform/currency/frontend_sites_test.go) and
// TestTheAmountFieldRefusesAtTheBoundTheServerEnforces
// (internal/platform/money/bound_sites_test.go) close for their own constants.
var credentialFormSites = []string{
	"web/src/routes/setup.tsx",
	"web/src/routes/family/member-dialog.tsx",
}

func TestTheCredentialFormsRefuseAtTheRulesTheServerEnforces(t *testing.T) {
	// The username is written at both sites as a JS regex literal, which is
	// UsernamePattern itself between a pair of slashes; the password as a
	// comparison against the same integer. Compared as TEXT rather than
	// re-derived, so this fails the way a human diff would if the two ever read
	// differently.
	//
	// The password comparison is the one place the two languages do not agree
	// exactly, and it is written down rather than glossed over: JavaScript's
	// String.length counts UTF-16 code units, so four astral characters (emoji)
	// are 8 to the form and 4 to the server, and the form would let such a
	// password through for the server to refuse. Every character a Russian or
	// English speaker types is one code unit, so the two agree everywhere the
	// audience actually lives; a surrogate-aware count in the form would buy
	// that last case, and it is not bought here.
	wantUsername := "/" + family.UsernamePattern + "/"
	wantPassword := "password.length >= " + strconv.Itoa(family.MinPasswordRunes)
	for _, rel := range credentialFormSites {
		body := string(repoFile(t, rel))
		if !strings.Contains(body, wantUsername) {
			t.Errorf("%s does not contain the regex literal %s (family.UsernamePattern): "+
				"its Save button would enable for a username the server refuses, or stay disabled "+
				"for one the server would take", rel, wantUsername)
		}
		if !strings.Contains(body, wantPassword) {
			t.Errorf("%s does not contain %q (family.MinPasswordRunes): "+
				"its Save button would enable for a password the server refuses, or stay disabled "+
				"for one the server would take", rel, wantPassword)
		}
	}
}
