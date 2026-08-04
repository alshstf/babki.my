package money_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/platform/money"
)

// MaxAmountMinor is written down in three places, in three languages, and only
// one of them is the rule. money.MaxAmountMinor is what the server enforces; the
// contract states the same bound as minimum/maximum so a client can validate
// against the document; and web/src/lib/money.ts holds a copy so an amount field
// refuses at the keystroke instead of after a round trip.
//
// Nothing made them agree. Go cannot import a YAML literal and TypeScript cannot
// import a Go constant, so the two outer copies are typed by hand, and a change
// to the rule that touched only two of the three would leave a field accepting
// what the server refuses (or refusing what it takes) with every test in this
// repository still green. That is the shape of defect this package's own doc
// comment calls out — "two statements of one rule drift; this codebase has been
// bitten by that before" — and it had been left standing inside the constant
// that says so.
//
// This test does not remove the duplication; it removes the SILENCE. Change the
// bound anywhere and this names the other two sites.
//
// What it cannot check is the PROSE. All three sites also spell the figure out
// in words — "ten trillion whole roubles or dollars", "±10^15 minor units" — and
// those sentences are read by people, not parsers. Whoever moves this number
// re-reads them by hand; there is no mechanism here that will notice.

// The bound as the contract states it, in the one schema that states it. The
// path is spelled out rather than searched for, so that a bound added elsewhere
// tomorrow does not silently satisfy this test in place of the one it is about.
type contractDoc struct {
	Components struct {
		Schemas struct {
			SetBalanceRequest struct {
				Properties struct {
					AmountMinor struct {
						Minimum *int64 `yaml:"minimum"`
						Maximum *int64 `yaml:"maximum"`
					} `yaml:"amount_minor"`
				} `yaml:"properties"`
			} `yaml:"SetBalanceRequest"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

// repoFile reads a path relative to the repository root. Tests run with their
// own package directory as the working directory, and the two files this test is
// about live outside the Go tree entirely.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// shown writes an optional bound for a person. %v on a *int64 prints the
// ADDRESS, which is the one thing about it nobody can act on — and the message
// below exists precisely for the case where one of these is missing.
func shown(v *int64) string {
	if v == nil {
		return "absent"
	}
	return strconv.FormatInt(*v, 10)
}

func TestTheContractStatesTheBoundTheServerEnforces(t *testing.T) {
	var doc contractDoc
	if err := yaml.Unmarshal([]byte(repoFile(t, "api/openapi.yaml")), &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	amount := doc.Components.Schemas.SetBalanceRequest.Properties.AmountMinor

	// Absent, not merely different: a schema that dropped the bound would leave
	// the pointers nil, and comparing a nil-derived zero against the constant
	// would report "0, want 10^15" — true, but it would send the reader looking
	// for a wrong number rather than a missing one.
	if amount.Maximum == nil || amount.Minimum == nil {
		t.Fatalf("SetBalanceRequest.amount_minor has minimum=%s maximum=%s; both state money.MaxAmountMinor (%d) "+
			"and a client validating against the contract can only check a bound the contract carries",
			shown(amount.Minimum), shown(amount.Maximum), money.MaxAmountMinor)
	}
	if *amount.Maximum != money.MaxAmountMinor {
		t.Errorf("api/openapi.yaml SetBalanceRequest.amount_minor.maximum = %d, want %d (money.MaxAmountMinor): "+
			"the contract promises a client a bound the server does not enforce",
			*amount.Maximum, money.MaxAmountMinor)
	}
	// The bound is on the MAGNITUDE — a debt is a negative balance and is
	// refused at the same size an asset is — so the contract's floor is the
	// negation of the same figure.
	if *amount.Minimum != -money.MaxAmountMinor {
		t.Errorf("api/openapi.yaml SetBalanceRequest.amount_minor.minimum = %d, want %d (-money.MaxAmountMinor): "+
			"a debt is refused at the same magnitude an asset is",
			*amount.Minimum, -money.MaxAmountMinor)
	}
}

// webMaxAmountRe matches the frontend's copy of the bound. Anchored to the
// declaration rather than to the digits alone, because the digits also appear in
// that file's comments, and a test that matched a comment would pass on a file
// whose actual constant had moved.
var webMaxAmountRe = regexp.MustCompile(`export const MAX_AMOUNT_MINOR = ([\d_]+);`)

func TestTheAmountFieldRefusesAtTheBoundTheServerEnforces(t *testing.T) {
	const rel = "web/src/lib/money.ts"
	found := webMaxAmountRe.FindStringSubmatch(repoFile(t, rel))
	if found == nil {
		// Reported apart from a mismatch on purpose: this is what a rename or a
		// reformat looks like, and the fix for it is to update the pattern above,
		// not to change any number.
		t.Fatalf("no `export const MAX_AMOUNT_MINOR = <digits>;` in %s. "+
			"It holds the frontend's copy of money.MaxAmountMinor (%d); if the declaration was renamed or "+
			"reformatted, teach webMaxAmountRe its new shape rather than leaving the two untied",
			rel, money.MaxAmountMinor)
	}
	// Go and TypeScript happen to write digit separators the same way, and this
	// test would be about nothing if it compared the two spellings instead of the
	// two values.
	web, err := strconv.ParseInt(strings.ReplaceAll(found[1], "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("MAX_AMOUNT_MINOR in %s is %q, which is not an int64: %v", rel, found[1], err)
	}
	if web != money.MaxAmountMinor {
		t.Errorf("MAX_AMOUNT_MINOR in %s = %d, want %d (money.MaxAmountMinor): "+
			"the field would refuse a sum the server takes, or send one it refuses",
			rel, web, money.MaxAmountMinor)
	}
}
