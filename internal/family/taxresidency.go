package family

import (
	"regexp"
	"slices"
	"strings"
)

// This file is the single place where a country decides how a cost basis is
// computed. Everything downstream — the session, the positions response, the
// engine's own claim to be right — reads it from here; nothing anywhere else
// tests a country code.
//
// That concentration is deliberate. The rules genuinely differ (Britain has no
// parcels at all, Australia lets the taxpayer pick, the Netherlands does not
// tax the gain), and a rule spread across `if country == "RU"` conditions would
// be impossible to audit against the law and impossible to extend without
// finding every site. Here each row is one line with the norm it comes from
// written beside it, so a reader can check the table against the statute
// instead of against the code.
//
// The application implements exactly ONE of the combinations below — FIFO
// queued within a single account — and says so out loud for every country where
// that is not what the law requires (see TaxRules.Notices). Computing FIFO for
// a British resident and presenting the result as their cost basis would be a
// number that is wrong in a way nothing on screen could reveal; a wrong number
// with a warning attached is at least falsifiable, and a country the table does
// not know is reported as unknown rather than quietly treated as Russia.

// CostBasisMethod is how a jurisdiction decides WHICH of the parcels held are
// the ones a disposal consumed. It is the question that has to be answered
// before any cost basis exists at all.
type CostBasisMethod string

const (
	// MethodFIFO releases the earliest acquisitions first. The only method
	// implemented (see portfolio.Compute).
	MethodFIFO CostBasisMethod = "fifo"
	// MethodAverage has no parcels: every unit of an instrument carries the
	// same pooled average cost, so there is nothing to queue. Not implemented.
	MethodAverage CostBasisMethod = "average"
	// MethodSpecificLot lets the taxpayer nominate which parcel was sold, which
	// means the answer is an input rather than a computation. Not implemented.
	MethodSpecificLot CostBasisMethod = "specific_lot"
	// MethodNotApplicable is for a country that does not tax an individual's
	// capital gain at all: nothing has to be matched, because nothing is
	// declared. It is NOT "we do not know" — see MethodUnknown for that.
	MethodNotApplicable CostBasisMethod = "not_applicable"
	// MethodUnknown means the stored country has no row in this table. It
	// exists so an unrecognised code can be reported as such instead of falling
	// back to the default and behaving like Russia.
	MethodUnknown CostBasisMethod = "unknown"
)

// CostBasisPerimeter is the set of holdings the release queue is built over.
// Two jurisdictions can share a method and still disagree here, and the
// disagreement changes the numbers: a queue spanning every account releases a
// parcel bought at another broker before one bought later at this one.
type CostBasisPerimeter string

const (
	// PerimeterAccount queues separately per account (per depot, per brokerage
	// contract). The only perimeter implemented — the engine folds one
	// account's journal at a time.
	PerimeterAccount CostBasisPerimeter = "account"
	// PerimeterOwner queues across everything the owner holds, wherever it
	// sits. REPRESENTABLE BUT NOT IMPLEMENTED: Britain and Canada need it, and
	// naming it is what lets the application say it does not do it. Without the
	// name the only available answer would be the account perimeter, applied
	// silently.
	PerimeterOwner CostBasisPerimeter = "owner"
	// PerimeterNotApplicable is for a country with no queue to scope — the
	// taxpayer picks the parcel, or the gain is not taxed.
	PerimeterNotApplicable CostBasisPerimeter = "not_applicable"
	// PerimeterUnknown mirrors MethodUnknown: no row for the stored country.
	PerimeterUnknown CostBasisPerimeter = "unknown"
)

// CostBasisNotice is one way this application's computation fails to answer for
// a country. It is a code, never a sentence: every string the owner reads is
// the interface's to translate, so the server states the fact and the interface
// states it in Russian.
type CostBasisNotice string

const (
	// NoticeMethodMismatch: the country matches disposals by another method, so
	// the figures are not a cost basis under its rules.
	NoticeMethodMismatch CostBasisNotice = "method_mismatch"
	// NoticePerimeterMismatch: the queue must span more than the one account
	// the engine folds.
	NoticePerimeterMismatch CostBasisNotice = "perimeter_mismatch"
	// NoticeNotTaxed: the country does not tax an individual's capital gain, so
	// the figures are informational rather than fiscal.
	NoticeNotTaxed CostBasisNotice = "not_taxed"
	// NoticeUnknownCountry: no rules row, so nothing is claimed about the
	// country. The figures are still FIFO within one account — which is what
	// they always are — but this application does not know whether that is
	// right there.
	NoticeUnknownCountry CostBasisNotice = "unknown_country"
)

// TaxRules is one country's row. Comparable on purpose: two rows are equal when
// they say the same thing, which is what lets a test assert that an unknown
// country did NOT resolve to Russia's row without comparing field by field.
// Notices are derived (see Notices) rather than stored, so the table states
// facts about the law and this file derives the consequences for us from them
// in exactly one place.
type TaxRules struct {
	Country   string
	Method    CostBasisMethod
	Perimeter CostBasisPerimeter
	// CapitalGainsTaxed is false for a country that does not tax an
	// individual's capital gain on securities at all. It is not published
	// directly: it would have to be a third state for an unknown country, and a
	// boolean that is sometimes meaningless is worse than the notice it feeds.
	CapitalGainsTaxed bool
}

// DefaultTaxResidency is what a space has unless its owner says otherwise, and
// what migration 0009 gave every space that existed before the column did. It
// is Russia because this application's owner is a Russian resident and because
// Russia's rules are precisely the ones the engine has always computed — so the
// default states what those spaces already meant rather than guessing for them.
const DefaultTaxResidency = "RU"

// taxResidencyRe is the ISO 3166-1 alpha-2 shape. Shape only: whether a
// well-formed code is one this application can answer for is decided by the
// table below, not by a pattern.
var taxResidencyRe = regexp.MustCompile(`^[A-Z]{2}$`)

// taxRules maps a country of tax residency to its cost basis rules. ONE ROW PER
// COUNTRY, each with the norm it was read from. Adding a country means adding a
// line here and nothing else.
//
// The four rows that say fifo/account are the ones this application actually
// implements; every other row exists so the application can say what it does
// NOT do rather than compute something and let the figure speak for itself.
var taxRules = map[string]TaxRules{
	// Russia. НК РФ ст. 214.1 п. 13: disposals are valued "по стоимости первых
	// по времени приобретений" — FIFO, mandatory, no election. Perimeter: ФЗ
	// 425-ФЗ makes it explicit from 2027-01-01 that the queue is kept "по
	// договору на брокерское обслуживание", i.e. per brokerage contract; an ИИС
	// is already accounted for separately today.
	"RU": {Country: "RU", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// Germany. § 20 Abs. 4 S. 7 EStG: FIFO, mandatory. Perimeter: BMF letter of
	// 2025-05-14 keeps the queue per depot, so securities of the same issue held
	// at two banks form two queues.
	"DE": {Country: "DE", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// Kazakhstan. FIFO is mandatory (jurisdiction research, 2026-07). NO ARTICLE
	// IS CITED HERE BECAUSE NONE WAS ESTABLISHED — inventing a plausible-looking
	// reference in a table whose whole purpose is to be checkable against the
	// law would be the same class of error as inventing a number.
	"KZ": {Country: "KZ", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// United States. 26 CFR 1.1012-1(c)(1)(i): FIFO by default — "the earliest
	// lot the taxpayer purchased or acquired" — with specific identification
	// permitted, and averaging permitted for regulated investment company
	// shares. The row records the DEFAULT, which is what an application that
	// does not ask the owner to nominate parcels actually computes. Perimeter:
	// the IRS declined in 2010 to write "per account" into the regulation, but
	// brokers compute and report exactly that way, so per account is what a US
	// resident's statements will agree with.
	"US": {Country: "US", Method: MethodFIFO, Perimeter: PerimeterAccount, CapitalGainsTaxed: true},

	// United Kingdom. TCGA 1992 s.104: identical securities form a single pooled
	// holding with one averaged cost, so there are no dated parcels to queue at
	// all — the disagreement with FIFO is total, not a matter of ordering. The
	// pool spans everything the owner holds, across brokers.
	"GB": {Country: "GB", Method: MethodAverage, Perimeter: PerimeterOwner, CapitalGainsTaxed: true},

	// Canada. ITA s.47: identical properties are averaged into one adjusted cost
	// base, across all of the owner's holdings.
	"CA": {Country: "CA", Method: MethodAverage, Perimeter: PerimeterOwner, CapitalGainsTaxed: true},

	// Australia. ATO TD 33: the taxpayer may nominate which parcel was disposed
	// of. There is therefore no queue to scope — the perimeter is not "unknown",
	// it does not exist: a free choice has no ordering to be bounded.
	"AU": {Country: "AU", Method: MethodSpecificLot, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: true},

	// Netherlands. An individual's capital gain on securities is not taxed as
	// such (jurisdiction research, 2026-07), so no method has to match disposals
	// to acquisitions and no cost basis is declared anywhere.
	"NL": {Country: "NL", Method: MethodNotApplicable, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: false},

	// Switzerland. A private investor's capital gain on movable assets is
	// exempt (jurisdiction research, 2026-07); same consequence as NL.
	"CH": {Country: "CH", Method: MethodNotApplicable, Perimeter: PerimeterNotApplicable, CapitalGainsTaxed: false},
}

// TaxRulesFor returns the rules for a country of tax residency. A country with
// no row comes back as UNKNOWN — not as the default, and not as an error the
// caller might ignore. Falling back to Russia here is the precise failure this
// whole mechanism exists to prevent: the figures would be computed the same way
// either case, but only one of them admits that nothing is known about whether
// that way is right.
func TaxRulesFor(country string) TaxRules {
	if r, ok := taxRules[country]; ok {
		return r
	}
	return TaxRules{Country: country, Method: MethodUnknown, Perimeter: PerimeterUnknown}
}

// KnownTaxResidency reports whether the code is one this application has rules
// for. It is the write path's gate: a code that fails here is refused rather
// than stored, so the unknown case above is reachable only through a value that
// bypassed this application (a hand-edited row, a country later removed from
// the table) — and even then it is reported, not guessed at.
func KnownTaxResidency(country string) bool {
	_, ok := taxRules[country]
	return ok
}

// TaxResidencies returns every country the table knows, sorted by code. It is
// what the API publishes so a client offers exactly the countries the server
// will accept — a client with its own copy of the list would drift from this
// table the first time a country is added or corrected.
func TaxResidencies() []TaxRules {
	out := make([]TaxRules, 0, len(taxRules))
	for _, r := range taxRules {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b TaxRules) int { return strings.Compare(a.Country, b.Country) })
	return out
}

// taxResidencyCodes is the same list as country codes only, for naming what
// this application can answer for in a validation error the owner will read.
func taxResidencyCodes() []string {
	out := make([]string, 0, len(taxRules))
	for code := range taxRules {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Supported reports whether this application's computation IS this country's
// rule — and is DERIVED from Notices rather than restating its conditions.
// Written out separately the two would be one edit away from contradicting each
// other, and the contradiction would take the worst possible shape: a country
// affirmed as supported while carrying a notice that says the figures are not
// its cost basis. Having nothing to warn about is what "supported" means here,
// so it is defined that way.
func (r TaxRules) Supported() bool { return len(r.Notices()) == 0 }

// Notices lists every way this application's computation fails to answer for
// the country. It is the whole of the honesty rule, in one function.
//
// All applicable notices are returned, not the first: Britain diverges in BOTH
// the method and the perimeter, and reporting only the method would hide a
// second, independent reason the number is not a British cost basis. The two
// early returns are not shortcuts but statements — for an unknown country
// nothing at all is claimed, and for an untaxed one there is no method to
// diverge from, so adding "method_mismatch" would be a second falsehood on top
// of an already unnecessary one.
//
// It never returns nil, so a caller can hand the result straight to a JSON
// encoder and get [] rather than null: "no notices" and "the field is missing"
// must not look the same to a reader deciding whether to trust the figures.
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
		// Exactly what the engine folds: one account's journal at a time.
	case PerimeterNotApplicable:
		// The country defines no queue at all — the taxpayer nominates the
		// parcel — so there is no perimeter to get wrong. The method notice
		// above already names the whole of the divergence, and a second one
		// would report a single difference twice, which reads as two.
	default:
		out = append(out, NoticePerimeterMismatch)
	}
	return out
}
