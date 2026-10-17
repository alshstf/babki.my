// Package taxyear estimates a family's income tax for a year (decisions
// Р-41, Р-42): per person, what the investment incomes come to, the tax on
// them, what the brokers who are tax agents withhold, what the tax withheld
// abroad offsets, and what is left to pay by return. An estimate, not the
// tax office's figure: it reads the journals the family keeps and says what
// it could not value. It counts the brokers' accounts and the interest the
// banks' accounts kept by their operations paid; wages and property join
// next (tax-year plan).
package taxyear

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/interest"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/valuables"
)

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type facts interface {
	YearFacts(ctx context.Context, spaceID, accountID uuid.UUID, year int, currency string, longYears int) (portfolio.YearFacts, error)
}

type keyRates interface {
	HighestKeyRate(ctx context.Context, from, to time.Time) (decimal.Decimal, bool, error)
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
	keys     keyRates
	now      func() time.Time
	// For the earnings under marked categories (WithEarnings).
	flows      flows
	categories categories
	// For the bars and coins sold (WithProperty).
	property soldPieces
	// For the deposits' and savings accounts' interest by their rates
	// (WithInterest).
	earning      earningStore
	earnKeys     interest.KeyRates
	earnMeasures interest.Measures
	cash         cash
	// For the wages written by hand (WithEntries).
	db  db.Executor
	ops operations
	// For the lines of the accounts abroad (WithForeign).
	rows   rows
	papers papers
	// For the report on the accounts abroad (WithReport).
	moves moves
	rates marketdata.RateSource
}

type soldPieces interface {
	SoldIn(ctx context.Context, spaceID uuid.UUID, year int) ([]valuables.SoldPiece, error)
}

// WithProperty lets the estimate count the bars and coins sold in the year
// (decision Р-41: the sale of property).
func (s *Service) WithProperty(p soldPieces) *Service {
	s.property = p
	return s
}

func NewService(a accounts, f facts, s spaces, k keyRates) *Service {
	return &Service{accounts: a, facts: f, spaces: s, keys: k, now: time.Now}
}

// interestFreeBase is what the key rate is applied to for the interest free
// of tax: 1 млн roubles (НК РФ ст. 214.2 п. 1).
const interestFreeBase = 1_000_000_00

// ErrNoEstimate: the space's country of tax residency has no rules for the
// year's tax in the application.
var ErrNoEstimate = errors.New("taxyear: no estimate for the country")

// Account is one account's part of a person's year: a broker's, or a
// bank's kept by its operations, for the interest it paid.
type Account struct {
	ID            uuid.UUID
	Name          string
	Bank          bool
	ForeignBroker bool
	Facts         portfolio.YearFacts
	// Securities is the account's result on papers — sales, the gain on
	// pieces held long taken off, and coupons — and Dividends its dividends,
	// as its broker reckons them; AgentTax what a broker who is a tax agent
	// withholds on them for the year, nothing for a broker abroad.
	Securities, Dividends int64
	AgentTax              int64
	// Estimate is a deposit's or savings account's interest by its rate:
	// what the year paid when it is kept by its balance, what is to come.
	Estimate *Estimate
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
	// Interest is what the banks paid by the journals; InterestEstimated
	// what the accounts kept by their balance paid, by their rates;
	// InterestFree what of the two is free of tax (1 млн × the year's
	// highest key rate), nil without the rate; ByNotice the tax on the rest,
	// which the tax office asks for by notice — part of Tax, not of ToPay.
	// InterestAhead is what the rates bring after today to the year's end,
	// and ByNoticeYearEnd the tax on the interest with it; nil but this year.
	Interest          int64
	InterestEstimated int64
	InterestFree      *int64
	ByNotice          int64
	InterestAhead     int64
	ByNoticeYearEnd   *int64
	// Employers are the wages by employer, as reckoned from what reached the
	// accounts; Wages their gross, WagesTax the tax on it by the wages'
	// scale, WagesWithheld what the employers withheld, each on its own;
	// WagesToPay what is left to pay — when two employers each stayed under
	// a band the sum crosses.
	Employers                                            []Employer
	WagesNet, Wages, WagesTax, WagesWithheld, WagesToPay int64
	// WagePayments are the wage payments one by one, each with its gross
	// and withheld tax written by hand or reckoned.
	WagePayments []WagePayment
	// PropertySold and PropertyGain are what the bars and coins sold in the
	// year brought and gained, those held long enough left out; PropertyBase
	// what of it is taxed — the gain, or the proceeds less the year's
	// deduction when smaller; PropertyFree how many sold free of tax. The
	// base is in Base, its tax paid by return.
	PropertySold, PropertyGain, PropertyBase int64
	PropertyFree                             int
	Unvalued                                 int
}

// Year is the family's year.
type Year struct {
	Year     int
	Currency string
	People   []Person
	// DeclareBy and PayBy are the return's and the payment's last days;
	// NoticeBy the last day to pay the tax on deposits' interest.
	DeclareBy, PayBy, NoticeBy time.Time
	// KeyRate is the year's highest so far, nil when unknown.
	KeyRate *decimal.Decimal
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
	personByID := func(who uuid.UUID) *Person {
		p, ok := people[who]
		if !ok {
			p = &Person{UserID: who, Name: names[who]}
			people[who] = p
			order = append(order, who)
		}
		return p
	}
	personOf := func(a account.Account) *Person { return personByID(whose(a, owner)) }
	byID := map[uuid.UUID]account.Account{}
	for _, a := range list {
		byID[a.ID] = a.Account
	}
	est, err := s.estimates(ctx, spaceID, list, year, rules.TaxCurrency)
	if err != nil {
		return Year{}, err
	}
	for _, a := range list {
		if a.Status != account.StatusActive {
			continue
		}
		bank := a.Type == account.TypeChecking || a.Type == account.TypeSavings || a.Type == account.TypeDeposit
		e, estimated := est[a.ID]
		said := estimated && (e.Paid != 0 || e.Ahead != 0 || e.Missing != "" || e.NoRateAhead)
		if bank && !account.CountedByJournal(a.Account) {
			if said {
				p := personOf(a.Account)
				p.Accounts = append(p.Accounts, Account{ID: a.ID, Name: a.Name, Bank: true, Estimate: &e})
				p.InterestEstimated += e.Paid
				p.InterestAhead += e.Ahead
			}
			continue
		}
		if !account.CountedByJournal(a.Account) || (a.Type != account.TypeBrokerage && !bank) {
			continue
		}
		f, err := s.facts.YearFacts(ctx, spaceID, a.ID, year, rules.TaxCurrency, rules.LongHoldingYears)
		if err != nil {
			return Year{}, fmt.Errorf("taxyear: account %s: %w", a.ID, err)
		}
		if bank {
			if f.Interest == 0 && f.Unvalued == 0 && !said {
				continue
			}
			p := personOf(a.Account)
			acc := Account{ID: a.ID, Name: a.Name, Bank: true, Facts: f}
			if said {
				acc.Estimate = &e
				p.InterestAhead += e.Ahead
			}
			p.Accounts = append(p.Accounts, acc)
			p.Interest += f.Interest
			p.Unvalued += f.Unvalued
			continue
		}
		if f == (portfolio.YearFacts{}) {
			continue
		}
		p := personOf(a.Account)
		acc := Account{ID: a.ID, Name: a.Name, ForeignBroker: a.ForeignBroker, Facts: f}
		acc.Securities = f.Sales - max(f.SalesLong, 0) + f.Coupons
		acc.Dividends = max(f.Dividends, 0)
		if !a.ForeignBroker {
			acc.AgentTax = family.Banded(rules.InvestmentBands[:], max(acc.Securities, 0)+acc.Dividends, rules.TaxUnitMinor)
			p.ByAgents += acc.AgentTax
			p.Withheld += f.Withheld
		} else {
			c, err := s.foreignCredit(ctx, spaceID, a.ID, year, rules, sp.TaxResidency, f, acc.Dividends)
			if err != nil {
				return Year{}, fmt.Errorf("taxyear: account %s: %w", a.ID, err)
			}
			p.Credit += c
		}
		p.Accounts = append(p.Accounts, acc)
		p.Unvalued += f.Unvalued
	}
	// Earnings under marked categories: wages by employer, interest.
	rows, err := s.earnings(ctx, spaceID, year, rules.TaxCurrency, byID, owner)
	if err != nil {
		return Year{}, err
	}
	entered, err := s.Entries(ctx, spaceID)
	if err != nil {
		return Year{}, err
	}
	// Bars and coins sold: the owner of the piece, else the account's person.
	if s.property != nil {
		sold, err := s.property.SoldIn(ctx, spaceID, year)
		if err != nil {
			return Year{}, err
		}
		for _, it := range sold {
			who := owner
			switch {
			case it.OwnerUserID != nil:
				who = *it.OwnerUserID
			case it.AccountOwner != nil:
				who = *it.AccountOwner
			}
			p := personByID(who)
			gain, free, _ := valuables.SaleTax(it.Item, rules)
			if free {
				p.PropertyFree++
				continue
			}
			p.PropertySold += *it.SoldMinor
			p.PropertyGain += gain
		}
	}
	wages := map[uuid.UUID][]earning{}
	for _, r := range rows {
		p := personByID(r.person)
		switch {
		case r.unvalued:
			p.Unvalued++
		case r.mark == category.TaxInterest:
			p.Interest += r.minor
		case r.mark == category.TaxWages:
			wages[r.person] = append(wages[r.person], r)
		}
	}
	out := Year{
		Year: year, Currency: rules.TaxCurrency, People: []Person{},
		DeclareBy: time.Date(year+1, time.April, 30, 0, 0, 0, 0, time.UTC),
		PayBy:     time.Date(year+1, time.July, 15, 0, 0, 0, 0, time.UTC),
		NoticeBy:  time.Date(year+1, time.December, 1, 0, 0, 0, 0, time.UTC),
	}
	// The year's highest key rate so far: for this year it may still rise.
	end := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
	thisYear := false
	if today := s.now().UTC(); today.Before(end) {
		end, thisYear = today, true
	}
	rate, known, err := s.keys.HighestKeyRate(ctx, time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC), end)
	if err != nil {
		return Year{}, err
	}
	if known {
		out.KeyRate = &rate
	}
	for _, who := range order {
		p := people[who]
		var securities, dividends int64
		for _, a := range p.Accounts {
			securities += a.Securities
			dividends += a.Dividends
		}
		invested := max(securities, 0) + dividends
		var taxable, taxableYearEnd int64
		if known {
			free := rate.Mul(decimal.NewFromInt(interestFreeBase)).Div(decimal.NewFromInt(100)).Round(0).IntPart()
			p.InterestFree = &free
			taxable = max(p.Interest+p.InterestEstimated-free, 0)
			taxableYearEnd = max(p.Interest+p.InterestEstimated+p.InterestAhead-free, 0)
		}
		p.PropertyBase = max(min(p.PropertyGain, p.PropertySold-rules.PropertyDeductionMinor), 0)
		invested += p.PropertyBase
		p.Base = invested + taxable
		p.Tax = family.Banded(rules.InvestmentBands[:], p.Base, rules.TaxUnitMinor)
		// The interest's tax is what it adds on top of the rest.
		p.ByNotice = p.Tax - family.Banded(rules.InvestmentBands[:], invested, rules.TaxUnitMinor)
		if thisYear {
			// What the rest of the year's interest adds, the other incomes as
			// they stand today.
			v := family.Banded(rules.InvestmentBands[:], invested+taxableYearEnd, rules.TaxUnitMinor) -
				family.Banded(rules.InvestmentBands[:], invested, rules.TaxUnitMinor)
			p.ByNoticeYearEnd = &v
		}
		due := max(p.Tax-p.ByNotice-p.Credit, 0)
		p.ToPay, p.Refund = max(due-p.ByAgents, 0), max(p.ByAgents-due, 0)
		p.Employers, p.WagePayments = wagesOf(rules.MainBands[:], rules.TaxUnitMinor, wages[who], entered)
		for _, e := range p.Employers {
			p.WagesNet += e.Net
			p.Wages += e.Gross
			p.WagesWithheld += e.Withheld
		}
		p.WagesTax = family.Banded(rules.MainBands[:], p.Wages, rules.TaxUnitMinor)
		p.WagesToPay = max(p.WagesTax-p.WagesWithheld, 0)
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

// foreignCredit is what the tax a broker abroad withheld offsets of the
// Russian one. With the year's lines at hand, payment by payment as the return
// reckons it: no more than the Russian tax on each, and only where the treaty
// with the issuer's country applies on its day (decision Р-50); a tax set
// against no payment offsets nothing. Without them, as before: the withheld,
// no more than the tax on the dividends.
func (s *Service) foreignCredit(ctx context.Context, spaceID, accountID uuid.UUID, year int, rules family.TaxRules, residency string,
	f portfolio.YearFacts, dividends int64,
) (int64, error) {
	if s.rows == nil || s.papers == nil {
		return min(max(f.Withheld, 0), family.Banded([]family.TaxBand{{BP: rules.InvestmentBands[0].BP}}, dividends, rules.TaxUnitMinor)), nil
	}
	book, haveBook, err := family.TreatiesOf(residency)
	if err != nil {
		return 0, err
	}
	r, err := s.rows.YearRows(ctx, spaceID, accountID, year, rules.TaxCurrency, rules.LongHoldingYears)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range r.Payouts {
		if p.Withheld <= 0 {
			continue
		}
		var country string
		if p.Instrument != nil {
			paper, err := s.papers.ByID(ctx, *p.Instrument)
			if err != nil {
				return 0, err
			}
			if len(paper.ISIN) == 12 {
				country = paper.ISIN[:2]
			}
		}
		_, c, _, _ := credit(p, country, rules, book, haveBook)
		total += c
	}
	return total, nil
}
