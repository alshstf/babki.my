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

// MaxAmountMinor is stated three times: here, in the OpenAPI contract
// (minimum/maximum), and in web/src/lib/money.ts for the amount fields. This
// test holds the other two to it. The prose around each copy is not checked.

// Every contract field bounded by the cap, by explicit path, so a missing
// declaration fails here (#100, #102).
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

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// shown formats an optional bound; %v on a pointer would print the address.
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

	// Floors differ by field: amounts and balances are bounded by magnitude, a fee
	// and a stated cost basis stop at zero, as the server enforces.
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
		// Reported as missing rather than as a wrong number.
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

// webMaxAmountRe matches the declaration, not the digits in a comment.
var webMaxAmountRe = regexp.MustCompile(`export const MAX_AMOUNT_MINOR = ([\d_]+);`)

func TestTheAmountFieldRefusesAtTheBoundTheServerEnforces(t *testing.T) {
	const rel = "web/src/lib/money.ts"
	found := webMaxAmountRe.FindStringSubmatch(repoFile(t, rel))
	if found == nil {
		// A rename or reformat, not a different value.
		t.Fatalf("no `export const MAX_AMOUNT_MINOR = <digits>;` in %s. "+
			"It holds the frontend's copy of money.MaxAmountMinor (%d); if the declaration was renamed or "+
			"reformatted, teach webMaxAmountRe its new shape rather than leaving the two untied",
			rel, money.MaxAmountMinor)
	}
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
