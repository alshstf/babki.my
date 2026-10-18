package compare

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/intermediary"
	"babki.my/babki/internal/structure"
	"babki.my/babki/internal/taxyear"
)

// State is how a condition is taken: as the family's data tell it today,
// met, or not met.
type State string

const (
	StateNow State = "now"
	StateYes State = "yes"
	StateNo  State = "no"
)

// Choice is a condition's state; Level the step of a stepped bonus when met,
// -1 its highest.
type Choice struct {
	State State
	Level int
}

// CondKey names a condition of a programme's variant.
type CondKey struct{ Programme, Variant, Condition string }

// Scenario is the key rate over the horizon: today's all along, a rate the
// family sets, or the Bank of Russia's forecast — the middle of each year's
// range.
type Scenario struct {
	Custom   bool
	Forecast bool
	Percent  decimal.Decimal
}

// YearKey is the scenario's key rate in a year: the highest of it, for the
// year's interest free of tax.
type YearKey struct {
	Year    int
	Percent decimal.Decimal
}

// Ask is what to compare: the sum in roubles over Months from today, for the
// tax and insurance of Person; the key rate's scenario; the conditions —
// Preset for each, Choices over it one by one; the groups Person is in, for
// the variants open only to them.
type Ask struct {
	Amount    int64
	Months    int
	Person    uuid.UUID
	Key       Scenario
	Preset    State
	Choices   map[CondKey]Choice
	Audiences []deposit.Audience
}

// Told is what the family's data say of a condition today: Known false when
// they cannot tell — a condition only a mark tells, or no data that month.
type Told struct {
	Known bool
	Met   bool
	Level int
	Value int64
	Month time.Time
}

// Condition is a variant's condition as reckoned: what the data say and the
// state it was taken at.
type Condition struct {
	ID          string
	Name        string
	Requirement bool
	Check       deposit.Check
	Group       string
	Plus        string
	Levels      []deposit.Level
	Now         Told
	Used        Choice
	Met         bool
	Level       int
}

// Row is a variant on the sum over the horizon: what it earns, a year
// compounded, the tax it adds to the person's, what is left, and what of
// the sum deposit insurance covers with the person's money in the bank.
type Row struct {
	Programme  deposit.Programme
	BankName   string
	Variant    deposit.Variant
	Earning    Earning
	Yearly     decimal.Decimal
	Tax        *int64
	Net        int64
	Insured    *int64
	Conditions []Condition
}

// Result is the comparison: the rows by what is left after the tax, the key
// rate taken, the horizon, and how many variants were left out — another
// currency, a sum the bank has no rate for, a day without a rate.
type Result struct {
	Rows       []Row
	KeyPercent decimal.Decimal
	KeyPath    []YearKey
	Forecast   *deposit.KeyForecast
	Start, End time.Time
	Person     string
	Skipped    int
}

type keyRates interface {
	KeyRateOn(ctx context.Context, day time.Time) (decimal.Decimal, bool, error)
}

type measures interface {
	MeasuresFor(ctx context.Context, spaceID uuid.UUID) (account.Measure, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type structures interface {
	Of(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation) (structure.Structure, error)
}

type taxes interface {
	Of(ctx context.Context, spaceID uuid.UUID, year int) (taxyear.Year, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

type members interface {
	ListMembers(ctx context.Context, spaceID uuid.UUID) ([]family.Member, error)
}

// Service compares the catalog's programmes.
type Service struct {
	keys       keyRates
	measures   measures
	accounts   accounts
	structures structures
	taxes      taxes
	spaces     spaces
	members    members
	now        func() time.Time
}

func NewService(k keyRates, m measures, a accounts, st structures, t taxes, sp spaces, mb members) *Service {
	return &Service{keys: k, measures: m, accounts: a, structures: st, taxes: t, spaces: sp, members: mb, now: time.Now}
}

// The bounds of an ask.
const (
	MaxMonths  = 60
	maxAmount  = 1_000_000_000_00
	maxPercent = 100
)

var errAsk = fmt.Errorf("%w: a sum from 1 kopeck to 1 billion roubles, 1 to %d months, the key rate from 0 to %d %%", family.ErrValidation, MaxMonths, maxPercent)

// Compare is the comparison the ask sets.
func (s *Service) Compare(ctx context.Context, spaceID uuid.UUID, ask Ask) (Result, error) {
	if ask.Amount <= 0 || ask.Amount > maxAmount || ask.Months < 1 || ask.Months > MaxMonths ||
		ask.Key.Custom && (ask.Key.Percent.IsNegative() || ask.Key.Percent.GreaterThan(decimal.NewFromInt(maxPercent))) {
		return Result{}, errAsk
	}
	if ask.Preset == "" {
		ask.Preset = StateNow
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	out := Result{Start: today, End: today.AddDate(0, ask.Months, 0)}
	key := ask.Key.Percent
	if !ask.Key.Custom {
		k, ok, err := s.keys.KeyRateOn(ctx, today)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, fmt.Errorf("%w: today's key rate is not known — set one", family.ErrValidation)
		}
		key = k
	}
	out.KeyPercent = key
	keyOn := func(time.Time) (decimal.Decimal, bool, error) { return key, true, nil }
	if ask.Key.Forecast {
		f, err := deposit.Forecast()
		if err != nil {
			return Result{}, err
		}
		out.Forecast = &f
		keyOn = func(day time.Time) (decimal.Decimal, bool, error) {
			if k, ok := f.On(day); ok {
				return k, true, nil
			}
			return key, true, nil
		}
	}
	// The year's highest key rate as the scenario has it — its rates change
	// only at a year's start or, this year, after today.
	yearKey := func(year int) decimal.Decimal {
		first, _, _ := keyOn(time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC))
		last, _, _ := keyOn(time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC))
		k := decimal.Max(first, last)
		if year == today.Year() {
			k = decimal.Max(k, key)
		}
		return k
	}
	for y := today.Year(); y <= out.End.AddDate(0, 0, overshoot).Year(); y++ {
		out.KeyPath = append(out.KeyPath, YearKey{Year: y, Percent: yearKey(y)})
	}

	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Result{}, err
	}
	var measure account.Measure
	if s.measures != nil {
		if measure, err = s.measures.MeasuresFor(ctx, spaceID); err != nil {
			return Result{}, err
		}
	}
	banks, err := s.banksOf(ctx, spaceID)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.taxOf(ctx, spaceID, ask.Person, today, yearKey, sp.CostBasisRules())
	if err != nil {
		return Result{}, err
	}
	out.Person = tx.person
	held, limit, err := s.heldOf(ctx, spaceID, ask.Person)
	if err != nil {
		return Result{}, err
	}
	catalog, err := deposit.Catalog()
	if err != nil {
		return Result{}, err
	}
	names, err := intermediary.Load()
	if err != nil {
		return Result{}, err
	}
	lastMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	for _, p := range catalog {
		e, ok := p.EditionOn(today)
		if !ok {
			continue
		}
		for _, v := range e.Variants {
			if !open(v.Audience, ask.Audiences) {
				continue
			}
			if v.Money() != "RUB" {
				out.Skipped++
				continue
			}
			conds := conditionsOf(v)
			for i := range conds {
				c := &conds[i]
				c.Now = told(*c, p.Bank, banks, measure, lastMonth)
				choice, ok := ask.Choices[CondKey{p.ID, v.ID, c.ID}]
				if !ok {
					choice = Choice{State: ask.Preset, Level: -1}
				}
				c.Used = choice
				c.Met, c.Level = resolve(*c, choice)
			}
			met := func(id string, _ time.Time) (int, bool) {
				for _, c := range conds {
					if c.ID == id {
						return c.Level, c.Met
					}
				}
				return 0, false
			}
			earning, err := earn(p, v, ask.Amount, out.Start, out.End, keyOn, met)
			if errors.Is(err, errNoRate) {
				out.Skipped++
				continue
			}
			if err != nil {
				return Result{}, fmt.Errorf("compare: %s %s: %w", p.ID, v.ID, err)
			}
			row := Row{Programme: p, Variant: v, Earning: earning, Conditions: conds, Yearly: yearly(ask.Amount, earning)}
			if b, ok := names.ByID(p.Bank); ok {
				row.BankName = b.Name
			}
			row.Net = earning.Income
			if t, ok := tx.added(earning.Payments); ok {
				row.Tax = &t
				row.Net -= t
			}
			if limit > 0 {
				insured := min(max(limit-held[p.Bank], 0), ask.Amount)
				row.Insured = &insured
			}
			out.Rows = append(out.Rows, row)
		}
	}
	slices.SortStableFunc(out.Rows, func(a, b Row) int {
		if c := cmp.Compare(b.Net, a.Net); c != 0 {
			return c
		}
		return b.Yearly.Cmp(a.Yearly)
	})
	return out, nil
}

// open says the variant is open to someone of the groups: to anyone, or to
// one in every group it lists; never one no longer opened.
func open(of, groups []deposit.Audience) bool {
	for _, a := range of {
		switch {
		case a == deposit.AudienceAll:
		case a == deposit.AudienceClosed || !slices.Contains(groups, a):
			return false
		}
	}
	return true
}

// conditionsOf are the variant's conditions: its periods' requirements and
// its bonuses.
func conditionsOf(v deposit.Variant) []Condition {
	var out []Condition
	seen := map[string]bool{}
	for _, s := range v.Schedule {
		if s.Requires == "" || seen[s.Requires] {
			continue
		}
		seen[s.Requires] = true
		out = append(out, Condition{ID: s.Requires, Name: deposit.Requirements[s.Requires], Requirement: true, Check: deposit.CheckManual})
	}
	for _, b := range v.Bonuses {
		out = append(out, Condition{ID: b.ID, Name: b.Name, Check: b.Check, Group: b.Group, Plus: b.Plus, Levels: b.Levels})
	}
	return out
}

// newClient is the requirement of being new to the bank.
const newClient = "new_client"

// told is what the family's data say of the condition today: a new client —
// no account of the family in the bank; a bonus the program measures — the
// last month finished, as the bank pays; a condition only a mark tells is not
// known.
func told(c Condition, bank string, banks map[string]bool, measure account.Measure, month time.Time) Told {
	if c.Requirement {
		if c.ID == newClient {
			return Told{Known: true, Met: !banks[bank]}
		}
		return Told{}
	}
	if c.Check == deposit.CheckManual || measure == nil {
		return Told{}
	}
	value, known := measure(bank, c.Check, month)
	if !known {
		return Told{Month: month}
	}
	out := Told{Known: true, Value: value, Month: month}
	if len(c.Levels) == 0 {
		out.Met = value > 0
		return out
	}
	for i, l := range c.Levels {
		if value >= l.From {
			out.Met, out.Level = true, i
		}
	}
	return out
}

// resolve is whether the condition is met, and at which step, by the choice:
// as told — not met when the data cannot tell; met at the step chosen or the
// highest; not met.
func resolve(c Condition, choice Choice) (bool, int) {
	switch choice.State {
	case StateYes:
		level := len(c.Levels) - 1
		if choice.Level >= 0 && choice.Level < len(c.Levels) {
			level = choice.Level
		}
		return true, max(level, 0)
	case StateNo:
		return false, 0
	}
	return c.Now.Known && c.Now.Met, c.Now.Level
}

// yearly is the earning a year, compounded, in percent: (1 + income /
// amount) ^ (365 / days) − 1.
func yearly(amount int64, e Earning) decimal.Decimal {
	if amount <= 0 || e.Days <= 0 {
		return decimal.Zero
	}
	growth := decimal.NewFromInt(amount + e.Income).Div(decimal.NewFromInt(amount))
	power, err := growth.PowWithPrecision(decimal.NewFromInt(365).Div(decimal.NewFromInt(int64(e.Days))), 16)
	if err != nil {
		return decimal.Zero
	}
	return power.Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(100)).Round(2)
}

// banksOf are the banks the family holds an active account in.
func (s *Service) banksOf(ctx context.Context, spaceID uuid.UUID) (map[string]bool, error) {
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range list {
		if a.Status != account.StatusActive {
			continue
		}
		if i, _, ok := account.IntermediaryOf(a.Account); ok {
			out[i.ID] = true
		}
	}
	return out, nil
}

// heldOf is what the person holds in each bank against deposit insurance,
// and the limit; no limit when the country has none.
func (s *Service) heldOf(ctx context.Context, spaceID, person uuid.UUID) (map[string]int64, int64, error) {
	st, err := s.structures.Of(ctx, spaceID, structure.Liquid)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]int64{}
	for _, r := range st.Insurance {
		if r.Person == person {
			out[r.Bank] += r.Held
		}
	}
	return out, st.InsuredLimit, nil
}
