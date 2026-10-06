package instrument

import "testing"

// The list holds Russia's dollar and euro external loan bonds, and none of its
// rouble ones or anyone else's.
func TestRussianExternalBondsAreTheForeignCurrencyIssuesOfMinfin(t *testing.T) {
	for _, isin := range []string{"XS0088543193", " ru000a0jxu14 ", "RU000A102CK5"} {
		if !RussianExternalBond(isin) {
			t.Errorf("%q (RUS-28, RUS-47, RUS-27 EUR) is not recognised", isin)
		}
	}
	for _, isin := range []string{"XS0564087541", "RU000A1038V6", "US0378331005", ""} {
		if RussianExternalBond(isin) {
			t.Errorf("%q (a rouble eurobond, an OFZ, Apple, nothing) is taken for one", isin)
		}
	}
	for isin := range russianExternalBonds {
		if _, err := NormalizeISIN(isin); err != nil || len(isin) != 12 {
			t.Errorf("%q is not an ISIN: %v", isin, err)
		}
	}
	if len(russianExternalBonds) != 29 {
		t.Errorf("%d issues listed, want the 29 the exchange knows", len(russianExternalBonds))
	}
}
