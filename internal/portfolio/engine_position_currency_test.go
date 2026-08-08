package portfolio

import "testing"

// TestMustMatchPositionCurrencyClassifiesEveryType pins, type by type and as
// literals, which operations have to be denominated in the position's own
// currency. The behaviour is exercised through Compute elsewhere
// (engine_income_currency_test.go), but only for the four types that fixture
// can reach; this is the whole enum, so widening the exemption by one case —
// an amortization, say, whose amount really does retire basis — turns red here
// instead of quietly mixing two currencies into CostMinor.
//
// The count check is the other half: a Type added to the enum without a line
// here fails rather than inheriting an answer nobody chose.
func TestMustMatchPositionCurrencyClassifiesEveryType(t *testing.T) {
	want := map[Type]bool{
		// Cost, quantity or fees — one int64 each, in one currency.
		TypeBuy:          true,
		TypeSell:         true,
		TypeAmortization: true,
		TypeTransferIn:   true,
		TypeTransferOut:  true,
		TypeSplit:        true,
		TypeFee:          true,
		// Income, kept per currency and free to arrive in any of them.
		TypeDividend: false,
		TypeCoupon:   false,
		TypeTax:      false,
		// Never folded into a position at all: Compute skips a conversion and
		// refuses the rest by type. They answer with the safe default, which is
		// what a type added later inherits until somebody decides otherwise.
		TypeDeposit:    true,
		TypeWithdrawal: true,
		TypeInterest:   true,
		TypeConversion: true,
	}
	if len(want) != len(validTypes) {
		t.Fatalf("this table classifies %d types, the enum has %d — classify the new one", len(want), len(validTypes))
	}
	for typ, w := range want {
		if got := typ.mustMatchPositionCurrency(); got != w {
			t.Errorf("%s.mustMatchPositionCurrency() = %v, want %v", typ, got, w)
		}
	}
}
