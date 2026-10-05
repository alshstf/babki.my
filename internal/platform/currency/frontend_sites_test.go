package currency_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/currency"
)

// Three frontend dialogs copy currency.Pattern to enable their Save buttons;
// this holds the copies to the constant.
var currencyFormSites = []string{
	"web/src/routes/settings/index.tsx",
	"web/src/routes/accounts/account-dialog.tsx",
	"web/src/routes/accounts/instrument-picker.tsx",
}

func TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces(t *testing.T) {
	// Compared as the JS regex literal text.
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
