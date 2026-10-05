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

// The credential rules are stated in Go and in the OpenAPI contract (#117);
// these tests hold the contract to the Go constants.

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return body
}

// contractDoc reads only the keywords these tests need.
type contractDoc struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Pattern   *string `yaml:"pattern"`
				MinLength *int    `yaml:"minLength"`
				MaxLength *int    `yaml:"maxLength"`
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

// UsernamePattern is declared on both schemas that carry a username (setup and
// member creation).
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

// MinPasswordRunes is declared as minLength on the same schemas; possible since
// the count is in characters, as minLength counts (#117).
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
		if prop.MaxLength == nil || *prop.MaxLength != family.MaxPasswordRunes {
			t.Errorf("api/openapi.yaml %s.password maxLength = %s, want %d (family.MaxPasswordRunes)",
				schema, shownInt(prop.MaxLength), family.MaxPasswordRunes)
		}
	}
}

// A new password is held to the same rule.
func TestTheContractStatesTheNewPasswordRule(t *testing.T) {
	prop, ok := readContract(t).Components.Schemas["ChangePasswordRequest"].Properties["new_password"]
	if !ok {
		t.Fatal("api/openapi.yaml ChangePasswordRequest has no `new_password` property")
	}
	if prop.MinLength == nil || *prop.MinLength != family.MinPasswordRunes {
		t.Errorf("ChangePasswordRequest.new_password minLength = %s, want %d", shownInt(prop.MinLength), family.MinPasswordRunes)
	}
	if prop.MaxLength == nil || *prop.MaxLength != family.MaxPasswordRunes {
		t.Errorf("ChangePasswordRequest.new_password maxLength = %s, want %d", shownInt(prop.MaxLength), family.MaxPasswordRunes)
	}
}

// Every name whose only rule is "not empty", across modules, in one explicit
// list (a discovered one could sweep in a response field, see #119). The floor
// is 1: the server compares with "" and does not trim.
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

// Login declares no rule: it checks neither shape nor length, and a declared
// minLength would lock out older passwords.
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

// credentialFormSites are the two dialogs whose copies of the rules enable
// their Save buttons; held to the Go constants.
var credentialFormSites = []string{
	"web/src/routes/setup.tsx",
	"web/src/routes/family/member-dialog.tsx",
}

func TestTheCredentialFormsRefuseAtTheRulesTheServerEnforces(t *testing.T) {
	// Compared as text: the username as a JS regex literal, the password as a
	// comparison with the same integer. JavaScript counts UTF-16 units, so four
	// emoji pass the form and fail the server; accepted for this audience.
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
