// Package forecast says what the family's everyday money will come to over
// the coming months (household stage 4, Р-24): the money on cards, current
// and savings accounts and in cash today, and ahead of it the regular payments
// the journal shows (internal/recurring) and the loans' scheduled payments
// (internal/loan), day by day. From it: the lowest point, and how much can be
// spent before the next salary without going below zero. It owns no table.
package forecast

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/recurring"
)

// Kind is where an event comes from.
type Kind string

const (
	KindRegular Kind = "regular"
	KindLoan    Kind = "loan"
)

// Event is one payment ahead. Amount is in Currency, signed as the journal
// signs it; InBase is the same in the base currency at today's rate.
type Event struct {
	On        time.Time
	Name      string
	Kind      Kind
	AccountID uuid.UUID
	Amount    int64
	Currency  string
	InBase    int64
	// Overdue is a regular payment whose day has passed without it: expected
	// today.
	Overdue bool
}

// Day is the money at the end of a day.
type Day struct {
	On      time.Time
	Balance int64
}

// Forecast is the family's everyday money ahead, in the base currency.
type Forecast struct {
	BaseCurrency string
	Days         int
	// Start is the money today; Accounts how many accounts it is on.
	Start    int64
	Accounts int
	Series   []Day
	Events   []Event
	Lowest   Day
	// NextIncome is the next salary — the next regular income of at least
	// half the largest — and FreeUntilIncome the lowest the money gets before
	// it comes, now included: what can be spent until then. Both nil without
	// a regular income in sight.
	NextIncome      *Event
	FreeUntilIncome *int64
	MissingRates    []string
}

// Horizon bounds, in days.
const (
	MinDays     = 7
	MaxDays     = 366
	DefaultDays = 90
)

// spending is what counts as money to live on: not a deposit (locked), not a
// broker's account (investing), not a loan (paid from these).
var spending = map[account.Type]bool{
	account.TypeChecking:   true,
	account.TypeCreditCard: true,
	account.TypeCash:       true,
	account.TypeSavings:    true,
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type positions interface {
	Positions(ctx context.Context, spaceID, accountID uuid.UUID) (apitypes.PositionsResponse, int, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

type regulars interface {
	Find(ctx context.Context, spaceID uuid.UUID) ([]recurring.Payment, error)
}

type loans interface {
	All(ctx context.Context, spaceID uuid.UUID) ([]loan.Terms, error)
}

// Service builds forecasts.
type Service struct {
	accounts  accounts
	positions positions
	spaces    spaces
	regulars  regulars
	loans     loans
	rates     marketdata.RateSource
	now       func() time.Time
}

func NewService(acc accounts, pos positions, sp spaces, reg regulars, l loans, rates marketdata.RateSource) *Service {
	return &Service{accounts: acc, positions: pos, spaces: sp, regulars: reg, loans: l, rates: rates, now: time.Now}
}

// Of is the space's forecast for the given number of days from today.
func (s *Service) Of(ctx context.Context, spaceID uuid.UUID, days int) (Forecast, error) {
	if days < MinDays || days > MaxDays {
		return Forecast{}, fmt.Errorf("%w: days is %d to %d", family.ErrValidation, MinDays, MaxDays)
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Forecast{}, err
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	end := today.AddDate(0, 0, days)
	out := Forecast{BaseCurrency: sp.BaseCurrency, Days: days, MissingRates: []string{}}
	conv := converter{memo: marketdata.NewRateMemo(s.rates), base: sp.BaseCurrency, on: today, missing: map[string]bool{}}

	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Forecast{}, err
	}
	inScope := map[uuid.UUID]bool{}
	// markedOn is the day of the balance an account is counted by: a payment
	// due on or before it is in that balance already, if it came at all.
	markedOn := map[uuid.UUID]time.Time{}
	for _, a := range list {
		if a.Status != account.StatusActive || !spending[a.Type] {
			continue
		}
		inScope[a.ID] = true
		out.Accounts++
		held, byBalance, err := s.money(ctx, spaceID, a)
		if byBalance {
			markedOn[a.ID] = a.Balance.AsOf
		}
		if err != nil {
			return Forecast{}, err
		}
		for currency, minor := range held {
			v, ok, err := conv.inBase(ctx, minor, currency)
			if err != nil {
				return Forecast{}, err
			}
			if ok {
				out.Start += v
			}
		}
	}

	var events []Event
	terms, err := s.loans.All(ctx, spaceID)
	if err != nil {
		return Forecast{}, err
	}
	loanNames := map[string]bool{}
	for _, t := range terms {
		a, ok := accountOf(list, t.AccountID)
		if !ok || a.Status != account.StatusActive {
			continue
		}
		loanNames[fold(a.Name)] = true
		rows, err := loan.Schedule(t)
		if err != nil {
			return Forecast{}, err
		}
		for _, r := range rows {
			if r.On.Before(today) || !r.On.Before(end) {
				continue
			}
			events = append(events, Event{On: r.On, Name: a.Name, Kind: KindLoan, AccountID: a.ID, Amount: -r.Payment, Currency: a.Currency})
		}
	}

	payments, err := s.regulars.Find(ctx, spaceID)
	if err != nil {
		return Forecast{}, err
	}
	for _, p := range payments {
		// Only what moves the money counted here; a loan's interest, written
		// under the loan's name, is already in its schedule; and what the
		// family said is not regular is not.
		if !inScope[p.AccountID] || loanNames[fold(p.Name)] || p.Hidden {
			continue
		}
		mark, byBalance := markedOn[p.AccountID]
		events = append(events, occurrences(p, today, end, byBalance && !mark.Before(p.Next))...)
	}

	for i := range events {
		v, ok, err := conv.inBase(ctx, events[i].Amount, events[i].Currency)
		if err != nil {
			return Forecast{}, err
		}
		if !ok {
			continue
		}
		events[i].InBase = v
		out.Events = append(out.Events, events[i])
	}
	slices.SortStableFunc(out.Events, func(a, b Event) int {
		if c := a.On.Compare(b.On); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	out.Series, out.Lowest, out.NextIncome, out.FreeUntilIncome = lay(out.Start, out.Events, today, days)
	for c := range conv.missing {
		out.MissingRates = append(out.MissingRates, c)
	}
	slices.Sort(out.MissingRates)
	return out, nil
}

// money is what an account holds by currency: its journal's money when the
// total counts it by its journal, else its last balance (byBalance).
func (s *Service) money(ctx context.Context, spaceID uuid.UUID, a account.WithBalance) (held map[string]int64, byBalance bool, err error) {
	if account.CountedByJournal(a.Account) {
		resp, ops, err := s.positions.Positions(ctx, spaceID, a.ID)
		if err != nil {
			return nil, false, err
		}
		if ops > 0 {
			held := map[string]int64{}
			for _, c := range resp.Cash {
				held[c.Currency] += c.AmountMinor
			}
			return held, false, nil
		}
	}
	if a.Balance == nil {
		return nil, false, nil
	}
	return map[string]int64{a.Currency: a.Balance.AmountMinor}, true, nil
}

// occurrences are a regular payment's days from today to end: an overdue one
// today, as still expected — unless inBalance, the balance it is counted by
// being of its day or later — then on its pace.
func occurrences(p recurring.Payment, today, end time.Time, inBalance bool) []Event {
	var out []Event
	ev := func(on time.Time, overdue bool) Event {
		return Event{On: on, Name: p.Name, Kind: KindRegular, AccountID: p.AccountID, Amount: p.Amount, Currency: p.Currency, Overdue: overdue}
	}
	on := p.Next
	if on.Before(today) {
		if !inBalance {
			out = append(out, ev(today, true))
		}
		for on.Before(today) || on.Equal(today) {
			on = p.Cadence.After(on)
		}
	}
	for ; on.Before(end); on = p.Cadence.After(on) {
		out = append(out, ev(on, false))
	}
	return out
}

// lay walks the days from today, the events of each day applied by its end.
func lay(start int64, events []Event, today time.Time, days int) (series []Day, lowest Day, next *Event, free *int64) {
	// The salary: regular income of at least half the largest one.
	var largest int64
	for _, e := range events {
		if e.Kind == KindRegular && e.InBase > largest {
			largest = e.InBase
		}
	}
	for i := range events {
		e := events[i]
		if largest > 0 && e.Kind == KindRegular && e.On.After(today) && 2*e.InBase >= largest {
			next = &e
			break
		}
	}
	// The money as it is now counts too: today's payments, an overdue salary
	// among them, may come later in the day than the spending.
	balance, k := start, 0
	lowest = Day{On: today, Balance: start}
	if next != nil {
		b := start
		free = &b
	}
	series = make([]Day, 0, days)
	for i := range days {
		on := today.AddDate(0, 0, i)
		for k < len(events) && !events[k].On.After(on) {
			balance += events[k].InBase
			k++
		}
		day := Day{On: on, Balance: balance}
		series = append(series, day)
		if balance < lowest.Balance {
			lowest = day
		}
		if next != nil && on.Before(next.On) && balance < *free {
			b := balance
			free = &b
		}
	}
	return series, lowest, next, free
}

func accountOf(list []account.WithBalance, id uuid.UUID) (account.WithBalance, bool) {
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return account.WithBalance{}, false
}

// fold is a name as names are compared: case aside, «ё» as «е».
func fold(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "ё", "е")
}

// converter turns amounts into the base currency at one day's rate and notes
// the currencies it has no rate for.
type converter struct {
	memo    *marketdata.RateMemo
	base    string
	on      time.Time
	missing map[string]bool
}

func (c converter) inBase(ctx context.Context, minor int64, currency string) (int64, bool, error) {
	if currency == c.base || minor == 0 {
		return minor, true, nil
	}
	res := c.memo.Rate(ctx, currency, c.base, c.on)
	if errors.Is(res.Err, marketdata.ErrNoRate) {
		c.missing[currency] = true
		return 0, false, nil
	}
	if res.Err != nil {
		return 0, false, res.Err
	}
	v, err := money.Minor(decimal.NewFromInt(minor).Mul(res.Rate))
	if err != nil {
		return 0, false, fmt.Errorf("forecast: %d %s in %s: %w", minor, currency, c.base, err)
	}
	return v, true, nil
}
