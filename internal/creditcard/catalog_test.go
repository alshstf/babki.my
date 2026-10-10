package creditcard

import (
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Every version in the catalog (decision Р-32) makes terms a card takes, has
// the bank's documents and the days it is of.
func TestTheCatalogHolds(t *testing.T) {
	all, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 4 {
		t.Fatalf("catalog = %d products", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		if p.ID == "" || seen[p.ID] || p.Bank == "" || p.Card == "" || len(p.Versions) == 0 || len(p.Watch) == 0 {
			t.Errorf("product %+v", p)
		}
		seen[p.ID] = true
		for _, v := range p.Versions {
			if !dayPattern.MatchString(v.Revision) || !dayPattern.MatchString(v.CheckedOn) || len(v.Sources) == 0 ||
				v.ContractsFrom != nil && !dayPattern.MatchString(*v.ContractsFrom) {
				t.Errorf("%s: version %+v", p.ID, v)
			}
			api, err := v.Apply(TermsAPI(alfa))
			if err != nil {
				t.Fatalf("%s: %v", p.ID, err)
			}
			terms, err := termsFromAPI(uuid.New(), api)
			if err != nil {
				t.Fatalf("%s: %v", p.ID, err)
			}
			// The contract's day is the person's, for windows and first days.
			if terms.GraceKind == Windows || terms.Fees.IntroDays > 0 {
				opened := d("2026-07-10")
				terms.OpenedOn = &opened
			}
			if err := terms.Validate(); err != nil {
				t.Errorf("%s %s: %v", p.ID, v.Revision, err)
			}
			if len(v.Changes(api)) != 0 {
				t.Errorf("%s: applied terms still differ: %+v", p.ID, v.Changes(api))
			}
		}
	}
}

// A newer revision of the card's version is offered, field by field; the
// same revision is not.
func TestANewerRevisionIsOffered(t *testing.T) {
	p, ok := productByID("gpb-180-premium")
	if !ok {
		t.Fatal("no gpb-180-premium in the catalog")
	}
	v := p.Versions[0]
	api, err := v.Apply(TermsAPI(alfa))
	if err != nil {
		t.Fatal(err)
	}
	terms, err := termsFromAPI(uuid.New(), api)
	if err != nil {
		t.Fatal(err)
	}
	terms.AnnualRate = decimal.NewFromInt(50)
	terms.Catalog = &CatalogRef{Product: p.ID, ContractsFrom: v.ContractsFrom, Revision: "2026-01-01"}
	got, changes := catalogUpdate(terms)
	if got == nil || len(changes) != 1 || changes[0].Field != "annual_rate" || changes[0].Ours != "50" || changes[0].Theirs != "59.99" {
		t.Errorf("update = %+v, changes %+v", got, changes)
	}
	terms.Catalog.Revision = v.Revision
	if got, _ := catalogUpdate(terms); got != nil {
		t.Errorf("the same revision was offered: %+v", got)
	}
}
