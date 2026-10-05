package currency_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/currency"
)

// The frontend's forms enable Save through isCurrencyCode in
// web/src/lib/currencies.ts; this holds its regex literal to currency.Pattern.
const currencyCodeSite = "web/src/lib/currencies.ts"

func TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces(t *testing.T) {
	// Compared as the JS regex literal text.
	want := "/" + currency.Pattern + "/"
	body, err := os.ReadFile(filepath.Join("..", "..", "..", currencyCodeSite))
	if err != nil {
		t.Fatalf("read %s: %v", currencyCodeSite, err)
	}
	if !strings.Contains(string(body), want) {
		t.Errorf("%s does not contain the regex literal %s (currency.Pattern): "+
			"a Save button would enable for a code the server refuses, or stay "+
			"disabled for one the server would take", currencyCodeSite, want)
	}
}
