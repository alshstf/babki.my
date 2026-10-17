package taxyear

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/interest"
	"babki.my/babki/internal/platform/apitypes"
)

// The deposits' and savings accounts' interest by their rates (deposit-rates
// plan, step 6): on an account kept by its balance the journal has none of
// it, so what the year paid is reckoned by the rate on the balance marks; on
// every account with a rate, what is still to come to the year's end.

type earningStore interface {
	interest.Store
	BalanceHistory(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]account.BalancePoint, error)
}

type cash interface {
	Positions(ctx context.Context, spaceID, accountID uuid.UUID) (apitypes.PositionsResponse, int, error)
}

// WithInterest estimates the deposits' and savings accounts' interest by
// their rates; c gives the money on the accounts kept by their journal.
func (s *Service) WithInterest(st earningStore, keys interest.KeyRates, ms interest.Measures, c cash) *Service {
	s.earning, s.earnKeys, s.earnMeasures, s.cash = st, keys, ms, c
	return s
}

// Missing is why an account's interest is not estimated.
type Missing string

const (
	// MissingRate: money on a day without a rate (MissingOn).
	MissingRate Missing = "rate"
	// MissingHowPaid: a deposit whose term or way of payment is not known.
	MissingHowPaid Missing = "how_paid"
	// MissingCurrency: interest in a currency other than the tax's.
	MissingCurrency Missing = "currency"
)

// Estimate is an account's interest by its rate. Paid is what the year paid
// to today on an account kept by its balance, From the first balance mark
// when it came after the stretch began — no money is known before it. Ahead
// is what is still to come after today to the year's end. Missing says why
// what the year paid is not estimated; NoRateAhead that what is to come is
// not, for want of today's rate. PaysOn is the term's end of a deposit that
// pays at it after the year: its interest is a later year's.
type Estimate struct {
	Paid        int64
	From        *time.Time
	Ahead       int64
	Missing     Missing
	MissingOn   *time.Time
	NoRateAhead bool
	PaysOn      *time.Time
}

// estimates are the year's estimates of the space's active deposits and
// savings accounts: of every one kept by its balance, and — this year — of
// those kept by their journal for what is to come.
func (s *Service) estimates(ctx context.Context, spaceID uuid.UUID, list []account.WithBalance, year int, currency string) (map[uuid.UUID]Estimate, error) {
	out := map[uuid.UUID]Estimate{}
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if s.earning == nil || today.Before(start) {
		return out, nil
	}
	current := !today.After(end)
	rates, err := interest.Load(ctx, spaceID, today, s.earning, s.earnKeys, s.earnMeasures)
	if err != nil {
		return nil, err
	}
	history, err := s.earning.BalanceHistory(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		byJournal := account.CountedByJournal(a.Account)
		if a.Status != account.StatusActive || !account.Earns(a.Type) || byJournal && !current {
			continue
		}
		var e Estimate
		pays, opened, ok := rates.Pays(a.Account)
		switch {
		case a.Currency != currency:
			e.Missing = MissingCurrency
		case !ok:
			e.Missing = MissingHowPaid
		}
		if e.Missing != "" {
			out[a.ID] = e
			continue
		}
		// A deposit paying at its end earns for it since its opening; one
		// paying monthly, or a savings account, since the year's start.
		from := start
		if !opened.IsZero() && (!pays.Monthly || opened.After(from)) {
			from = opened
		}
		if !byJournal {
			points := marked(rates, a.ID, history[a.ID])
			paid, unrated, err := interest.Earned(points, rates.Of(a.ID), pays, from, end)
			if err != nil {
				return nil, err
			}
			if unrated != nil && !unrated.After(today) {
				e.Missing, e.MissingOn = MissingRate, unrated
			} else {
				for _, p := range paid {
					switch {
					case p.On.Before(start):
					case !p.On.After(today):
						e.Paid += p.Minor
					default:
						e.Ahead += p.Minor
					}
				}
				e.NoRateAhead = unrated != nil
				if len(points) > 0 && points[0].On.After(from) {
					first := points[0].On
					e.From = &first
				}
			}
		}
		if !pays.Monthly && pays.Ends.After(end) {
			ends := pays.Ends
			e.PaysOn = &ends
		}
		if current && (byJournal || e.Missing == MissingRate) {
			paid, ok, err := s.fromToday(ctx, spaceID, a, rates, pays, from, today, end)
			if err != nil {
				return nil, err
			}
			e.Ahead, e.NoRateAhead = 0, !ok
			for _, p := range paid {
				if p.On.After(today) {
					e.Ahead += p.Minor
				}
			}
		}
		out[a.ID] = e
	}
	return out, nil
}

// fromToday is what the account is paid from `from` to `to` on its money
// today — as the money forecast reckons it: this month's days before today
// at today's balance and rate, as the bank pays for the whole month. ok is
// false without today's rate.
func (s *Service) fromToday(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, rates interest.Rates, pays interest.Pays,
	from, today, to time.Time,
) ([]interest.Payment, bool, error) {
	balance, err := s.balanceOf(ctx, spaceID, a)
	if err != nil {
		return nil, false, err
	}
	if pays.Monthly {
		if month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC); month.After(from) {
			from = month
		}
	}
	rate := rates.Of(a.ID)
	asToday := func(day time.Time, balance int64) (decimal.Decimal, bool, error) {
		if day.Before(today) {
			day = today
		}
		return rate(day, balance)
	}
	paid, unrated, err := interest.Earned([]interest.Point{{On: from, Minor: balance}}, asToday, pays, from, to)
	if err != nil || unrated != nil {
		return nil, unrated == nil, err
	}
	return paid, true, nil
}

// balanceOf is the money on the account today: by its journal, or its last
// balance mark.
func (s *Service) balanceOf(ctx context.Context, spaceID uuid.UUID, a account.WithBalance) (int64, error) {
	if !account.CountedByJournal(a.Account) {
		if a.Balance == nil {
			return 0, nil
		}
		return a.Balance.AmountMinor, nil
	}
	if s.cash == nil {
		return 0, nil
	}
	resp, _, err := s.cash.Positions(ctx, spaceID, a.ID)
	if err != nil {
		return 0, err
	}
	var balance int64
	for _, c := range resp.Cash {
		if c.Currency == a.Currency {
			balance += c.AmountMinor
		}
	}
	return balance, nil
}

// marked are the balance marks of an account kept by its balance, with the
// sum a deposit was opened with until the first.
func marked(rates interest.Rates, id uuid.UUID, history []account.BalancePoint) []interest.Point {
	points := pointsOf(history)
	if on, amount, ok := rates.Opening(id); ok && (len(points) == 0 || points[0].On.After(on)) {
		points = append([]interest.Point{{On: on, Minor: amount}}, points...)
	}
	return points
}

func pointsOf(marks []account.BalancePoint) []interest.Point {
	out := make([]interest.Point, 0, len(marks))
	for _, m := range marks {
		day := time.Date(m.AsOf.Year(), m.AsOf.Month(), m.AsOf.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, interest.Point{On: day, Minor: m.AmountMinor})
	}
	return out
}

// lowRate are the active savings accounts and deposits in the tax's currency
// whose rate is known for every day of the year and never above 1 % a year:
// their interest is not in the base (НК РФ ст. 214.2 п. 1). Known from the
// year's first day — or a programme's day of opening, when later — so that a
// stretch with no rate written in never counts as low.
func (s *Service) lowRate(ctx context.Context, spaceID uuid.UUID, list []account.WithBalance, year int, currency string) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if s.earning == nil {
		return out, nil
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	rates, err := interest.Load(ctx, spaceID, today, s.earning, s.earnKeys, s.earnMeasures)
	if err != nil {
		return nil, err
	}
	one := decimal.NewFromInt(1)
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
	for _, a := range list {
		if a.Status != account.StatusActive || !account.Earns(a.Type) || a.Currency != currency {
			continue
		}
		from := start
		if on, ok := rates.OpenedOn(a.ID); ok && on.After(from) {
			from = on
		}
		balance, err := s.balanceOf(ctx, spaceID, a)
		if err != nil {
			return nil, err
		}
		low := !from.After(end)
		for day := from; low && !day.After(end); day = day.AddDate(0, 0, 1) {
			r, ok, err := rates.On(a.ID, day, max(balance, 1))
			if err != nil {
				return nil, err
			}
			low = ok && !r.Percent.GreaterThan(one)
		}
		if low {
			out[a.ID] = true
		}
	}
	return out, nil
}
