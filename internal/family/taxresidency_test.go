package family_test

import (
	"regexp"
	"slices"
	"testing"

	"babki.my/babki/internal/family"
)

// notices is a small helper: the tests below care about the SET of notices, not
// their order, and comparing sorted copies keeps a reordering of the rule table
// from failing them for no reason.
func notices(r family.TaxRules) []string {
	out := make([]string, 0, len(r.Notices()))
	for _, n := range r.Notices() {
		out = append(out, string(n))
	}
	slices.Sort(out)
	return out
}

// TestFIFOWithinOneAccountIsExactlyTheseCountries pins the countries whose rules
// the engine already implements: first-in-first-out, queued per account. These
// must come back supported and SILENT — a notice on any of them would tell the
// owner their own figures are wrong when they are not, which is as damaging as
// the silence this whole change removes.
func TestFIFOWithinOneAccountIsExactlyTheseCountries(t *testing.T) {
	for _, country := range []string{"RU", "DE", "KZ", "US"} {
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

// TestADifferentMethodIsSaidOutLoud is the point of the task: for a country
// that matches disposals some other way, the application must state that its
// cost basis is not that country's, rather than quietly computing FIFO and
// presenting the result as if it answered.
func TestADifferentMethodIsSaidOutLoud(t *testing.T) {
	cases := map[string]struct {
		method    family.CostBasisMethod
		perimeter family.CostBasisPerimeter
		notices   []string
	}{
		// Section 104 pool: one averaged cost per holding, no dated parcels at
		// all, and the pool spans everything the owner holds.
		"GB": {family.MethodAverage, family.PerimeterOwner, []string{"method_mismatch", "perimeter_mismatch"}},
		// ITA s.47 adjusted cost base: averaged over identical property
		// wherever the owner holds it.
		"CA": {family.MethodAverage, family.PerimeterOwner, []string{"method_mismatch", "perimeter_mismatch"}},
		// ATO TD 33: the taxpayer nominates the parcel, so there is no queue to
		// scope and only the method diverges.
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

// TestCountriesThatDoNotTaxGainsSayTheFiguresAreInformational covers the other
// half of the honesty rule. The Netherlands and Switzerland do not tax an
// individual's capital gains on securities at all, so there is no method to
// diverge from — saying "method_mismatch" there would be a second falsehood.
// The one thing to say is that the numbers are for reference.
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

// TestAnUnknownCountryIsNotSilentlyRussia is the failure mode the task names by
// hand: a code with no rules row must not fall through to the default and
// behave like Russia. Nothing may be claimed about it.
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

// TestTheOwnerWidePerimeterIsRepresentable pins that "the whole of the owner's
// holdings" exists in the model even though nothing implements it — Britain and
// Canada need it, and a perimeter that could not be named would have to be
// approximated by the account perimeter, silently.
func TestTheOwnerWidePerimeterIsRepresentable(t *testing.T) {
	for _, country := range []string{"GB", "CA"} {
		if got := family.TaxRulesFor(country).Perimeter; got != family.PerimeterOwner {
			t.Errorf("%s perimeter = %s, want owner", country, got)
		}
	}
}

// TestTaxResidenciesIsTheOneList checks the published list against the lookup
// it is published from: same countries, each row describing itself, sorted, and
// every code a well-formed ISO 3166-1 alpha-2. A list that drifted from the
// lookup would let a client offer a country the server then refuses.
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

// TestSupportedAndNoticesNeverContradict is the invariant the whole contract
// rests on: a country is affirmed exactly when there is nothing to warn about.
// A row affirmed as supported while carrying a notice — or the reverse — would
// let the interface show a green tick next to a statement that the figures are
// not this country's cost basis.
func TestSupportedAndNoticesNeverContradict(t *testing.T) {
	all := append(family.TaxResidencies(), family.TaxRulesFor("XX"))
	for _, r := range all {
		if r.Supported() != (len(r.Notices()) == 0) {
			t.Errorf("%s: Supported() = %v with notices %v", r.Country, r.Supported(), r.Notices())
		}
	}
}
