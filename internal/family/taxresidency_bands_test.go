package family

import "testing"

// The bands at their edges: 13 % up to 2,4 млн, 15 % on the rest; the wages'
// scale on 6 млн — 2,4 at 13 %, 2,6 at 15 %, 1 at 18 %.
func TestBanded(t *testing.T) {
	ru := TaxRulesFor("RU")
	for _, c := range []struct {
		sum, want int64
	}{
		{0, 0}, {-100, 0}, {100_000_00, 13_000_00}, {2_400_000_00, 312_000_00}, {3_000_000_00, 312_000_00 + 90_000_00},
	} {
		if got := Banded(ru.InvestmentBands[:], c.sum, 1); got != c.want {
			t.Errorf("investment %d: %d, want %d", c.sum, got, c.want)
		}
	}
	if got := Banded(ru.MainBands[:], 6_000_000_00, 1); got != 312_000_00+390_000_00+180_000_00 {
		t.Errorf("wages of 6 млн: %d", got)
	}
	if got := Banded(ru.InvestmentBands[:], 1, 1); got != 0 {
		t.Errorf("a kopeck: %d", got)
	}
	if got := Banded(ru.InvestmentBands[:], 4, 1); got != 1 {
		t.Errorf("four kopecks: %d, want 13 %% rounded half up", got)
	}
	// In whole roubles: 13 % of 3,84 is 0,4992 — nothing; of 3,85, 0,5005 — a rouble.
	if got := Banded(ru.InvestmentBands[:], 384, ru.TaxUnitMinor); got != 0 {
		t.Errorf("3,84: %d", got)
	}
	if got := Banded(ru.InvestmentBands[:], 385, ru.TaxUnitMinor); got != 100 {
		t.Errorf("3,85: %d", got)
	}
}
