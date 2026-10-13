package taxyear

import "testing"

// The codes the return wants, as ОКСМ and ОКВ write them: three digits, the
// leading zeros kept; none for an international ISIN or an unknown currency.
func TestTheReturnsCodes(t *testing.T) {
	for alpha, want := range map[string]string{"US": "840", "IE": "372", "KY": "136", "KZ": "398", "AM": "051", "AU": "036", "XS": "", "": "", "ZZ": ""} {
		if got := countryCode(alpha); got != want {
			t.Errorf("country %q = %q, want %q", alpha, got, want)
		}
	}
	for code, want := range map[string]string{"USD": "840", "EUR": "978", "AMD": "051", "HKD": "344", "XYZ": ""} {
		if got := currencyCode(code); got != want {
			t.Errorf("currency %q = %q, want %q", code, got, want)
		}
	}
}
