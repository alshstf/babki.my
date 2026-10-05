package family_test

import (
	"regexp"
	"slices"
	"testing"

	"babki.my/babki/internal/family"
)

// notices returns the notice codes sorted: tests care about the set.
func notices(r family.TaxRules) []string {
	out := make([]string, 0, len(r.Notices()))
	for _, n := range r.Notices() {
		out = append(out, string(n))
	}
	slices.Sort(out)
	return out
}

// RU, DE and US are fifo/account with a checked norm: supported and silent. KZ
// is not among them (see the next test).
func TestFIFOWithinOneAccountIsExactlyTheseCountries(t *testing.T) {
	for _, country := range []string{"RU", "DE", "US"} {
		r := family.TaxRulesFor(country)
		if r.Method != family.MethodFIFO || r.Perimeter != family.PerimeterAccount {
			t.Errorf("%s = %s/%s, want fifo/account", country, r.Method, r.Perimeter)
		}
		if !r.Supported() {
			t.Errorf("%s: Supported() = false, want true", country)
		}
		if got := notices(r); len(got) != 0 {
			t.Errorf("%s: notices = %v, want none", country, got)
		}
	}
}

// KZ reads fifo/account but its norm is unverified, so it is not supported.
func TestKazakhstanNormIsUnverifiedSoItIsNotSilentlyAffirmed(t *testing.T) {
	r := family.TaxRulesFor("KZ")
	if r.Method != family.MethodFIFO || r.Perimeter != family.PerimeterAccount {
		t.Errorf("KZ = %s/%s, want fifo/account", r.Method, r.Perimeter)
	}
	if !r.NormUnverified {
		t.Error("KZ: NormUnverified = false, want true")
	}
	if r.Supported() {
		t.Error("KZ: Supported() = true, want false — the norm behind fifo/account was never established")
	}
	if got := notices(r); !slices.Equal(got, []string{"unverified_rule"}) {
		t.Errorf("KZ: notices = %v, want exactly [unverified_rule]", got)
	}
}

// A country that matches disposals another way gets a notice saying so.
func TestADifferentMethodIsSaidOutLoud(t *testing.T) {
	cases := map[string]struct {
		method    family.CostBasisMethod
		perimeter family.CostBasisPerimeter
		notices   []string
	}{
		// Section 104 pool: averaged, across everything held.
		"GB": {family.MethodAverage, family.PerimeterOwner, []string{"method_mismatch", "perimeter_mismatch"}},
		// ITA s.47: averaged across all holdings.
		"CA": {family.MethodAverage, family.PerimeterOwner, []string{"method_mismatch", "perimeter_mismatch"}},
		// ATO TD 33: the taxpayer nominates; only the method diverges.
		"AU": {family.MethodSpecificLot, family.PerimeterNotApplicable, []string{"method_mismatch"}},
	}
	for country, want := range cases {
		r := family.TaxRulesFor(country)
		if r.Method != want.method || r.Perimeter != want.perimeter {
			t.Errorf("%s = %s/%s, want %s/%s", country, r.Method, r.Perimeter, want.method, want.perimeter)
		}
		if r.Supported() {
			t.Errorf("%s: Supported() = true, want false", country)
		}
		if got := notices(r); !slices.Equal(got, want.notices) {
			t.Errorf("%s: notices = %v, want %v", country, got, want.notices)
		}
	}
}

// NL and CH do not tax gains: only not_taxed, no method notice.
func TestCountriesThatDoNotTaxGainsSayTheFiguresAreInformational(t *testing.T) {
	for _, country := range []string{"NL", "CH"} {
		r := family.TaxRulesFor(country)
		if r.Method != family.MethodNotApplicable || r.Perimeter != family.PerimeterNotApplicable {
			t.Errorf("%s = %s/%s, want not_applicable/not_applicable", country, r.Method, r.Perimeter)
		}
		if r.Supported() {
			t.Errorf("%s: Supported() = true, want false", country)
		}
		if got := notices(r); !slices.Equal(got, []string{"not_taxed"}) {
			t.Errorf("%s: notices = %v, want exactly [not_taxed]", country, got)
		}
	}
}

// An unknown code claims nothing and does not behave like Russia.
func TestAnUnknownCountryIsNotSilentlyRussia(t *testing.T) {
	r := family.TaxRulesFor("XX")
	if r == family.TaxRulesFor("RU") {
		t.Fatalf("XX resolved to the same rules as RU (%+v)", r)
	}
	if r.Method != family.MethodUnknown || r.Perimeter != family.PerimeterUnknown {
		t.Errorf("XX = %s/%s, want unknown/unknown", r.Method, r.Perimeter)
	}
	if r.Supported() {
		t.Error("XX: Supported() = true, want false")
	}
	if got := notices(r); !slices.Equal(got, []string{"unknown_country"}) {
		t.Errorf("XX: notices = %v, want exactly [unknown_country]", got)
	}
	if family.KnownTaxResidency("XX") {
		t.Error("KnownTaxResidency(XX) = true, want false")
	}
	if !family.KnownTaxResidency(family.DefaultTaxResidency) {
		t.Errorf("KnownTaxResidency(%s) = false: the default must itself be a country with rules", family.DefaultTaxResidency)
	}
}

// The owner-wide perimeter can be named though it is not implemented.
func TestTheOwnerWidePerimeterIsRepresentable(t *testing.T) {
	for _, country := range []string{"GB", "CA"} {
		if got := family.TaxRulesFor(country).Perimeter; got != family.PerimeterOwner {
			t.Errorf("%s perimeter = %s, want owner", country, got)
		}
	}
}

// The published list matches the lookup: same countries, sorted, valid codes.
func TestTaxResidenciesIsTheOneList(t *testing.T) {
	all := family.TaxResidencies()
	if len(all) == 0 {
		t.Fatal("TaxResidencies() is empty")
	}
	codeRe := regexp.MustCompile(`^[A-Z]{2}$`)
	codes := make([]string, 0, len(all))
	for _, r := range all {
		if !codeRe.MatchString(r.Country) {
			t.Errorf("country %q is not an ISO 3166-1 alpha-2 code", r.Country)
		}
		if r != family.TaxRulesFor(r.Country) {
			t.Errorf("%s: listed rules %+v differ from TaxRulesFor", r.Country, r)
		}
		if !family.KnownTaxResidency(r.Country) {
			t.Errorf("%s is listed but KnownTaxResidency says otherwise", r.Country)
		}
		codes = append(codes, r.Country)
	}
	if !slices.IsSorted(codes) {
		t.Errorf("TaxResidencies() = %v, want sorted by country", codes)
	}
}

// A country is supported exactly when it has no notices.
func TestSupportedAndNoticesNeverContradict(t *testing.T) {
	all := append(family.TaxResidencies(), family.TaxRulesFor("XX"))
	for _, r := range all {
		if r.Supported() != (len(r.Notices()) == 0) {
			t.Errorf("%s: Supported() = %v with notices %v", r.Country, r.Supported(), r.Notices())
		}
	}
}
