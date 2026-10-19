// Package loyalty is the catalog of the banks' loyalty programmes (decision
// Р-58): each programme's categories as sets of merchant category codes, how
// a cardholder picks them and for how long, and what earns no bonus at all.
// The percents a bank offers change each period with the offer itself, so
// they are not here: the family says them when it says what it picked.
package loyalty

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/intermediary"
)

//go:embed catalog/*.yaml
var catalogFiles embed.FS

// Period is how long a pick of categories holds.
type Period string

const (
	PeriodMonth     Period = "month"
	PeriodTwoMonths Period = "two_months"
	PeriodQuarter   Period = "quarter"
	// PeriodNone: the bank picks nothing for the cardholder to choose — the
	// bonus is by the categories as they are.
	PeriodNone Period = "none"
)

// Programme is a bank's loyalty programme, its editions oldest first.
type Programme struct {
	ID       string    `yaml:"id"`
	Bank     string    `yaml:"bank"`
	Name     string    `yaml:"name"`
	Watch    []string  `yaml:"watch"`
	Editions []Edition `yaml:"editions"`
}

// Edition is the programme's terms from From on, as Sources say them.
// Excluded are the codes a purchase earns no bonus with — on a bonus for
// everything; a category that names a code itself takes it all the same, as
// the banks' rules say («unless the code is in a category picked»).
type Edition struct {
	From       string     `yaml:"from"`
	CheckedOn  string     `yaml:"checked_on"`
	Sources    []string   `yaml:"sources"`
	Notes      string     `yaml:"notes"`
	Period     Period     `yaml:"period"`
	Variants   []Variant  `yaml:"variants"`
	Categories []Category `yaml:"categories"`
	Excluded   []string   `yaml:"excluded"`
}

// Variant is one card's or package's terms within the edition: how many
// categories its holder picks at once (Pick) of how many the bank offers
// (Of, 0 when the bank does not say), and the most the bank pays for a
// period, in roubles' minor units (0 when the documents set none).
type Variant struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	Pick int    `yaml:"pick"`
	Of   int    `yaml:"of"`
	Cap  int64  `yaml:"cap_minor"`
}

// Category is a set of merchant category codes under the bank's name for it:
// a code «5411» or a range «3000-3303». ByMerchant: the bank names the shops
// rather than codes, so a purchase cannot be told into it by its code. All
// is a category of many: «every» purchase with a code but the edition's
// exclusions (Т-Банк's «1 % на все покупки»), or every purchase with a code
// «listed» in the edition's other categories (ВТБ's «Все покупки»).
type Category struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	MCC        []string `yaml:"mcc"`
	ByMerchant bool     `yaml:"by_merchant"`
	All        AllOf    `yaml:"all"`
}

// AllOf is what a category of many takes.
type AllOf string

const (
	AllEvery  AllOf = "every"
	AllListed AllOf = "listed"
)

var catalog struct {
	once  sync.Once
	progs []Programme
	err   error
}

// Catalog is the catalog of programmes, by bank and name.
func Catalog() ([]Programme, error) {
	catalog.once.Do(func() {
		catalog.progs, catalog.err = readCatalog(catalogFiles)
	})
	return catalog.progs, catalog.err
}

// ByID is the catalog's programme of the id.
func ByID(id string) (Programme, bool) {
	all, err := Catalog()
	if err != nil {
		return Programme{}, false
	}
	i := slices.IndexFunc(all, func(p Programme) bool { return p.ID == id })
	if i < 0 {
		return Programme{}, false
	}
	return all[i], true
}

// EditionOn is the edition in force on day: the last one from day or before.
func (p Programme) EditionOn(day time.Time) (Edition, bool) {
	d := day.Format(time.DateOnly)
	var out Edition
	found := false
	for _, e := range p.Editions {
		if e.From > d {
			break
		}
		out, found = e, true
	}
	return out, found
}

// CategoriesOf are the edition's categories a purchase with mcc falls into:
// those naming its code, and those of many that take it — unless the code is
// excluded.
func (e Edition) CategoriesOf(mcc string) []Category {
	var out []Category
	listed := false
	for _, c := range e.Categories {
		if c.All == "" && Matches(c.MCC, mcc) {
			out = append(out, c)
			listed = true
		}
	}
	if !span4(mcc) || e.Excludes(mcc) {
		return out
	}
	var many []Category
	for _, c := range e.Categories {
		if c.All == AllEvery || (c.All == AllListed && listed) {
			many = append(many, c)
		}
	}
	return append(many, out...)
}

// span4 says mcc is four digits.
func span4(mcc string) bool {
	_, _, ok := span(mcc)
	return ok && !strings.Contains(mcc, "-")
}

// Excludes says a purchase with mcc earns no bonus outside the categories
// that name its code.
func (e Edition) Excludes(mcc string) bool { return Matches(e.Excluded, mcc) }

// Matches says mcc is one of codes, or within one of their ranges.
func Matches(codes []string, mcc string) bool {
	n, err := strconv.Atoi(mcc)
	if err != nil || len(mcc) != 4 {
		return false
	}
	for _, c := range codes {
		lo, hi, ok := span(c)
		if ok && n >= lo && n <= hi {
			return true
		}
	}
	return false
}

// span is a code's or a range's bounds; ok is false for neither.
func span(code string) (lo, hi int, ok bool) {
	a, b, isRange := strings.Cut(code, "-")
	if !isRange {
		b = a
	}
	if len(a) != 4 || len(b) != 4 {
		return 0, 0, false
	}
	lo, err1 := strconv.Atoi(a)
	hi, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

func readCatalog(files fs.FS) ([]Programme, error) {
	names, err := fs.Glob(files, "catalog/*.yaml")
	if err != nil {
		return nil, err
	}
	banks, err := intermediary.Load()
	if err != nil {
		return nil, err
	}
	var out []Programme
	seen := map[string]bool{}
	for _, name := range names {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		var p Programme
		if err := yaml.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("loyalty catalog: %s: %w", name, err)
		}
		if err := p.check(banks); err != nil {
			return nil, fmt.Errorf("loyalty catalog: %s: %w", name, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("loyalty catalog: %s: id %s twice", name, p.ID)
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bank != out[j].Bank {
			return out[i].Bank < out[j].Bank
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

var periods = []Period{PeriodMonth, PeriodTwoMonths, PeriodQuarter, PeriodNone}

// check is what makes a programme one a purchase can be told into.
func (p Programme) check(banks intermediary.Catalog) error {
	if p.ID == "" || p.Name == "" {
		return fmt.Errorf("a programme without an id or a name")
	}
	if b, ok := banks.ByID(p.Bank); !ok || b.Kind != intermediary.Bank {
		return fmt.Errorf("%s: no bank %q in the catalog of banks", p.ID, p.Bank)
	}
	if len(p.Editions) == 0 {
		return fmt.Errorf("%s: no editions", p.ID)
	}
	prev := ""
	for _, e := range p.Editions {
		if _, err := time.Parse(time.DateOnly, e.From); err != nil || e.From <= prev {
			return fmt.Errorf("%s: edition from %q: dates go up, YYYY-MM-DD", p.ID, e.From)
		}
		prev = e.From
		if err := e.check(); err != nil {
			return fmt.Errorf("%s %s: %w", p.ID, e.From, err)
		}
	}
	return nil
}

func (e Edition) check() error {
	if _, err := time.Parse(time.DateOnly, e.CheckedOn); err != nil || len(e.Sources) == 0 {
		return fmt.Errorf("checked_on and sources are required")
	}
	if !slices.Contains(periods, e.Period) {
		return fmt.Errorf("period %q", e.Period)
	}
	if len(e.Variants) == 0 || len(e.Categories) == 0 {
		return fmt.Errorf("no variants or no categories")
	}
	ids := map[string]bool{}
	for _, v := range e.Variants {
		if v.ID == "" || v.Name == "" || ids[v.ID] {
			return fmt.Errorf("variant %q: empty or twice", v.ID)
		}
		ids[v.ID] = true
		switch {
		case e.Period == PeriodNone && v.Pick != 0:
			return fmt.Errorf("variant %s: nothing to pick in a programme without a choice", v.ID)
		case e.Period != PeriodNone && (v.Pick < 1 || (v.Of != 0 && v.Of < v.Pick)):
			return fmt.Errorf("variant %s: picks %d of %d", v.ID, v.Pick, v.Of)
		case v.Cap < 0:
			return fmt.Errorf("variant %s: cap %d", v.ID, v.Cap)
		}
	}
	cats := map[string]bool{}
	for _, c := range e.Categories {
		if c.ID == "" || c.Name == "" || cats[c.ID] {
			return fmt.Errorf("category %q: empty or twice", c.ID)
		}
		cats[c.ID] = true
		if c.All != "" && c.All != AllEvery && c.All != AllListed {
			return fmt.Errorf("category %s: all %q, every or listed", c.ID, c.All)
		}
		if len(c.MCC) == 0 && !c.ByMerchant && c.All == "" {
			return fmt.Errorf("category %s: no codes, and not by merchant nor of many", c.ID)
		}
		if c.All != "" && len(c.MCC) > 0 {
			return fmt.Errorf("category %s: of many, yet with codes of its own", c.ID)
		}
		for _, code := range c.MCC {
			if _, _, ok := span(code); !ok {
				return fmt.Errorf("category %s: code %q is neither four digits nor a range of them", c.ID, code)
			}
		}
	}
	for _, code := range e.Excluded {
		if _, _, ok := span(code); !ok {
			return fmt.Errorf("excluded code %q is neither four digits nor a range of them", code)
		}
	}
	return nil
}

// PeriodOf is the period of the pick day falls in, its first and last days:
// a calendar month; two months from an odd one (January–February,
// March–April…), as Новикомбанк counts them; a calendar quarter. Without a
// choice, the month — what a bank pays its bonus for.
func (p Period) PeriodOf(day time.Time) (from, to time.Time) {
	m := day.Month()
	months := 1
	switch p {
	case PeriodTwoMonths:
		m -= (m - 1) % 2
		months = 2
	case PeriodQuarter:
		m -= (m - 1) % 3
		months = 3
	}
	from = time.Date(day.Year(), m, 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, months, -1)
}
