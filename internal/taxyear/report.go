package taxyear

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
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/portfolio"
)

type moves interface {
	YearMoves(ctx context.Context, spaceID, accountID uuid.UUID, year int) (portfolio.YearMoves, error)
}

// WithReport lets the service lay out the yearly report on the money's moves
// of each account abroad (decision Р-45, step 3): moves are the accounts'
// years, rates price what came in at the year's last rate.
func (s *Service) WithReport(m moves, rates marketdata.RateSource) *Service {
	s.moves, s.rates = m, rates
	return s
}

// Whether an account needs the report.
const (
	// ReportFree: the broker is in a country the rules free, and what came in
	// is within the limit.
	ReportFree = "free"
	// ReportFreeIfListed: within the limit, the broker elsewhere — free if its
	// country exchanges account data automatically (the tax office's list).
	ReportFreeIfListed = "free_if_listed"
	// ReportDue: past the limit; the report is due.
	ReportDue = "due"
	// ReportUnknown: a rate is missing; the sum cannot be told.
	ReportUnknown = "unknown"
)

// Report is the year's report on each account at a broker abroad: the money
// by currency and the papers' worth, as the form КНД 1112520 asks them.
type Report struct {
	Year     int
	Currency string
	// FreeUpTo is the limit of what came in, in Currency; 0: the residency's
	// rules know no such report.
	FreeUpTo int64
	DueBy    time.Time
	Accounts []ReportAccount
}

// ReportAccount is one account's report. In is what came in over the year at
// the Central Bank's rates of 31 December, in the report's currency — with
// nothing in, the money at the year's end; Verdict says whether the report is
// due.
type ReportAccount struct {
	ID        uuid.UUID
	Name      string
	Person    uuid.UUID
	Country   string
	Moves     portfolio.YearMoves
	In        int64
	NothingIn bool
	Verdict   string
}

// ReportOf lays out year's report for the space's accounts at brokers abroad.
func (s *Service) ReportOf(ctx context.Context, spaceID uuid.UUID, year int) (Report, error) {
	if s.moves == nil || s.rates == nil {
		return Report{}, errors.New("taxyear: the report is not set up")
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	rules := sp.CostBasisRules()
	if rules.TaxCurrency == "" || rules.ReportFreeUpToMinor == 0 {
		return Report{}, ErrNoEstimate
	}
	members, err := s.spaces.ListMembers(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	var owner uuid.UUID
	for _, m := range members {
		if m.Role == family.RoleOwner {
			owner = m.ID
		}
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	out := Report{
		Year: year, Currency: rules.TaxCurrency, FreeUpTo: rules.ReportFreeUpToMinor,
		DueBy: time.Date(year+1, time.June, 1, 0, 0, 0, 0, time.UTC), Accounts: []ReportAccount{},
	}
	memo := marketdata.NewRateMemo(s.rates)
	lastDay := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
	for _, a := range list {
		if a.Status != account.StatusActive || a.Type != account.TypeBrokerage || !a.ForeignBroker {
			continue
		}
		m, err := s.moves.YearMoves(ctx, spaceID, a.ID, year)
		if err != nil {
			return Report{}, fmt.Errorf("taxyear: account %s: %w", a.ID, err)
		}
		if len(m.Cash) == 0 && len(m.PapersStart) == 0 && len(m.PapersEnd) == 0 {
			continue
		}
		ra := ReportAccount{ID: a.ID, Name: a.Name, Person: whose(a.Account, owner), Moves: m}
		if i, _, ok := account.IntermediaryOf(a.Account); ok {
			ra.Country = i.Country
		}
		var in, end decimal.Decimal
		known := true
		for _, c := range m.Cash {
			r := decimal.NewFromInt(1)
			if c.Currency != rules.TaxCurrency {
				got := memo.Rate(ctx, c.Currency, rules.TaxCurrency, lastDay)
				if errors.Is(got.Err, marketdata.ErrNoRate) {
					known = false
					continue
				}
				if got.Err != nil {
					return Report{}, got.Err
				}
				r = got.Rate
			}
			in = in.Add(decimal.NewFromInt(c.In).Mul(r))
			end = end.Add(decimal.NewFromInt(c.End).Mul(r))
		}
		ra.NothingIn = in.IsZero()
		if ra.NothingIn {
			in = end
		}
		ra.In = in.Round(0).IntPart()
		switch {
		case !known:
			ra.Verdict = ReportUnknown
		case ra.In > rules.ReportFreeUpToMinor:
			ra.Verdict = ReportDue
		case ra.Country != "" && slices.Contains(rules.ReportFreeIn[:], ra.Country):
			ra.Verdict = ReportFree
		default:
			ra.Verdict = ReportFreeIfListed
		}
		out.Accounts = append(out.Accounts, ra)
	}
	slices.SortStableFunc(out.Accounts, func(a, b ReportAccount) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
