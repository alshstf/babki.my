package interest

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/deposit"
)

// Store is where the accounts' rates, programmes and marks are kept.
type Store interface {
	AllRates(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]account.Rate, error)
	AllBindings(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]account.Binding, error)
	AllMarks(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]account.Mark, error)
}

// KeyRates strike a rate tied to the key rate.
type KeyRates interface {
	KeyRateOn(ctx context.Context, day time.Time) (decimal.Decimal, bool, error)
}

// Measures tell the conditions of the accounts kept in the automatic mode.
type Measures interface {
	MeasuresFor(ctx context.Context, spaceID uuid.UUID) (account.Measure, error)
}

// Rates are the space's accounts' rates: the one place the goals, the
// forecast and the year's tax take them from, so that they never differ.
type Rates struct {
	sources map[uuid.UUID]account.RateSource
	keyOn   func(time.Time) (decimal.Decimal, bool, error)
	today   time.Time
}

// Load reads the space's rates, their conditions told as of today. Without
// keys a rate tied to the key rate is not known; without measures a
// condition told by the program is not met.
func Load(ctx context.Context, spaceID uuid.UUID, today time.Time, st Store, keys KeyRates, ms Measures) (Rates, error) {
	rates, err := st.AllRates(ctx, spaceID)
	if err != nil {
		return Rates{}, err
	}
	bindings, err := st.AllBindings(ctx, spaceID)
	if err != nil {
		return Rates{}, err
	}
	marks, err := st.AllMarks(ctx, spaceID)
	if err != nil {
		return Rates{}, err
	}
	var measure account.Measure
	for _, b := range bindings {
		if b.Conditions == account.ConditionsAuto && ms != nil {
			if measure, err = ms.MeasuresFor(ctx, spaceID); err != nil {
				return Rates{}, err
			}
			break
		}
	}
	// Earned asks the rate of every day: the key rate of each is read once.
	// The day is kept as text — a date read back from the base is midnight
	// UTC, not the day asked.
	type known struct {
		rate decimal.Decimal
		ok   bool
	}
	memo := map[string]known{}
	out := Rates{sources: map[uuid.UUID]account.RateSource{}, today: today, keyOn: func(day time.Time) (decimal.Decimal, bool, error) {
		if keys == nil {
			return decimal.Zero, false, nil
		}
		k := day.Format(time.DateOnly)
		if v, ok := memo[k]; ok {
			return v.rate, v.ok, nil
		}
		rate, ok, err := keys.KeyRateOn(ctx, day)
		if err != nil {
			return decimal.Zero, false, err
		}
		memo[k] = known{rate, ok}
		return rate, ok, nil
	}}
	add := func(id uuid.UUID) {
		if _, ok := out.sources[id]; ok {
			return
		}
		src := account.RateSource{Rates: rates[id], Marks: marks[id], Measure: measure}
		if b, ok := bindings[id]; ok {
			src.Binding = &b
		}
		out.sources[id] = src
	}
	for id := range rates {
		add(id)
	}
	for id := range bindings {
		add(id)
	}
	return out, nil
}

// On is the account's rate on day with balance on it.
func (r Rates) On(id uuid.UUID, day time.Time, balance int64) (account.Resolved, bool, error) {
	return account.Resolve(r.sources[id], day, r.today, balance, r.keyOn)
}

// Of is the account's rate as Earned takes it.
func (r Rates) Of(id uuid.UUID) RateOn {
	return func(day time.Time, balance int64) (decimal.Decimal, bool, error) {
		res, ok, err := r.On(id, day, balance)
		return res.Percent, ok, err
	}
}

// Pays is how the account's interest is paid, and the first day it earns: a
// savings account at each month's end onto itself; a deposit by its
// programme — from the day after its opening to its term's end, at the end
// or monthly, the way chosen at opening or its variant's only one. ok is
// false for a deposit whose term or way of payment is not known: without a
// programme, or a variant with more than one way and none chosen.
func (r Rates) Pays(a account.Account) (pays Pays, opened time.Time, ok bool) {
	switch a.Type {
	case account.TypeSavings:
		return Pays{Monthly: true, Capitalize: true}, time.Time{}, true
	case account.TypeDeposit:
	default:
		return Pays{}, time.Time{}, false
	}
	b := r.sources[a.ID].Binding
	if b == nil || b.Term == nil {
		return Pays{}, time.Time{}, false
	}
	p, found := deposit.ByID(b.Programme)
	if !found {
		return Pays{}, time.Time{}, false
	}
	e, found := p.EditionOn(b.OpenedOn)
	if !found {
		return Pays{}, time.Time{}, false
	}
	v, found := e.Variant(b.Variant)
	if !found {
		return Pays{}, time.Time{}, false
	}
	way, chosen := deposit.Payout(""), b.Payout != nil && slices.Contains(v.Payouts, *b.Payout)
	switch {
	case chosen:
		way = *b.Payout
	case len(v.Payouts) == 1:
		way = v.Payouts[0]
	default:
		return Pays{}, time.Time{}, false
	}
	ends := b.OpenedOn.AddDate(0, 0, *b.Term)
	if v.TermUnit == deposit.TermMonths {
		ends = b.OpenedOn.AddDate(0, *b.Term, 0)
	}
	pays = Pays{Ends: ends}
	switch way {
	case deposit.PayoutEnd:
	case deposit.PayoutCapitalize:
		pays.Monthly, pays.Capitalize = true, true
	case deposit.PayoutMonthly:
		pays.Monthly = true
	default:
		// Every day or every quarter: not in the reckoning yet.
		return Pays{}, time.Time{}, false
	}
	return pays, b.OpenedOn.AddDate(0, 0, 1), true
}

// Opening is a deposit's day of opening and the sum it was opened with, by
// its programme; ok is false without them.
func (r Rates) Opening(id uuid.UUID) (on time.Time, amount int64, ok bool) {
	b := r.sources[id].Binding
	if b == nil || b.Amount == nil {
		return time.Time{}, 0, false
	}
	return b.OpenedOn, *b.Amount, true
}

// OpenedOn is the day an account's programme says it was opened or last
// prolonged; ok is false without a programme.
func (r Rates) OpenedOn(id uuid.UUID) (time.Time, bool) {
	b := r.sources[id].Binding
	if b == nil {
		return time.Time{}, false
	}
	return b.OpenedOn, true
}
