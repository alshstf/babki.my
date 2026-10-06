package portfolio

import "testing"

// Every type an instrument's journal can hold is folded by an applier of its
// own; only money moving on the account, with no paper, has none.
func TestEveryPaperTypeHasAnApplier(t *testing.T) {
	cashOnly := map[Type]bool{TypeDeposit: true, TypeWithdrawal: true, TypeInterest: true, TypeConversion: true}
	for typ := range validTypes {
		if _, ok := appliers[typ]; ok == cashOnly[typ] {
			t.Errorf("%s: applier present = %v, want %v", typ, ok, !cashOnly[typ])
		}
	}
}
