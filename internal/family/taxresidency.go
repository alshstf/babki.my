package family

import (
	"regexp"
	"slices"
	"strings"
)

// This file is the one place a country decides how cost basis is computed;
// nothing else tests a country code. Each row cites its norm, so the table can
// be checked against the law.
//
// The application implements one combination — FIFO within a single account —
// and says so (TaxRules.Notices) for every country whose law differs. A country
// the table does not know is reported as unknown, never treated as Russia.

// CostBasisMethod is how a jurisdiction decides which parcels a disposal consumed.
type CostBasisMethod string

const (
	// MethodFIFO releases the earliest acquisitions first; the only one implemented.
	MethodFIFO CostBasisMethod = "fifo"
	// MethodAverage pools every unit at one average cost. Not implemented.
	MethodAverage CostBasisMethod = "average"
	// MethodSpecificLot lets the taxpayer nominate the parcel. Not implemented.
	MethodSpecificLot CostBasisMethod = "specific_lot"
	// MethodNotApplicable is for a country that does not tax an individual's
	// capital gains — not "unknown".
	MethodNotApplicable CostBasisMethod = "not_applicable"
	// MethodUnknown means the country has no row in the table.
	MethodUnknown CostBasisMethod = "unknown"
)

// CostBasisPerimeter is the set of holdings the release queue spans; it
// changes the numbers even under the same method.
type CostBasisPerimeter string

const (
	// PerimeterAccount queues per account (depot, brokerage contract); the only
	// one implemented.
	PerimeterAccount CostBasisPerimeter = "account"
	// PerimeterOwner queues across everything the owner holds. Named, not
	// implemented, so the application can say it does not do it (GB, CA).
	PerimeterOwner CostBasisPerimeter = "owner"
	// PerimeterNotApplicable is for a country with no queue: the taxpayer picks,
	// or gains are untaxed.
	PerimeterNotApplicable CostBasisPerimeter = "not_applicable"
	PerimeterUnknown       CostBasisPerimeter = "unknown"
)

// CostBasisNotice is a code for one way the computation fails to answer for a
// country; the interface translates it.
type CostBasisNotice string

const (
	// NoticeMethodMismatch: the country picks parcels by another rule, so the
	// figures may not be its cost basis. "May": GB and CA pool, while AU lets the
	// taxpayer nominate but accepts FIFO absent an election, so the sentence must
	// hold for both.
	NoticeMethodMismatch CostBasisNotice = "method_mismatch"
	// NoticePerimeterMismatch: the country's computation spans more than one
	// account. Worded neutrally, since GB and CA pool rather than queue.
	NoticePerimeterMismatch CostBasisNotice = "perimeter_mismatch"
	// NoticeNotTaxed: capital gains are untaxed; the figures are informational.
	NoticeNotTaxed CostBasisNotice = "not_taxed"
	// NoticeUnknownCountry: no row, so nothing is claimed about the country.
	NoticeUnknownCountry CostBasisNotice = "unknown_country"
	// NoticeUnverifiedRule: the row's method and perimeter are an assumption by
	// analogy, not a rule found in the law (KZ). Supported must not affirm it.
	NoticeUnverifiedRule CostBasisNotice = "unverified_rule"
)

// TaxRules is one country's row. It is comparable, so a test can check an
// unknown country did not resolve to Russia's row. Notices are derived.
type TaxRules struct {
	Country   string
	Method    CostBasisMethod
	Perimeter CostBasisPerimeter
	// CapitalGainsTaxed is false where individuals' capital gains are untaxed. It
	// feeds the notices rather than being published.
	CapitalGainsTaxed bool
	// NormUnverified marks a row whose method and perimeter were not traced to a
	// norm. See NoticeUnverifiedRule.
	NormUnverified bool
}

// DefaultTaxResidency is a space's residency unless set otherwise, and what
// migration 0009 gave existing spaces: Russia's rules are what the engine has
// always computed.
const DefaultTaxResidency = "RU"

// taxResidencyRe is the ISO 3166-1 alpha-2 shape; the table decides whether a
// code is known.
var taxResidencyRe = regexp.MustCompile(`^[A-Z]{2}$`)

// taxRules maps a country of tax residency to its cost basis rules, one row
// each with its norm. Rows that differ from fifo/account exist so the
// application can say what it does not do.
var taxRules = map[string]TaxRules{
	// Russia. НК РФ ст. 214.1 п. 13: «по стоимости первых по времени
	// приобретений» — FIFO, mandatory. 425-ФЗ keeps the queue per brokerage
	// contract from 2027-01-01; an ИИС is already separate.
	"RU": {Country: "RU", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// Germany. § 20 Abs. 4 S. 7 EStG: FIFO, mandatory; per depot (BMF letter of
	// 2025-05-14).
	"DE": {Country: "DE", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// Kazakhstan. Gains are taxed at 10% with exchange-list and three-year
	// reliefs, but no mandatory method was found (research 2026-07, rechecked).
	// fifo/account is an analogy, no norm is cited, and NormUnverified marks it.
	// Replace when a cited method is found.
	"KZ": {Country: "KZ", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true, NormUnverified: true},

	// United States. 26 CFR 1.1012-1(c)(1)(i): FIFO by default (specific
	// identification permitted); the row records the default. Per account: 26 USC
	// 1012(c)(1), "on an account by account basis", as 1099-B reporting is.
	"US": {Country: "US", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// United Kingdom. TCGA 1992 s.104 pools identical securities at an average
	// cost across all the owner's holdings, after same-day and 30-day matching.
	"GB": {Country: "GB", Method: MethodAverage, Perimeter: PerimeterOwner, CapitalGainsTaxed: true},

	// Canada. ITA s.47: identical properties averaged across all holdings.
	"CA": {Country: "CA", Method: MethodAverage, Perimeter: PerimeterOwner, CapitalGainsTaxed: true},

	// Australia. ATO TD 33: the taxpayer may nominate the parcel; absent an
	// election FIFO is accepted. The row keeps specific_lot and its notice, which
	// over-warns in the no-election case; there is no queue to scope. Do not
	// "correct" it to fifo/account.
	"AU": {Country: "AU", Method: MethodSpecificLot, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: true},

	// Netherlands. Individuals' capital gains on securities are not taxed
	// (research 2026-07).
	"NL": {Country: "NL", Method: MethodNotApplicable, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: false},

	// Switzerland. A private investor's capital gains are exempt (research
	// 2026-07).
	"CH": {Country: "CH", Method: MethodNotApplicable, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: false},
}

// TaxRulesFor returns a country's rules; an unknown country comes back as
// unknown, never as the default.
func TaxRulesFor(country string) TaxRules {
	if r, ok := taxRules[country]; ok {
		return r
	}
	return TaxRules{Country: country, Method: MethodUnknown, Perimeter: PerimeterUnknown}
}

// KnownTaxResidency reports whether the table has the code. Writes refuse
// unknown codes, so unknown is reachable only through rows written around this
// application.
func KnownTaxResidency(country string) bool {
	_, ok := taxRules[country]
	return ok
}

// TaxResidencies returns every known country sorted by code, for the API, so
// clients offer exactly what the server accepts.
func TaxResidencies() []TaxRules {
	out := make([]TaxRules, 0, len(taxRules))
	for _, r := range taxRules {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b TaxRules) int { return strings.Compare(a.Country, b.Country) })
	return out
}

// taxResidencyCodes lists the known codes for a validation error. "Known
// rules" is not "supported": RU, DE and US are computed correctly, the rest
// carry notices.
func taxResidencyCodes() []string {
	out := make([]string, 0, len(taxRules))
	for code := range taxRules {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Supported reports whether the computation is this country's rule: having
// no notices, by definition.
func (r TaxRules) Supported() bool { return len(r.Notices()) == 0 }

// Notices lists every way the computation fails to answer for the country.
// All of them are returned (GB diverges in method and perimeter). An unknown
// country claims nothing and an untaxed one has no method to diverge from;
// NormUnverified is independent of both. Never nil, so JSON gets [].
func (r TaxRules) Notices() []CostBasisNotice {
	if r.Method == MethodUnknown || r.Perimeter == PerimeterUnknown {
		return []CostBasisNotice{NoticeUnknownCountry}
	}
	if !r.CapitalGainsTaxed {
		return []CostBasisNotice{NoticeNotTaxed}
	}
	out := []CostBasisNotice{}
	if r.Method != MethodFIFO {
		out = append(out, NoticeMethodMismatch)
	}
	switch r.Perimeter {
	case PerimeterAccount:
	case PerimeterNotApplicable:
		// No queue, no perimeter to get wrong; the method notice already covers it.
	default:
		out = append(out, NoticePerimeterMismatch)
	}
	if r.NormUnverified {
		out = append(out, NoticeUnverifiedRule)
	}
	return out
}
