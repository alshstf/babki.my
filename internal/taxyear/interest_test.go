package taxyear

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
)

type fakeEarning struct {
	rates    map[uuid.UUID][]account.Rate
	bindings map[uuid.UUID]account.Binding
	history  map[uuid.UUID][]account.BalancePoint
}

func (f fakeEarning) AllRates(context.Context, uuid.UUID) (map[uuid.UUID][]account.Rate, error) {
	return f.rates, nil
}

func (f fakeEarning) AllBindings(context.Context, uuid.UUID) (map[uuid.UUID]account.Binding, error) {
	return f.bindings, nil
}

func (fakeEarning) AllMarks(context.Context, uuid.UUID) (map[uuid.UUID][]account.Mark, error) {
	return nil, nil
}

func (f fakeEarning) BalanceHistory(context.Context, uuid.UUID) (map[uuid.UUID][]account.BalancePoint, error) {
	return f.history, nil
}

type fakeCash map[uuid.UUID]int64

func (f fakeCash) Positions(_ context.Context, _, id uuid.UUID) (apitypes.PositionsResponse, int, error) {
	return apitypes.PositionsResponse{Cash: []apitypes.CashPosition{{Currency: "RUB", AmountMinor: f[id]}}}, 1, nil
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func fixedFrom(from, pct string) []account.Rate {
	return []account.Rate{{From: day(from), Kind: account.RateFixed, Percent: decimal.RequireFromString(pct), ByHand: true}}
}

// The interest by the rates, seen on 17 October 2026:
//   - Maria's savings kept by its balance, 3 000 000 ₽ from the first mark on
//     5 March at 12 %: March to September paid 213 351,55 ₽ (each month's
//     interest added to the balance), October to December bring 98 175,97 more;
//     past the 210 000 free, 13 % of 3 351,55 by notice — 436 ₽ — and of
//     101 527,52 at the year's end — 13 199 ₽;
//   - her deposit kept by its balance with a rate and no programme: when its
//     interest is paid is not known;
//   - Alex's savings kept by its balance, the rate written in from 1 September
//     and the mark of 1 February: the year's interest is not known — money on
//     1 February and no rate; what is to come, on 200 000 ₽ at 10 %: 5 083,57;
//   - his savings kept by its journal, 30 000 paid, 1 000 000 ₽ on it today at
//     10 %: 25 417,81 to come; a dollar savings account: not in roubles.
func TestTheInterestByTheRates(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	withBalance := func(a account.WithBalance, on string, minor int64) account.WithBalance {
		a.Currency = "RUB"
		a.Balance = &account.BalancePoint{AsOf: day(on), AmountMinor: minor}
		return a
	}
	cushion := withBalance(bank("Копилка", account.TypeSavings, &maria.ID, false), "2026-03-05", 3_000_000_00)
	deposit := withBalance(bank("Вклад", account.TypeDeposit, &maria.ID, false), "2026-01-10", 500_000_00)
	late := withBalance(bank("Сейф", account.TypeSavings, &alex.ID, false), "2026-02-01", 200_000_00)
	kept := withBalance(bank("Накопительный", account.TypeSavings, &alex.ID, true), "2026-01-01", 0)
	dollars := withBalance(bank("Долларовый", account.TypeSavings, &alex.ID, false), "2026-01-01", 10_000_00)
	dollars.Currency = "USD"
	earning := fakeEarning{
		rates: map[uuid.UUID][]account.Rate{
			cushion.ID: fixedFrom("2026-01-01", "12"), deposit.ID: fixedFrom("2026-01-01", "16"),
			late.ID: fixedFrom("2026-09-01", "10"), kept.ID: fixedFrom("2026-01-01", "10"), dollars.ID: fixedFrom("2026-01-01", "3"),
		},
		history: map[uuid.UUID][]account.BalancePoint{
			cushion.ID: {*cushion.Balance}, deposit.ID: {*deposit.Balance}, late.ID: {*late.Balance}, dollars.ID: {*dollars.Balance},
		},
	}
	svc := NewService(fakeAccounts{cushion, deposit, late, kept, dollars}, fakeFacts{kept.ID: {Interest: 30_000_00}},
		fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{rate21}).
		WithInterest(earning, nil, nil, fakeCash{kept.ID: 1_000_000_00})
	svc.now = func() time.Time { return day("2026-10-17") }
	y, err := svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 2 {
		t.Fatalf("people = %+v", y.People)
	}
	a, m := y.People[0], y.People[1]
	if m.InterestEstimated != 213_351_55 || m.InterestAhead != 98_175_97 || m.ByNotice != 436_00 || m.ByNoticeYearEnd == nil || *m.ByNoticeYearEnd != 13_199_00 {
		t.Errorf("Maria: estimated %d, ahead %d, by notice %d, at the year's end %v", m.InterestEstimated, m.InterestAhead, m.ByNotice, m.ByNoticeYearEnd)
	}
	estimate := func(p Person, name string) Estimate {
		for _, acc := range p.Accounts {
			if acc.Name == name && acc.Estimate != nil {
				return *acc.Estimate
			}
		}
		t.Fatalf("%s: no estimate in %+v", name, p.Accounts)
		return Estimate{}
	}
	if e := estimate(m, "Копилка"); e.From == nil || !e.From.Equal(day("2026-03-05")) || e.Missing != "" {
		t.Errorf("Копилка: %+v", e)
	}
	if e := estimate(m, "Вклад"); e.Missing != MissingHowPaid || e.Paid != 0 || e.Ahead != 0 {
		t.Errorf("Вклад: %+v", e)
	}
	if e := estimate(a, "Сейф"); e.Missing != MissingRate || e.MissingOn == nil || !e.MissingOn.Equal(day("2026-02-01")) || e.Ahead != 5_083_57 {
		t.Errorf("Сейф: %+v", e)
	}
	if e := estimate(a, "Накопительный"); e.Ahead != 25_417_81 || e.Paid != 0 {
		t.Errorf("Накопительный: %+v", e)
	}
	if e := estimate(a, "Долларовый"); e.Missing != MissingCurrency {
		t.Errorf("Долларовый: %+v", e)
	}
	if a.Interest != 30_000_00 || a.InterestEstimated != 0 || a.InterestAhead != 30_501_38 || a.ByNotice != 0 || *a.ByNoticeYearEnd != 0 {
		t.Errorf("Alex: interest %d, estimated %d, ahead %d, by notice %d", a.Interest, a.InterestEstimated, a.InterestAhead, a.ByNotice)
	}
	// A past year: what the balance account paid by the rate, nothing to come.
	y, err = svc.Of(t.Context(), uuid.New(), 2025)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range y.People {
		if p.InterestAhead != 0 || p.ByNoticeYearEnd != nil {
			t.Errorf("2025, %s: ahead %d, at the year's end %v", p.Name, p.InterestAhead, p.ByNoticeYearEnd)
		}
	}
}

// A deposit kept by its balance, «Стабильное будущее» of банк «Россия» for
// 91 days from 1 August 2026 with 1 000 000 ₽ at 19,5 %, its first mark on
// 1 October: the sum it was opened with earns from the opening, all paid on
// 31 October — 1 000 000 × 0,195 × 91 / 365 = 48 616,44 ₽, still to come on
// 17 October.
func TestADepositEarnsFromItsOpening(t *testing.T) {
	maria := member("Мария", family.RoleOwner)
	dep := bank("Стабильное будущее", account.TypeDeposit, &maria.ID, false)
	dep.Currency = "RUB"
	dep.Balance = &account.BalancePoint{AsOf: day("2026-10-01"), AmountMinor: 1_000_000_00}
	term, amount := 91, int64(1_000_000_00)
	svc := NewService(fakeAccounts{dep}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{maria}}, fakeKeys{rate21}).
		WithInterest(fakeEarning{
			bindings: map[uuid.UUID]account.Binding{dep.ID: {Programme: "rossiya-stabilnoe-budushchee", Variant: "main", OpenedOn: day("2026-08-01"), Term: &term, Amount: &amount}},
			history:  map[uuid.UUID][]account.BalancePoint{dep.ID: {*dep.Balance}},
		}, nil, nil, nil)
	svc.now = func() time.Time { return day("2026-10-17") }
	y, err := svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 1 || y.People[0].InterestAhead != 48_616_44 || y.People[0].InterestEstimated != 0 {
		t.Errorf("people = %+v", y.People)
	}
	// For 367 days it pays on 3 August 2027: a later year's, said so.
	term = 367
	y, err = svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if e := y.People[0].Accounts[0].Estimate; y.People[0].InterestAhead != 0 || e == nil || e.PaysOn == nil || !e.PaysOn.Equal(day("2027-08-03")) {
		t.Errorf("for 367 days: %+v", y.People[0])
	}
}
