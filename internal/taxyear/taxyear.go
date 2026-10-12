// Package taxyear estimates a family's income tax for a year (decisions
// Р-41, Р-42): per person, what the investment incomes come to, the tax on
// them, what the brokers who are tax agents withhold, what the tax withheld
// abroad offsets, and what is left to pay by return. An estimate, not the
// tax office's figure: it reads the journals the family keeps and says what
// it could not value. This step counts the brokers' accounts; deposits,
// wages and property join next (tax-year plan).
package taxyear

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type facts interface {
	YearFacts(ctx context.Context, spaceID, accountID uuid.UUID, year int, currency string, longYears int) (portfolio.YearFacts, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
	ListMembers(ctx context.Context, spaceID uuid.UUID) ([]family.Member, error)
}

// Service estimates the year's tax.
type Service struct {
	accounts accounts
	facts    facts
	spaces   spaces
}

func NewService(a accounts, f facts, s spaces) *Service {
	return &Service{accounts: a, facts: f, spaces: s}
}

// ErrNoEstimate: the space's country of tax residency has no rules for the
// year's tax in the application.
var ErrNoEstimate = errors.New("taxyear: no estimate for the country")

// Account is one broker's account's part of a person's year.
type Account struct {
	ID            uuid.UUID
	Name          string
	ForeignBroker bool
	Facts         portfolio.YearFacts
	// Securities is the account's result on papers — sales, the gain on
	// pieces held long taken off, and coupons — and Dividends its dividends,
	// as its broker reckons them; AgentTax what a broker who is a tax agent
	// withholds on them for the year, nothing for a broker abroad.
	Securities, Dividends int64
	AgentTax              int64
}

// Person is one person's year.
type Person struct {
	UserID   uuid.UUID
	Name     string
	Accounts []Account
	// Base is the investment incomes summed over the person's accounts:
	// the papers' result — losses on one broker offset gains on another —
	// and the dividends.
	Base int64
	// Tax is on Base by the bands; ByAgents what the brokers who are agents
	// withhold, each on its own account; Withheld the tax the journals show
	// already taken; Credit the tax withheld abroad on dividends that offsets
	// the Russian one; ToPay what is left to pay by return, Refund what the
	// agents took past the tax — a loss on one broker they did not see —
	// that a return gets back.
	Tax, ByAgents, Withheld, Credit, ToPay, Refund int64
	Unvalued                                       int
}

// Year is the family's year.
type Year struct {
	Year     int
	Currency string
	People   []Person
	// DeclareBy and PayBy are the return's and the payment's last days.
	DeclareBy, PayBy time.Time
}

// Of estimates year for the space.
func (s *Service) Of(ctx context.Context, spaceID uuid.UUID, year int) (Year, error) {
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Year{}, err
	}
	rules := sp.CostBasisRules()
	if rules.TaxCurrency == "" {
		return Year{}, ErrNoEstimate
	}
	members, err := s.spaces.ListMembers(ctx, spaceID)
	if err != nil {
		return Year{}, err
	}
	names := map[uuid.UUID]string{}
	var owner uuid.UUID
	for _, m := range members {
		names[m.ID] = m.DisplayName
		if m.Role == family.RoleOwner {
			owner = m.ID
		}
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Year{}, err
	}
	people := map[uuid.UUID]*Person{}
	var order []uuid.UUID
	for _, a := range list {
		if a.Type != account.TypeBrokerage || !account.CountedByJournal(a.Account) {
			continue
		}
		f, err := s.facts.YearFacts(ctx, spaceID, a.ID, year, rules.TaxCurrency, rules.LongHoldingYears)
		if err != nil {
			return Year{}, fmt.Errorf("taxyear: account %s: %w", a.ID, err)
		}
		if f == (portfolio.YearFacts{}) {
			continue
		}
		who := whose(a.Account, owner)
		p, ok := people[who]
		if !ok {
			p = &Person{UserID: who, Name: names[who]}
			people[who] = p
			order = append(order, who)
		}
		acc := Account{ID: a.ID, Name: a.Name, ForeignBroker: a.ForeignBroker, Facts: f}
		acc.Securities = f.Sales - max(f.SalesLong, 0) + f.Coupons
		acc.Dividends = max(f.Dividends, 0)
		if !a.ForeignBroker {
			acc.AgentTax = family.Banded(rules.InvestmentBands[:], max(acc.Securities, 0)+acc.Dividends, rules.TaxUnitMinor)
			p.ByAgents += acc.AgentTax
			p.Withheld += f.Withheld
		} else {
			// What a broker abroad withheld is the source's tax on dividends;
			// it offsets the Russian tax on them, no more.
			p.Credit += min(max(f.Withheld, 0), family.Banded([]family.TaxBand{{BP: rules.InvestmentBands[0].BP}}, acc.Dividends, rules.TaxUnitMinor))
		}
		p.Accounts = append(p.Accounts, acc)
		p.Unvalued += f.Unvalued
	}
	out := Year{
		Year: year, Currency: rules.TaxCurrency, People: []Person{},
		DeclareBy: time.Date(year+1, time.April, 30, 0, 0, 0, 0, time.UTC),
		PayBy:     time.Date(year+1, time.July, 15, 0, 0, 0, 0, time.UTC),
	}
	for _, who := range order {
		p := people[who]
		var securities, dividends int64
		for _, a := range p.Accounts {
			securities += a.Securities
			dividends += a.Dividends
		}
		p.Base = max(securities, 0) + dividends
		p.Tax = family.Banded(rules.InvestmentBands[:], p.Base, rules.TaxUnitMinor)
		due := max(p.Tax-p.Credit, 0)
		p.ToPay, p.Refund = max(due-p.ByAgents, 0), max(p.ByAgents-due, 0)
		out.People = append(out.People, *p)
	}
	slices.SortStableFunc(out.People, func(a, b Person) int {
		switch {
		case a.UserID == owner:
			return -1
		case b.UserID == owner:
			return 1
		}
		return 0
	})
	return out, nil
}

// whose is the person the account's income is for tax: as set, else its
// personal owner, else the space's owner.
func whose(a account.Account, owner uuid.UUID) uuid.UUID {
	switch {
	case a.TaxOwnerUserID != nil:
		return *a.TaxOwnerUserID
	case a.OwnerUserID != nil:
		return *a.OwnerUserID
	}
	return owner
}

// parseYear reads ?year=, this year when absent.
func parseYear(raw string, now time.Time) (int, error) {
	if raw == "" {
		return now.Year(), nil
	}
	y, err := strconv.Atoi(raw)
	if err != nil || y < 2000 || y > now.Year() {
		return 0, fmt.Errorf("%w: the year is a past or this one", family.ErrValidation)
	}
	return y, nil
}
