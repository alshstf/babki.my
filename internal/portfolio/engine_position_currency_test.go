package portfolio

import "testing"

// Which types must match the position's currency, for the whole enum as
// literals; every entry here carries money (moneyless entries are the next
// test), and a type missing from the table fails the count.
func TestMustMatchPositionCurrencyClassifiesEveryType(t *testing.T) {
	want := map[Type]bool{
		// Money into a figure that holds one currency: a lot's cost, the
		// remaining basis, the basis an amortization retires by amount.
		TypeBuy:          true,
		TypeAmortization: true,
		TypeTransferIn:   true,
		TypeTransferOut:  true,
		// Conversion legs carry the parcel's basis into a single-currency figure; the
		// arriving one settles the new paper's currency to the money's.
		TypeExchangeOut: true,
		TypeExchangeIn:  true,
		// Spin-off legs likewise, though the departing one moves no units.
		TypeSpinoffOut: true,
		TypeSpinoffIn:  true,
		// A sale's proceeds go to a Realization with its own currency; a redemption
		// is the same.
		TypeSell:       false,
		TypeRedemption: false,
		// Income and commissions, both kept per currency and free to arrive in
		// any of them.
		TypeDividend: false,
		TypeCoupon:   false,
		TypeTax:      false,
		TypeFee:      false,
		// Never folded into a position; they take the strict default a new type
		// inherits.
		TypeDeposit:    true,
		TypeWithdrawal: true,
		TypeInterest:   true,
		TypeConversion: true,
		// A split's amount is always zero, so its real answer is in the other table.
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

// An entry moving no money need not match: it makes no claim (the owner's
// zero-cost transfer of 2 400 shares).
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
