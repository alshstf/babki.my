package currency_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/currency"
)

// currency.Pattern is tied to api/openapi.yaml by
// TestTheContractStatesTheCurrencyShapeTheServerEnforces above. It is also
// copied, by hand, into three frontend dialogs that decide whether their Save
// button is enabled at all — before any request reaches the server, let alone
// gets refused by it. Until this test existed none of the three were tied to
// anything.
//
// Widening currency.Pattern to admit a fourth letter is not a hypothetical: the
// owner holds crypto, and a longer code is the kind of change that would leave
// the Go suite and the contract test above green while all three dialogs kept
// Save disabled for a code the server now accepts — no error, nothing red,
// just a button that never lights up. This is the same shape of gap
// TestTheAmountFieldRefusesAtTheBoundTheServerEnforces
// (internal/platform/money/bound_sites_test.go) closed for
// money.MaxAmountMinor's own frontend copy.
var currencyFormSites = []string{
	"web/src/routes/settings/index.tsx",
	"web/src/routes/accounts/account-dialog.tsx",
	"web/src/routes/accounts/instrument-picker.tsx",
}

func TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces(t *testing.T) {
	// Each site writes the pattern as a JS regex literal, which is Pattern
	// itself between a pair of slashes — comparing the literal text rather than
	// re-deriving a regexp.Regexp from it, so this test fails the same way a
	// human diff would if the two ever read differently.
	want := "/" + currency.Pattern + "/"
	for _, rel := range currencyFormSites {
		body, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not contain the regex literal %s (currency.Pattern): "+
				"its Save button would enable for a code the server refuses, or stay "+
				"disabled for one the server would take", rel, want)
		}
	}
}
