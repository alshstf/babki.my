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

// The bound as the contract states it, in every schema that has to state it.
// Each path is spelled out rather than searched for, so that a bound added
// elsewhere tomorrow does not silently satisfy this test in place of one it is
// about — and so that a MISSING declaration is a failure here rather than a
// schema this test simply never looked at.
//
// There are four such fields and not one, which is #100 and #102: the server
// refuses by this same cap at every door money is written through, and the
// contract declared it at exactly one of them. A client validating against the
// document could check its balance and not its deposit.
type moneyBound struct {
	Minimum *int64 `yaml:"minimum"`
	Maximum *int64 `yaml:"maximum"`
}

type contractDoc struct {
	Components struct {
		Schemas struct {
			SetBalanceRequest struct {
				Properties struct {
					AmountMinor moneyBound `yaml:"amount_minor"`
				} `yaml:"properties"`
			} `yaml:"SetBalanceRequest"`
			CreateOperationRequest struct {
				Properties struct {
					AmountMinor moneyBound `yaml:"amount_minor"`
					FeeMinor    moneyBound `yaml:"fee_minor"`
				} `yaml:"properties"`
			} `yaml:"CreateOperationRequest"`
			TransferRequest struct {
				Properties struct {
					CostMinor moneyBound `yaml:"cost_minor"`
				} `yaml:"properties"`
			} `yaml:"TransferRequest"`
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
	schemas := doc.Components.Schemas

	// THE FLOOR IS NOT THE SAME FIGURE AT EVERY DOOR, and each one below is
	// written from the code that enforces it rather than from its neighbour. An
	// amount and a balance are bounded by MAGNITUDE, because an outflow and a
	// debt are ordinary negative figures; a fee and a hand-given cost basis stop
	// at zero, because the server refuses a negative one outright and for a
	// reason of its own. Copying the amount's ±cap onto the fee would publish a
	// floor of -10^15 for a field the server refuses at -1 — a bound that is
	// wrong in the direction a client cannot detect, since every request it
	// wrongly permitted would come back 400.
	for _, site := range []struct {
		where    string
		declared moneyBound
		min, max int64
		enforced string // where the server's own refusal lives
		floor    string // why the floor is what it is
	}{
		{
			where: "SetBalanceRequest.amount_minor", declared: schemas.SetBalanceRequest.Properties.AmountMinor,
			min: -money.MaxAmountMinor, max: money.MaxAmountMinor,
			enforced: "internal/account/http.go, handleSetBalance",
			floor:    "a debt is a negative balance and is refused at the same magnitude an asset is",
		},
		{
			where: "CreateOperationRequest.amount_minor", declared: schemas.CreateOperationRequest.Properties.AmountMinor,
			min: -money.MaxAmountMinor, max: money.MaxAmountMinor,
			enforced: "internal/operation/service.go, validate",
			floor:    "an outflow is a negative amount and is refused at the same magnitude an inflow is",
		},
		{
			where: "CreateOperationRequest.fee_minor", declared: schemas.CreateOperationRequest.Properties.FeeMinor,
			min: 0, max: money.MaxAmountMinor,
			enforced: "internal/operation/service.go, validate",
			floor:    "a fee is never negative: `fee_minor must be >= 0` is a refusal of its own, not the cap read at the other end",
		},
		{
			where: "TransferRequest.cost_minor", declared: schemas.TransferRequest.Properties.CostMinor,
			min: 0, max: money.MaxAmountMinor,
			enforced: "internal/operation/service.go, CreateTransfer",
			floor:    "a cost basis given by hand is refused below zero: `cost_minor must be within 0..10^15`",
		},
	} {
		// Absent, not merely different: a schema that carries no bound leaves the
		// pointers nil, and comparing a nil-derived zero against the constant
		// would report "0, want 10^15" — true, but it would send the reader
		// looking for a wrong number rather than a missing one.
		if site.declared.Maximum == nil || site.declared.Minimum == nil {
			t.Errorf("api/openapi.yaml %s has minimum=%s maximum=%s; the server refuses past %d..%d (%s), "+
				"and a client validating against the contract can only check a bound the contract carries",
				site.where, shown(site.declared.Minimum), shown(site.declared.Maximum),
				site.min, site.max, site.enforced)
			continue
		}
		if *site.declared.Maximum != site.max {
			t.Errorf("api/openapi.yaml %s.maximum = %d, want %d (money.MaxAmountMinor, enforced in %s): "+
				"the contract states a ceiling the server does not",
				site.where, *site.declared.Maximum, site.max, site.enforced)
		}
		if *site.declared.Minimum != site.min {
			t.Errorf("api/openapi.yaml %s.minimum = %d, want %d (enforced in %s): %s",
				site.where, *site.declared.Minimum, site.min, site.enforced, site.floor)
		}
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
