// Package payouts forecasts what the family's papers will pay (#399): bond
// coupons, repayments and redemptions from the exchange's schedules, and the
// dividends issuers declared, for the holdings the journals leave today. It
// reads through the journal, the accounts and market data, and owns no table.
//
// A forecast is today's holdings carried forward: a sale or a purchase before
// a payment's day changes what it will be. Amounts are before tax; the base
// currency figure is at today's rate, as no later one is known.
package payouts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// Kind is what a payout is.
type Kind string

const (
	KindCoupon       Kind = "coupon"
	KindAmortization Kind = "amortization"
	KindRedemption   Kind = "redemption"
	KindOffer        Kind = "offer"
	KindDividend     Kind = "dividend"
)

// MaxMonths is the furthest a forecast looks.
const MaxMonths = 24

// Event is one payout to one account. PerUnit and Amount are in Currency; nil
// when not known yet (a floating coupon before its rate is set) or when the
// event pays nothing by itself (an offer). InBase is Amount at today's rate.
type Event struct {
	On           time.Time
	RecordOn     *time.Time
	Kind         Kind
	InstrumentID uuid.UUID
	AccountID    uuid.UUID
	Quantity     decimal.Decimal
	PerUnit      *decimal.Decimal
	Amount       *int64
	Currency     string
	InBase       *int64
}

// Forecast is the payouts between From and To, earliest first, with what each
// month of it comes to in the base currency.
type Forecast struct {
	BaseCurrency string
	From, To     time.Time
	Months       []time.Time
	ByMonth      []int64
	Total        int64
	Events       []Event
	// MissingRates names the currencies left out of the base figures.
	MissingRates []string
}

type journals interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type schedules interface {
	BondEventsBetween(ctx context.Context, ids []uuid.UUID, from, to time.Time) (map[uuid.UUID][]marketdata.BondEvent, error)
	DividendsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]marketdata.Dividend, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Service builds forecasts.
type Service struct {
	journal   journals
	accounts  accounts
	schedules schedules
	spaces    spaces
	rates     marketdata.RateSource
	now       func() time.Time
}

func NewService(j journals, acc accounts, sch schedules, sp spaces, rates marketdata.RateSource) *Service {
	return &Service{journal: j, accounts: acc, schedules: sch, spaces: sp, rates: rates, now: time.Now}
}

// holding is a paper an account holds today.
type holding struct {
	account  uuid.UUID
	quantity decimal.Decimal
}

// Forecast is the payouts of the next months — of one account, or of every
// active account of the space when accountID is nil.
func (s *Service) Forecast(ctx context.Context, spaceID uuid.UUID, months int, accountID *uuid.UUID) (Forecast, error) {
	if months < 1 || months > MaxMonths {
		return Forecast{}, fmt.Errorf("%w: a forecast looks 1 to %d months ahead", family.ErrValidation, MaxMonths)
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	f := Forecast{From: today, To: first.AddDate(0, months, -1), MissingRates: []string{}}
	for m := first; !m.After(f.To); m = m.AddDate(0, 1, 0) {
		f.Months = append(f.Months, m)
	}
	f.ByMonth = make([]int64, len(f.Months))
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Forecast{}, err
	}
	f.BaseCurrency = sp.BaseCurrency

	held, err := s.holdings(ctx, spaceID, accountID)
	if err != nil {
		return Forecast{}, err
	}
	ids := make([]uuid.UUID, 0, len(held))
	for id := range held {
		ids = append(ids, id)
	}
	bonds, err := s.schedules.BondEventsBetween(ctx, ids, f.From, f.To)
	if err != nil {
		return Forecast{}, err
	}
	dividends, err := s.schedules.DividendsOf(ctx, ids)
	if err != nil {
		return Forecast{}, err
	}

	for _, id := range ids {
		for _, e := range bonds[id] {
			for _, h := range held[id] {
				ev := Event{
					On: e.On, RecordOn: e.RecordOn, Kind: Kind(e.Kind), InstrumentID: id, AccountID: h.account,
					Quantity: h.quantity, Currency: e.Currency,
				}
				if e.Kind != marketdata.BondOffer {
					ev.PerUnit = e.Value
				}
				f.Events = append(f.Events, ev)
			}
		}
		for _, d := range declared(dividends[id], f.From, f.To) {
			per := d.PerShare
			on := d.RecordDate
			if d.PaymentDate != nil {
				on = *d.PaymentDate
			}
			record := d.RecordDate
			for _, h := range held[id] {
				f.Events = append(f.Events, Event{
					On: on, RecordOn: &record, Kind: KindDividend, InstrumentID: id,
					AccountID: h.account, Quantity: h.quantity, PerUnit: &per, Currency: d.Currency,
				})
			}
		}
	}
	slices.SortStableFunc(f.Events, func(a, b Event) int { return a.On.Compare(b.On) })
	if err := s.price(ctx, &f, today); err != nil {
		return Forecast{}, err
	}
	return f, nil
}

// holdings is, per paper, the accounts holding it today and how many.
func (s *Service) holdings(ctx context.Context, spaceID uuid.UUID, accountID *uuid.UUID) (map[uuid.UUID][]holding, error) {
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]holding{}
	for _, a := range list {
		if a.Status != account.StatusActive || (accountID != nil && a.ID != *accountID) {
			continue
		}
		ops, err := s.journal.ListForEngine(ctx, spaceID, a.ID)
		if err != nil {
			return nil, err
		}
		if len(ops) == 0 {
			continue
		}
		positions, err := portfolio.Compute(ops)
		if err != nil {
			// A journal that does not replay forecasts nothing rather than fail
			// the family's whole calendar; its screen says what is wrong.
			continue
		}
		for id, p := range positions {
			if p.Quantity.IsPositive() {
				out[id] = append(out[id], holding{account: a.ID, quantity: p.Quantity})
			}
		}
	}
	return out, nil
}

// declared is the dividends paid between from and to, one per record date —
// a broker's and a feed's copy of one dividend are one payout.
func declared(all []marketdata.Dividend, from, to time.Time) []marketdata.Dividend {
	seen := map[time.Time]bool{}
	var out []marketdata.Dividend
	for _, d := range all {
		on := d.RecordDate
		if d.PaymentDate != nil {
			on = *d.PaymentDate
		}
		if on.Before(from) || on.After(to) || seen[d.RecordDate] {
			continue
		}
		seen[d.RecordDate] = true
		out = append(out, d)
	}
	return out
}

// price works out each event's amount and its base currency figure at
// today's rate, and what each month comes to.
func (s *Service) price(ctx context.Context, f *Forecast, today time.Time) error {
	memo := marketdata.NewRateMemo(s.rates)
	missing := map[string]bool{}
	for i := range f.Events {
		e := &f.Events[i]
		if e.PerUnit == nil {
			continue
		}
		amount, err := money.Minor(e.PerUnit.Mul(e.Quantity).Shift(2))
		if err != nil {
			return fmt.Errorf("payouts: %s of %s: %w", e.Kind, e.InstrumentID, err)
		}
		e.Amount = &amount
		inBase := amount
		if e.Currency != f.BaseCurrency {
			res := memo.Rate(ctx, e.Currency, f.BaseCurrency, today)
			if errors.Is(res.Err, marketdata.ErrNoRate) {
				missing[e.Currency] = true
				continue
			}
			if res.Err != nil {
				return res.Err
			}
			if inBase, err = money.Minor(decimal.NewFromInt(amount).Mul(res.Rate)); err != nil {
				return fmt.Errorf("payouts: %s of %s in %s: %w", e.Kind, e.InstrumentID, f.BaseCurrency, err)
			}
		}
		e.InBase = &inBase
		month := (e.On.Year()-f.Months[0].Year())*12 + int(e.On.Month()) - int(f.Months[0].Month())
		if month >= 0 && month < len(f.ByMonth) {
			f.ByMonth[month] += inBase
			f.Total += inBase
		}
	}
	for c := range missing {
		f.MissingRates = append(f.MissingRates, c)
	}
	slices.Sort(f.MissingRates)
	return nil
}
