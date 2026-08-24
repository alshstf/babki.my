package portfolio

import "testing"

// TestMustMatchPositionCurrencyClassifiesEveryType pins, type by type and as
// literals, which operations have to be denominated in the position's own
// currency. The behaviour is exercised through Compute elsewhere
// (engine_income_currency_test.go), but only for the types that fixture can
// reach; this is the whole enum, so widening the exemption by one case — an
// amortization, say, whose amount really does retire basis — turns red here
// instead of quietly mixing two currencies into CostMinor.
//
// EVERY ENTRY CARRIES MONEY, because the rule is no longer about the type
// alone: an entry that moves nothing is exempt whatever its type, and a table
// of bare types could not tell that half of the rule from the other. The
// moneyless half is the table below this one.
//
// The count check is the other half: a Type added to the enum without a line
// here fails rather than inheriting an answer nobody chose.
func TestMustMatchPositionCurrencyClassifiesEveryType(t *testing.T) {
	want := map[Type]bool{
		// Money into a figure that holds one currency: a lot's cost, the
		// remaining basis, the basis an amortization retires by amount.
		TypeBuy:          true,
		TypeAmortization: true,
		TypeTransferIn:   true,
		TypeTransferOut:  true,
		// A conversion's legs carry the very same kind of figure a transfer's
		// do — the parcel's cost basis — into the very same single-currency
		// int64. The arriving leg is the one that makes the rule bite: it is
		// what SETTLES the new paper's cost currency, and it must settle it to
		// the currency the money was actually paid in rather than to whatever
		// the new paper is quoted in.
		TypeExchangeOut: true,
		TypeExchangeIn:  true,
		// A spin-off's legs answer the same way and for the same reason: the
		// departing one takes money out of the single-currency basis and the
		// arriving one settles the carved-out paper's cost currency to the
		// currency that money was paid in. The departing leg moves no units at
		// all, which changes nothing here — the question is about the money.
		TypeSpinoffOut: true,
		TypeSpinoffIn:  true,
		// A sale's proceeds and fee go to a Realization, which carries its own
		// currency, and what it retires is decided by the quantity sold. A
		// redemption is the same event by another name and answers the same.
		TypeSell:       false,
		TypeRedemption: false,
		// Income and commissions, both kept per currency and free to arrive in
		// any of them.
		TypeDividend: false,
		TypeCoupon:   false,
		TypeTax:      false,
		TypeFee:      false,
		// Never folded into a position at all: Compute skips a conversion and
		// refuses the rest by type. They answer with the safe default, which is
		// what a type added later inherits until somebody decides otherwise.
		TypeDeposit:    true,
		TypeWithdrawal: true,
		TypeInterest:   true,
		TypeConversion: true,
		// A split rewrites quantities and moves no money — but it is in this
		// table with a true because the answer here is the one for an entry
		// that DOES carry an amount, and every entry below does. A split's own
		// amount is always zero, so what it really answers is in the other
		// table; see TestAnEntryThatMovesNoMoneyNeedNotMatchTheCurrency.
		TypeSplit: true,
	}
	if len(want) != len(validTypes) {
		t.Fatalf("this table classifies %d types, the enum has %d — classify the new one", len(want), len(validTypes))
	}
	for typ, w := range want {
		// A non-zero amount on every entry: the exemption under test here is
		// the one the TYPE earns, not the one an empty entry earns anyway.
		o := Operation{Type: typ, AmountMinor: 1}
		if got := o.mustMatchPositionCurrency(); got != w {
			t.Errorf("%s.mustMatchPositionCurrency() = %v, want %v", typ, got, w)
		}
	}
}

// TestAnEntryThatMovesNoMoneyNeedNotMatchTheCurrency is the other half of the
// rule: a currency is a claim about a sum, and an entry with no sum makes none.
//
// This is what lets a securities transfer arrive with no cost attached under
// the paper's own currency while the receiving account holds it in another —
// the case that kept an incoming transfer of 2400 shares out of the owner's
// journal, over an amount of zero.
func TestAnEntryThatMovesNoMoneyNeedNotMatchTheCurrency(t *testing.T) {
	cases := []struct {
		name string
		op   Operation
		want bool
	}{
		{"transfer_in carrying no basis", Operation{Type: TypeTransferIn}, false},
		{"transfer_in carrying a basis", Operation{Type: TypeTransferIn, AmountMinor: 1}, true},
		{
			"transfer_in whose basis is only in its lots",
			Operation{Type: TypeTransferIn, TransferLots: []ReleasedLot{{CostMinor: 1}}},
			true,
		},
		{
			"transfer_in whose lots cost nothing",
			Operation{Type: TypeTransferIn, TransferLots: []ReleasedLot{{CostMinor: 0}}},
			false,
		},
		{"a purchase of nothing for nothing", Operation{Type: TypeBuy}, false},
		{"a purchase charging only a commission", Operation{Type: TypeBuy, FeeMinor: 1}, true},
		{"a split, which never carries an amount", Operation{Type: TypeSplit}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.op.mustMatchPositionCurrency(); got != c.want {
				t.Errorf("mustMatchPositionCurrency() = %v, want %v", got, c.want)
			}
		})
	}
}
