package taxyear

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/interest"
)

// Income is a deposit's or savings account's income by its rate and its
// share of its person's tax on interest (decision Р-56 Б), this year.
//
// A deposit: Ends and Pays, Term — the whole term's interest, Accrued —
// what it earned to today when it pays at the end, AtEnd — what will be on
// it at the end when the interest stays on it. A savings account: Month —
// this month's interest, on MonthOn, Twelve — the next twelve months'. All
// on the account's money today at today's rate, in its currency; nil when
// not known. YearEnd is what is still to come to 31 December, the same as
// the year's estimate says.
type Income struct {
	// Currency is the account's, of the lines; TaxCurrency of Year and Tax.
	Currency, TaxCurrency string
	YearNo                int
	Rated                 bool
	Ends                  *time.Time
	Pays                  *interest.Pays
	Term                  *int64
	Accrued               *int64
	AtEnd                 *int64
	Month                 *int64
	MonthOn               *time.Time
	Twelve                *int64
	YearEnd               *int64
	// Year is the account's interest of the year, in the tax's currency:
	// paid by the journal, by the rate on the balance marks, still to come;
	// nil when the year's estimate leaves it out (Estimate says why).
	Year     *int64
	Estimate *Estimate
	Tax      *Share
}

// Share is the person's tax on the interest of the year, with what is still
// to come, and the account's part of it: the tax split between the
// person's accounts by their interest of the year (decision Р-56 Б).
// Unestimated are the person's accounts whose interest is not in.
type Share struct {
	Person      string
	Interest    int64
	Free        *int64
	Tax         int64
	Share       int64
	Unestimated []string
}

// IncomeOf is the account's income by its rate and its share of the tax.
func (s *Service) IncomeOf(ctx context.Context, spaceID, accountID uuid.UUID) (Income, error) {
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Income{}, err
	}
	var a account.WithBalance
	found := false
	for _, x := range list {
		if x.ID == accountID {
			a, found = x, true
		}
	}
	if !found || !account.Earns(a.Type) {
		return Income{}, fmt.Errorf("taxyear: a deposit or savings account: %w", pgx.ErrNoRows)
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	out := Income{Currency: a.Currency, YearNo: today.Year()}
	if s.earning == nil {
		return out, nil
	}
	if err := s.lines(ctx, spaceID, a, today, &out); err != nil {
		return Income{}, err
	}
	y, err := s.Of(ctx, spaceID, today.Year())
	if errors.Is(err, ErrNoEstimate) {
		return out, nil
	}
	if err != nil {
		return Income{}, err
	}
	out.TaxCurrency = y.Currency
	for _, p := range y.People {
		for _, acc := range p.Accounts {
			if acc.ID != accountID {
				continue
			}
			out.Estimate = acc.Estimate
			e := acc.Estimate
			if e == nil || e.Missing == "" {
				v := yearOf(acc)
				out.Year = &v
			}
			if e != nil && (e.Missing == "" || e.Missing == MissingRate) && !e.NoRateAhead {
				ahead := e.Ahead
				out.YearEnd = &ahead
			}
			out.Tax = shareOf(p, accountID)
		}
	}
	return out, nil
}

// yearOf is an account's interest of the year: by its journal, filed under
// the mark, by the rate — paid and to come.
func yearOf(a Account) int64 {
	v := a.Facts.Interest + a.Marked
	if e := a.Estimate; e != nil && e.Missing == "" {
		v += e.Paid + e.Ahead
	}
	return v
}

// shareOf is the account's part of the person's tax on interest at the
// year's end: the tax split by the interest of each account — the rest, filed
// under the mark on accounts not listed, keeping its own part — the kopecks
// of rounding to the largest remainders, so that the parts add up to the
// tax. Nil without the year's tax to split.
func shareOf(p Person, accountID uuid.UUID) *Share {
	if p.ByNoticeYearEnd == nil {
		return nil
	}
	out := &Share{Person: p.Name, Interest: p.Interest + p.InterestEstimated + p.InterestAhead, Free: p.InterestFree, Tax: *p.ByNoticeYearEnd}
	type part struct {
		id     uuid.UUID
		weight int64
	}
	var parts []part
	var listed int64
	for _, a := range p.Accounts {
		if !a.Bank {
			continue
		}
		if a.Estimate != nil && a.Estimate.Missing != "" {
			out.Unestimated = append(out.Unestimated, a.Name)
		}
		w := yearOf(a)
		parts = append(parts, part{a.ID, w})
		listed += w
	}
	parts = append(parts, part{uuid.Nil, out.Interest - listed})
	if out.Interest <= 0 || out.Tax == 0 {
		return out
	}
	tax, total := decimal.NewFromInt(out.Tax), decimal.NewFromInt(out.Interest)
	shares := make([]int64, len(parts))
	rests := make([]decimal.Decimal, len(parts))
	given := int64(0)
	for i, x := range parts {
		exact := tax.Mul(decimal.NewFromInt(x.weight)).Div(total)
		shares[i] = exact.Floor().IntPart()
		rests[i] = exact.Sub(exact.Floor())
		given += shares[i]
	}
	for left := out.Tax - given; left > 0; left-- {
		best := 0
		for i := range rests {
			if rests[i].GreaterThan(rests[best]) {
				best = i
			}
		}
		shares[best]++
		rests[best] = decimal.NewFromInt(-1)
	}
	for i, x := range parts {
		if x.id == accountID {
			out.Share = shares[i]
		}
	}
	return out
}

// lines are the account's income by its rate on its money today.
func (s *Service) lines(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, today time.Time, out *Income) error {
	rates, err := interest.Load(ctx, spaceID, today, s.earning, s.earnKeys, s.earnMeasures)
	if err != nil {
		return err
	}
	pays, opened, ok := rates.Pays(a.Account)
	if !ok {
		return nil
	}
	history, err := s.earning.BalanceHistory(ctx, spaceID)
	if err != nil {
		return err
	}
	yearStart := time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	if a.Type == account.TypeSavings {
		month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		to := month.AddDate(1, 0, -1)
		var paid []interest.Payment
		rated := false
		if !account.CountedByJournal(a.Account) {
			// By the balance marks, as the year's estimate reckons it.
			got, unrated, err := interest.Earned(marked(rates, a.ID, history[a.ID]), rates.Of(a.ID), pays, yearStart, to)
			if err != nil {
				return err
			}
			if unrated == nil {
				paid, rated = got, true
			}
		}
		if !rated {
			got, ok, err := s.fromToday(ctx, spaceID, a, rates, pays, yearStart, today, to)
			if err != nil {
				return err
			}
			paid, rated = got, ok
		}
		if !rated {
			return nil
		}
		out.Rated = true
		var twelve int64
		for _, p := range paid {
			if p.On.Before(month) {
				continue
			}
			if p.On.Year() == month.Year() && p.On.Month() == month.Month() {
				v, on := p.Minor, p.On
				out.Month, out.MonthOn = &v, &on
			}
			twelve += p.Minor
		}
		out.Twelve = &twelve
		return nil
	}
	ends := pays.Ends
	out.Ends, out.Pays = &ends, &pays
	// The sum it was opened with, else — kept by its journal — its money
	// today from the opening, else its balance marks.
	var points []interest.Point
	on, amount, known := rates.Opening(a.ID)
	switch {
	case !account.CountedByJournal(a.Account):
		points = marked(rates, a.ID, history[a.ID])
	case known:
		points = []interest.Point{{On: on, Minor: amount}}
	default:
		balance, err := s.balanceOf(ctx, spaceID, a)
		if err != nil {
			return err
		}
		points = []interest.Point{{On: opened, Minor: balance}}
	}
	if len(points) == 0 {
		return nil
	}
	paid, unrated, err := interest.Earned(points, rates.Of(a.ID), pays, opened, ends)
	if err != nil || unrated != nil {
		return err
	}
	out.Rated = true
	var term int64
	for _, p := range paid {
		term += p.Minor
	}
	out.Term = &term
	if !pays.Monthly && today.Before(ends) {
		got, _, err := interest.Earned(points, rates.Of(a.ID), interest.Pays{Ends: today}, opened, today)
		if err != nil {
			return err
		}
		var accrued int64
		for _, p := range got {
			accrued += p.Minor
		}
		out.Accrued = &accrued
	}
	if !pays.Monthly || pays.Capitalize {
		// The last mark on or before the end holds what was paid before it.
		var last interest.Point
		for _, p := range points {
			if !p.On.After(ends) {
				last = p
			}
		}
		atEnd := last.Minor
		for _, p := range paid {
			if !p.On.Before(last.On) {
				atEnd += p.Minor
			}
		}
		out.AtEnd = &atEnd
	}
	return nil
}
