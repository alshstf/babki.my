// Package deposit is the catalog of the banks' deposit and savings account
// programmes (decisions Р-52 Б, Р-53 Б) and the rate an account of one earns
// on a day: by the edition of the bank's terms in force, the variant chosen
// at opening, the key rate where the rate is tied to it, the months since
// opening and the conditions of the bonuses met.
package deposit

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"babki.my/babki/internal/intermediary"
)

// The catalog: a programme a file, each edition with the bank's documents
// and the day it was checked against them. Built into the program; a rate
// written in by hand stands over it all the same (Р-51).
//
//go:embed catalog/*.yaml
var catalogFiles embed.FS

// Kind is a deposit for a term, or a savings account without one.
type Kind string

const (
	KindDeposit Kind = "deposit"
	KindSavings Kind = "savings"
)

// Programme is a bank's deposit or savings account.
type Programme struct {
	ID string `yaml:"id"`
	// Bank is the bank's id in the program's catalog of banks (Р-47).
	Bank string `yaml:"bank"`
	Name string `yaml:"name"`
	Kind Kind   `yaml:"kind"`
	// Watch are the bank's pages where a new edition turns up, for the
	// monthly check against the bank's documents.
	Watch    []string  `yaml:"watch"`
	Editions []Edition `yaml:"editions"`
}

// Edition is the programme's terms from From on: a deposit opened or
// prolonged from that day keeps them for its term; a savings account has
// them every day from that day, until the next edition.
type Edition struct {
	From      string    `yaml:"from"`
	CheckedOn string    `yaml:"checked_on"`
	Sources   []string  `yaml:"sources"`
	Notes     string    `yaml:"notes"`
	Variants  []Variant `yaml:"variants"`
}

// Accrual is what a savings account's interest is reckoned on: the balance
// of each day, or the month's smallest.
type Accrual string

const (
	AccrualDaily   Accrual = "daily"
	AccrualMinimum Accrual = "minimum"
)

// Payout is how interest is paid: at the end of the term, every month onto
// a card, added to the sum every month, every day, every quarter.
type Payout string

const (
	PayoutEnd        Payout = "end"
	PayoutMonthly    Payout = "monthly"
	PayoutCapitalize Payout = "capitalize"
	PayoutDaily      Payout = "daily"
	PayoutQuarterly  Payout = "quarterly"
)

// Ops are the operations a deposit allows after opening.
type Ops string

const (
	OpsNone          Ops = "none"
	OpsTopUp         Ops = "topup"
	OpsTopUpWithdraw Ops = "topup_withdraw"
)

// TermUnit is what a deposit's term is counted in, as the bank counts it.
type TermUnit string

const (
	TermDays   TermUnit = "days"
	TermMonths TermUnit = "months"
)

// Variant is what the family chooses at opening. Payouts are the ways of
// payment at the same rates; a way with rates of its own is a variant of
// its own.
type Variant struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Accrual  Accrual  `yaml:"accrual"`
	Payouts  []Payout `yaml:"payouts"`
	Ops      Ops      `yaml:"ops"`
	TermUnit TermUnit `yaml:"term_unit"`
	// Grid is the rate by term and sum. Split: each part of the balance
	// takes the rate of its own step (a savings account at the key rate on
	// up to 1 млн and 0.6 of it above); otherwise the whole sum takes the
	// rate of the step it reaches.
	Grid  []Cell `yaml:"grid"`
	Split bool   `yaml:"split"`
	// Cap: the part of the sum above Amount earns Percent, or Share of the
	// rate (ПСБ: 0.65 of it above 20 млн).
	Cap *Cap `yaml:"cap"`
	// Schedule is the rate by the months since opening that differs from the
	// grid: a welcome rate for the first months, a ladder.
	Schedule []Period `yaml:"schedule"`
	Bonuses  []Bonus  `yaml:"bonuses"`
	// Key is the day a new key rate starts to count, for a rate tied to it.
	Key *KeyLag `yaml:"key"`
	// MGS is the minimum guaranteed rate the bank states (since 01.10.2022).
	MGS string `yaml:"mgs"`
}

// Cell is a rate for a term — From to To in the variant's unit, both in —
// and a sum from AmountFrom (minor units), fixed or tied to the key rate.
type Cell struct {
	From       int   `yaml:"from"`
	To         int   `yaml:"to"`
	AmountFrom int64 `yaml:"amount_from"`
	Rate       `yaml:",inline"`
}

// Rate is Percent a year, or KeyTimes × the key rate + KeyPlus, not below
// Floor.
type Rate struct {
	Percent  string `yaml:"percent"`
	KeyTimes string `yaml:"key_times"`
	KeyPlus  string `yaml:"key_plus"`
	Floor    string `yaml:"floor"`
}

// Cap is the part of the sum above Amount and what it earns.
type Cap struct {
	Amount  int64  `yaml:"amount"`
	Percent string `yaml:"percent"`
	Share   string `yaml:"share"`
}

// Period is a stretch from opening with a rate of its own: Months calendar
// months counting the month of opening, or Days days. Its Grid replaces the
// variant's, or Plus is added to it. Up to Cap only; above it the variant's
// rate. Requires names the condition of having it — a new client — and
// Bonuses whether the bonuses add to it.
type Period struct {
	Name     string `yaml:"name"`
	Months   int    `yaml:"months"`
	Days     int    `yaml:"days"`
	Grid     []Cell `yaml:"grid"`
	Plus     string `yaml:"plus"`
	Cap      int64  `yaml:"cap"`
	Requires string `yaml:"requires"`
	Bonuses  bool   `yaml:"bonuses"`
}

// Check is how a bonus's condition can be told from the program's own data,
// for the accounts kept in the automatic mode (Р-53): purchases on the
// bank's cards in a month, wages or a pension paid into the bank, the
// balance kept in the bank; manual — only by the family's mark (a
// subscription, a loyalty level, a long-term savings programme).
type Check string

const (
	CheckManual      Check = "manual"
	CheckCardSpend   Check = "card_spend"
	CheckWages       Check = "wages"
	CheckBankBalance Check = "bank_balance"
)

// Bonus is an addition to the rate while a condition holds. Levels, from
// the lowest, add more the more is met (purchases from 20 000 ₽ +1, from
// 50 000 ₽ +4); a bonus of one level has Plus. Of the bonuses of one Group
// only the greatest counts (wages, a pension or a subscription — one of
// them). Up to Cap only. NextMonth: the condition of a month earns the
// bonus in the next.
type Bonus struct {
	ID        string  `yaml:"id"`
	Name      string  `yaml:"name"`
	Plus      string  `yaml:"plus"`
	Levels    []Level `yaml:"levels"`
	Check     Check   `yaml:"check"`
	Group     string  `yaml:"group"`
	Cap       int64   `yaml:"cap"`
	NextMonth bool    `yaml:"next_month"`
}

// Level is a step of a bonus: from From of what is measured, Plus.
type Level struct {
	From int64  `yaml:"from"`
	Plus string `yaml:"plus"`
}

// Requirements are the conditions of having a period, by id, as named to
// the family.
var Requirements = map[string]string{
	"new_client": "Новый клиент банка — по условиям приветственной ставки",
}

// KeyLag is when a new key rate starts to count: the same day (no lag), the
// next working day, or by the month — a change on or before the Cutoff day
// from the Workday-th working day of the next month, a later one from its
// LateWorkday-th.
type KeyLag struct {
	NextWorkday bool `yaml:"next_workday"`
	Cutoff      int  `yaml:"cutoff"`
	Workday     int  `yaml:"workday"`
	LateWorkday int  `yaml:"late_workday"`
}

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

// HasVariant says some edition has a variant of the id.
func (p Programme) HasVariant(id string) bool {
	for _, e := range p.Editions {
		if slices.ContainsFunc(e.Variants, func(v Variant) bool { return v.ID == id }) {
			return true
		}
	}
	return false
}

// Bonus is the variant's bonus of the id, as the latest edition that has it
// states it.
func (p Programme) Bonus(variant, id string) (Bonus, bool) {
	for i := len(p.Editions) - 1; i >= 0; i-- {
		v, ok := p.Editions[i].Variant(variant)
		if !ok {
			continue
		}
		for _, b := range v.Bonuses {
			if b.ID == id {
				return b, true
			}
		}
	}
	return Bonus{}, false
}

// Conditions are the conditions of the variant across the editions — its
// bonuses and its periods' requirements — each with its number of levels:
// a bonus's steps, 1 for a bonus of one or a requirement.
func (p Programme) Conditions(variant string) map[string]int {
	out := map[string]int{}
	for _, e := range p.Editions {
		for _, v := range e.Variants {
			if v.ID != variant {
				continue
			}
			for _, b := range v.Bonuses {
				out[b.ID] = max(out[b.ID], len(b.Levels), 1)
			}
			for _, s := range v.Schedule {
				if s.Requires != "" {
					out[s.Requires] = max(out[s.Requires], 1)
				}
			}
		}
	}
	return out
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
			return nil, fmt.Errorf("deposit catalog: %s: %w", name, err)
		}
		if err := p.check(banks); err != nil {
			return nil, fmt.Errorf("deposit catalog: %s: %w", name, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("deposit catalog: %s: id %s twice", name, p.ID)
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

// check is what makes a programme one the rate can be reckoned from.
func (p Programme) check(banks intermediary.Catalog) error {
	if p.ID == "" || p.Name == "" {
		return fmt.Errorf("a programme without an id or a name")
	}
	if b, ok := banks.ByID(p.Bank); !ok || b.Kind != intermediary.Bank {
		return fmt.Errorf("%s: no bank %q in the catalog of banks", p.ID, p.Bank)
	}
	if p.Kind != KindDeposit && p.Kind != KindSavings {
		return fmt.Errorf("%s: kind %q", p.ID, p.Kind)
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
		if _, err := time.Parse(time.DateOnly, e.CheckedOn); err != nil || len(e.Sources) == 0 {
			return fmt.Errorf("%s %s: checked_on and sources are required", p.ID, e.From)
		}
		if len(e.Variants) == 0 {
			return fmt.Errorf("%s %s: no variants", p.ID, e.From)
		}
		ids := map[string]bool{}
		for _, v := range e.Variants {
			if ids[v.ID] || v.ID == "" {
				return fmt.Errorf("%s %s: variant %q empty or twice", p.ID, e.From, v.ID)
			}
			ids[v.ID] = true
			if err := v.check(p.Kind); err != nil {
				return fmt.Errorf("%s %s %s: %w", p.ID, e.From, v.ID, err)
			}
		}
	}
	return nil
}

func (v Variant) check(kind Kind) error {
	switch {
	case kind == KindSavings && v.Accrual != AccrualDaily && v.Accrual != AccrualMinimum:
		return fmt.Errorf("accrual %q", v.Accrual)
	case kind == KindDeposit && v.TermUnit != TermDays && v.TermUnit != TermMonths:
		return fmt.Errorf("term_unit %q", v.TermUnit)
	case v.Ops != OpsNone && v.Ops != OpsTopUp && v.Ops != OpsTopUpWithdraw:
		return fmt.Errorf("ops %q", v.Ops)
	case len(v.Payouts) == 0:
		return fmt.Errorf("no payouts")
	}
	for _, p := range v.Payouts {
		if !slices.Contains([]Payout{PayoutEnd, PayoutMonthly, PayoutCapitalize, PayoutDaily, PayoutQuarterly}, p) {
			return fmt.Errorf("payout %q", p)
		}
	}
	if err := checkGrid(v.Grid, kind); err != nil {
		return err
	}
	if v.Cap != nil && (v.Cap.Amount <= 0 || (v.Cap.Percent == "") == (v.Cap.Share == "") || !number(v.Cap.Percent+v.Cap.Share)) {
		return fmt.Errorf("cap: an amount and either percent or share")
	}
	for _, s := range v.Schedule {
		if (s.Months > 0) == (s.Days > 0) || (len(s.Grid) > 0) == (s.Plus != "") {
			return fmt.Errorf("period %q: months or days, a grid or plus", s.Name)
		}
		if err := checkGrid(s.Grid, kind); len(s.Grid) > 0 && err != nil {
			return fmt.Errorf("period %q: %w", s.Name, err)
		}
		if _, known := Requirements[s.Requires]; s.Requires != "" && !known {
			return fmt.Errorf("period %q: requires %q", s.Name, s.Requires)
		}
		if s.Plus != "" && !number(s.Plus) {
			return fmt.Errorf("period %q: plus %q", s.Name, s.Plus)
		}
	}
	ids := map[string]bool{}
	for _, b := range v.Bonuses {
		if b.ID == "" || ids[b.ID] {
			return fmt.Errorf("bonus %q empty or twice", b.ID)
		}
		ids[b.ID] = true
		if (b.Plus == "") == (len(b.Levels) == 0) {
			return fmt.Errorf("bonus %s: plus or levels", b.ID)
		}
		if b.Plus != "" && !number(b.Plus) {
			return fmt.Errorf("bonus %s: plus %q", b.ID, b.Plus)
		}
		for i, l := range b.Levels {
			if !number(l.Plus) || (i > 0 && l.From <= b.Levels[i-1].From) {
				return fmt.Errorf("bonus %s: levels go up, each with plus", b.ID)
			}
		}
		if !slices.Contains([]Check{CheckManual, CheckCardSpend, CheckWages, CheckBankBalance}, b.Check) {
			return fmt.Errorf("bonus %s: check %q", b.ID, b.Check)
		}
	}
	if v.Key != nil && !v.Key.NextWorkday && (v.Key.Cutoff < 1 || v.Key.Cutoff > 28 || v.Key.Workday < 1 || v.Key.LateWorkday < v.Key.Workday) {
		return fmt.Errorf("key: next_workday, or cutoff with workday and late_workday")
	}
	return nil
}

func checkGrid(grid []Cell, kind Kind) error {
	if len(grid) == 0 {
		return fmt.Errorf("no grid")
	}
	for _, c := range grid {
		fixed := c.Percent != ""
		tied := c.KeyTimes != "" || c.KeyPlus != ""
		switch {
		case fixed == tied:
			return fmt.Errorf("a cell is fixed (percent) or tied to the key rate (key_times, key_plus)")
		case fixed && (!number(c.Percent) || decimal.RequireFromString(c.Percent).GreaterThanOrEqual(decimal.NewFromInt(100))):
			return fmt.Errorf("percent %q", c.Percent)
		case c.KeyTimes != "" && !number(c.KeyTimes):
			return fmt.Errorf("key_times %q", c.KeyTimes)
		case c.KeyPlus != "" && !number(c.KeyPlus):
			return fmt.Errorf("key_plus %q", c.KeyPlus)
		case c.Floor != "" && !number(c.Floor):
			return fmt.Errorf("floor %q", c.Floor)
		case c.AmountFrom < 0:
			return fmt.Errorf("amount_from %d", c.AmountFrom)
		case kind == KindDeposit && (c.From <= 0 || c.To < c.From):
			return fmt.Errorf("a deposit's cell needs its term, from and to")
		}
	}
	return nil
}

// number says s is a decimal.
func number(s string) bool {
	_, err := decimal.NewFromString(s)
	return err == nil
}
