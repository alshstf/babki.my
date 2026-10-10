package creditcard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/platform/apitypes"
)

// The catalog of tariffs (decision Р-32): a bank's card a file, its numbers
// by the dates of contracts, each version with the bank's documents and the
// day it was checked against them. Built into the program; what the bank
// itself says on a card stands over it all the same.
//
//go:embed catalog/*.yaml
var catalogFiles embed.FS

// Product is a bank's card in the catalog.
type Product struct {
	ID   string `yaml:"id"`
	Bank string `yaml:"bank"`
	Card string `yaml:"card"`
	// Watch are the bank's pages where a new revision of the tariff turns up,
	// for the monthly check against the bank's documents.
	Watch    []string  `yaml:"watch"`
	Versions []Version `yaml:"versions"`
}

// Version is a card's tariff for the contracts made from ContractsFrom to
// ContractsTo (YYYY-MM-DD; nil for no bound), as of the tariff's Revision.
// Terms are the fields of the card's terms the tariff sets, named as the API
// names them.
type Version struct {
	ContractsFrom *string        `yaml:"contracts_from"`
	ContractsTo   *string        `yaml:"contracts_to"`
	Revision      string         `yaml:"revision"`
	CheckedOn     string         `yaml:"checked_on"`
	Sources       []string       `yaml:"sources"`
	Notes         string         `yaml:"notes"`
	Terms         map[string]any `yaml:"terms"`
}

// CatalogRef is the catalog's version a card's terms were taken from: the
// product, the version (by its first contract day), and the tariff's
// revision then.
type CatalogRef struct {
	Product       string
	ContractsFrom *string
	Revision      string
}

var catalog struct {
	once     sync.Once
	products []Product
	err      error
}

// Catalog is the catalog of tariffs, by bank and card.
func Catalog() ([]Product, error) {
	catalog.once.Do(func() {
		catalog.products, catalog.err = readCatalog(catalogFiles)
	})
	return catalog.products, catalog.err
}

func readCatalog(files fs.FS) ([]Product, error) {
	names, err := fs.Glob(files, "catalog/*.yaml")
	if err != nil {
		return nil, err
	}
	var out []Product
	for _, name := range names {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		var p Product
		if err := yaml.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("credit card catalog: %s: %w", name, err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bank != out[j].Bank {
			return out[i].Bank < out[j].Bank
		}
		return out[i].Card < out[j].Card
	})
	return out, nil
}

// productByID is the catalog's product of the id.
func productByID(id string) (Product, bool) {
	all, err := Catalog()
	if err != nil {
		return Product{}, false
	}
	i := slices.IndexFunc(all, func(p Product) bool { return p.ID == id })
	if i < 0 {
		return Product{}, false
	}
	return all[i], true
}

// version is the product's version whose first contract day is from (nil
// for one with no bound).
func (p Product) version(from *string) (Version, bool) {
	for _, v := range p.Versions {
		if (v.ContractsFrom == nil) == (from == nil) && (from == nil || *v.ContractsFrom == *from) {
			return v, true
		}
	}
	return Version{}, false
}

// Apply lays the version's terms over a card's, nested fields one by one.
func (v Version) Apply(base apitypes.CreditCardTerms) (apitypes.CreditCardTerms, error) {
	m, err := asMap(base)
	if err != nil {
		return base, err
	}
	for k, val := range v.Terms {
		if sub, ok := val.(map[string]any); ok {
			into, _ := m[k].(map[string]any)
			if into == nil {
				into = map[string]any{}
			}
			for sk, sv := range sub {
				into[sk] = sv
			}
			m[k] = into
			continue
		}
		m[k] = val
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return base, err
	}
	var out apitypes.CreditCardTerms
	return out, json.Unmarshal(raw, &out)
}

// Change is a field of the card's terms the catalog's version sets
// otherwise: its name as the API names it (a nested one as «fees.cash_percent»),
// the card's value and the catalog's.
type Change struct {
	Field  string
	Ours   string
	Theirs string
}

// Changes are the version's fields the card's terms differ in.
func (v Version) Changes(base apitypes.CreditCardTerms) []Change {
	m, err := asMap(base)
	if err != nil {
		return nil
	}
	var out []Change
	add := func(field string, ours, theirs any) {
		if o, t := text(ours), text(theirs); o != t {
			out = append(out, Change{Field: field, Ours: o, Theirs: t})
		}
	}
	for k, val := range v.Terms {
		if sub, ok := val.(map[string]any); ok {
			into, _ := m[k].(map[string]any)
			for sk, sv := range sub {
				add(k+"."+sk, into[sk], sv)
			}
			continue
		}
		add(k, m[k], val)
	}
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Field, b.Field) })
	return out
}

func asMap(t apitypes.CreditCardTerms) (map[string]any, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(raw, &m)
}

// text is a value as it compares: numbers without a trailing «.0», nothing
// as empty.
func text(v any) string {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return fmt.Sprint(int64(f))
	}
	if reflect.TypeOf(v).Kind() == reflect.Map || reflect.TypeOf(v).Kind() == reflect.Slice {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	return fmt.Sprint(v)
}

// catalogUpdate is what the catalog's newer revision of the card's version
// would change in its terms; nil when it changes nothing or the card is
// not from the catalog.
func catalogUpdate(t Terms) (*Version, []Change) {
	if t.Catalog == nil {
		return nil, nil
	}
	p, ok := productByID(t.Catalog.Product)
	if !ok {
		return nil, nil
	}
	v, ok := p.version(t.Catalog.ContractsFrom)
	if !ok || v.Revision <= t.Catalog.Revision {
		return nil, nil
	}
	changes := v.Changes(TermsAPI(t))
	if len(changes) == 0 {
		return nil, nil
	}
	return &v, changes
}
