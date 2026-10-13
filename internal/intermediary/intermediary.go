// Package intermediary is the program's catalog of banks and brokers
// (decision Р-47 В): names and former names, so a family's accounts are told
// apart by who holds them; for a bank its Bank of Russia number and whether
// deposits with it are insured; for a broker its bank, if any, and its
// country, a broker abroad being outside Russian deposit insurance.
package intermediary

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Kind is what an intermediary is for an account.
type Kind string

const (
	Bank   Kind = "bank"
	Broker Kind = "broker"
)

// Intermediary is one bank or broker. Reg is a bank's number at the Bank of
// Russia; Insured, that deposits with it are insured; BankID, a broker's
// bank; Country, a broker's country (ISO), "RU" when unsaid.
type Intermediary struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name"`
	Reg     int      `yaml:"reg"`
	BankID  string   `yaml:"bank"`
	Country string   `yaml:"country"`
	Aliases []string `yaml:"aliases"`
	Kind    Kind     `yaml:"-"`
}

// Insured says deposits with a bank are insured: every bank of the catalog
// takes them and is open (the catalog's checked_on).
func (i Intermediary) Insured() bool { return i.Kind == Bank }

// Foreign says the intermediary is abroad.
func (i Intermediary) Foreign() bool { return i.Country != "RU" }

//go:embed catalog.yaml
var catalogFile []byte

// Catalog is the program's banks and brokers, banks first.
type Catalog struct {
	CheckedOn string
	All       []Intermediary
	byID      map[string]int
	byName    map[string]int
}

var (
	once   sync.Once
	loaded Catalog
	err    error
)

// Load is the catalog, read once.
func Load() (Catalog, error) {
	once.Do(func() { loaded, err = parse(catalogFile) })
	return loaded, err
}

var iso = regexp.MustCompile(`^[A-Z]{2}$`)

func parse(raw []byte) (Catalog, error) {
	var file struct {
		CheckedOn string         `yaml:"checked_on"`
		Banks     []Intermediary `yaml:"banks"`
		Brokers   []Intermediary `yaml:"brokers"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return Catalog{}, fmt.Errorf("intermediary: catalog: %w", err)
	}
	c := Catalog{CheckedOn: file.CheckedOn, byID: map[string]int{}, byName: map[string]int{}}
	add := func(i Intermediary, kind Kind) error {
		i.Kind = kind
		if i.Country == "" {
			i.Country = "RU"
		}
		switch {
		case i.ID == "" || i.Name == "":
			return fmt.Errorf("intermediary: an entry without an id or a name")
		case !iso.MatchString(i.Country):
			return fmt.Errorf("intermediary: %s: country %q", i.ID, i.Country)
		case kind == Bank && i.Reg <= 0:
			return fmt.Errorf("intermediary: bank %s without its number", i.ID)
		}
		if _, dup := c.byID[i.ID]; dup {
			return fmt.Errorf("intermediary: %s twice", i.ID)
		}
		c.byID[i.ID] = len(c.All)
		for _, n := range append([]string{i.Name}, i.Aliases...) {
			key := fold(n) + "|" + string(kind)
			if j, dup := c.byName[key]; dup && j != len(c.All) {
				return fmt.Errorf("intermediary: %q names both %s and %s", n, c.All[j].ID, i.ID)
			}
			c.byName[key] = len(c.All)
		}
		c.All = append(c.All, i)
		return nil
	}
	for _, b := range file.Banks {
		if err := add(b, Bank); err != nil {
			return Catalog{}, err
		}
	}
	for _, b := range file.Brokers {
		if err := add(b, Broker); err != nil {
			return Catalog{}, err
		}
	}
	for _, i := range c.All {
		if i.BankID != "" {
			if j, ok := c.byID[i.BankID]; !ok || c.All[j].Kind != Bank {
				return Catalog{}, fmt.Errorf("intermediary: broker %s of no bank %q", i.ID, i.BankID)
			}
		}
	}
	return c, nil
}

// ByID is the intermediary with this id.
func (c Catalog) ByID(id string) (Intermediary, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Intermediary{}, false
	}
	return c.All[i], true
}

// Match is the intermediary of this kind an account's «bank or broker» names,
// by its name or a former one, case, quotes, «ё» and legal forms aside.
func (c Catalog) Match(name string, kind Kind) (Intermediary, bool) {
	i, ok := c.byName[fold(name)+"|"+string(kind)]
	if !ok {
		return Intermediary{}, false
	}
	return c.All[i], true
}

var (
	legalForms = regexp.MustCompile(`(^|\s)(пао|ао|ооо|зао|оао|акб|кб|ао кб)(\s|$)`)
	punct      = regexp.MustCompile(`["«»“”'.,()]`)
	spaces     = regexp.MustCompile(`\s+`)
)

// fold is a name as matching reads it.
func fold(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "ё", "е"))
	s = punct.ReplaceAllString(s, " ")
	s = spaces.ReplaceAllString(" "+s+" ", " ")
	for legalForms.MatchString(s) {
		s = legalForms.ReplaceAllString(s, " ")
	}
	return strings.TrimSpace(strings.ReplaceAll(spaces.ReplaceAllString(s, " "), "-", " "))
}
