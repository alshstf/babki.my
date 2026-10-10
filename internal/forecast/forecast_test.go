package forecast

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/recurring"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

type fakeAccounts []account.WithBalance

func (f fakeAccounts) ListWithBalance(context.Context, uuid.UUID) ([]account.WithBalance, error) {
	return f, nil
}

// fakePositions: the card is kept by its journal, which holds 7 500 roubles.
type fakePositions struct{ journal uuid.UUID }

func (f fakePositions) Positions(_ context.Context, _, id uuid.UUID) (apitypes.PositionsResponse, int, error) {
	if id != f.journal {
		return apitypes.PositionsResponse{}, 0, nil
	}
	return apitypes.PositionsResponse{Cash: []apitypes.CashPosition{{Currency: "RUB", AmountMinor: -7_500_00}}}, 3, nil
}

type fakeSpaces struct{}

func (fakeSpaces) SpaceByID(context.Context, uuid.UUID) (family.Space, error) {
	return family.Space{BaseCurrency: "RUB"}, nil
}

type fakeRegulars []recurring.Payment

func (f fakeRegulars) Find(context.Context, uuid.UUID) ([]recurring.Payment, error) { return f, nil }

type fakeLoans []loan.Terms

func (f fakeLoans) All(context.Context, uuid.UUID) ([]loan.Terms, error) { return f, nil }

type fakeCards []creditcard.Card

func (f fakeCards) All(context.Context, uuid.UUID) ([]creditcard.Card, error) { return f, nil }

// fakeRates knows the dollar at 90 roubles and nothing else.
type fakeRates struct{}

func (fakeRates) Rate(_ context.Context, from, to string, _ time.Time) (decimal.Decimal, time.Time, error) {
	if from == "USD" && to == "RUB" {
		return decimal.NewFromInt(90), time.Time{}, nil
	}
	return decimal.Zero, time.Time{}, marketdata.ErrNoRate
}

func (fakeRates) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

// A family with a current account, a card kept by its journal, dollars in
// cash, a deposit, a broker and a mortgage. The money to live on is the
// first three; ahead of it the rent, the mortgage's schedule and the salary
// with its advance. The mortgage's interest, found among the regular
// payments under the loan's name, is not counted twice; a payment on the
// deposit is not counted at all, nor one the family said is not regular.
func TestTheMoneyAheadIsWorkedOutDayByDay(t *testing.T) {
	acc := func(name string, typ account.Type, currency string, balance *int64, byJournal bool) account.WithBalance {
		a := account.WithBalance{Account: account.Account{
			ID: uuid.New(), Name: name, Type: typ, Currency: currency, Status: account.StatusActive, KeptByOperations: byJournal,
		}}
		if balance != nil {
			a.Balance = &account.BalancePoint{AsOf: d("2026-10-01"), AmountMinor: *balance}
		}
		return a
	}
	n := func(v int64) *int64 { return &v }
	current := acc("Текущий Сбер", account.TypeChecking, "RUB", n(50_000_00), false)
	card := acc("Кредитка Альфа", account.TypeCreditCard, "RUB", n(-99_000_00), true)
	cash := acc("Доллары", account.TypeCash, "USD", n(100_00), false)
	deposit := acc("Вклад", account.TypeDeposit, "RUB", n(500_000_00), false)
	broker := acc("Брокер", account.TypeBrokerage, "RUB", n(1_000_000_00), false)
	mortgage := acc("Ипотека ВТБ", account.TypeLoan, "RUB", nil, false)

	pay := func(name string, on time.Time, amount int64, a account.WithBalance) recurring.Payment {
		return recurring.Payment{Name: name, Cadence: recurring.Monthly, Amount: amount, Currency: a.Currency, AccountID: a.ID, Next: on}
	}
	svc := NewService(
		fakeAccounts{current, card, cash, deposit, broker, mortgage},
		fakePositions{journal: card.ID},
		fakeSpaces{},
		fakeRegulars{
			pay("ИП Смирнова", d("2026-10-06"), -45_000_00, current), // overdue: expected today
			pay("ипотека втб", d("2026-10-15"), -46_000_00, current),
			pay("ООО «Ромашка»", d("2026-10-25"), 60_000_00, current),
			pay("ООО «Ромашка»", d("2026-11-05"), 180_000_00, current),
			pay("Проценты по вкладу", d("2026-10-20"), 3_000_00, deposit),
			// The card's cashback as the journal repeats it: its rules say
			// it instead.
			pay("Кэшбэк", d("2026-10-28"), 900_00, card),
			func() recurring.Payment {
				p := pay("Пятёрочка", d("2026-10-12"), -1_500_00, current)
				p.Hidden = true
				return p
			}(),
		},
		fakeLoans{{AccountID: mortgage.ID, Principal: 3_000_000_00, AnnualRate: decimal.RequireFromString("18.5"), TermMonths: 240, IssuedOn: d("2026-03-15"), Kind: loan.Annuity}},
		fakeCards{{
			Account: card,
			Terms:   creditcard.Terms{Cashback: creditcard.Cashback{BasePercent: decimal.NewFromInt(1)}},
			Status:  creditcard.Status{CashbackExpected: 1_234_00, CashbackOn: d("2026-11-01")},
		}},
		fakeRates{},
	)
	svc.now = func() time.Time { return d("2026-10-10").Add(15 * time.Hour) }

	f, err := svc.Of(context.Background(), uuid.New(), 40)
	if err != nil {
		t.Fatal(err)
	}
	// 50 000 on the account, the card's journal 7 500 in debt (not its
	// balance), 100 dollars at 90.
	if f.Start != 51_500_00 || f.Accounts != 3 {
		t.Fatalf("start = %d on %d accounts, want 51 500,00 on 3", f.Start, f.Accounts)
	}
	type ev struct {
		on, name string
		amount   int64
	}
	var got []ev
	for _, e := range f.Events {
		got = append(got, ev{e.On.Format(time.DateOnly), e.Name, e.InBase})
	}
	want := []ev{
		{"2026-10-10", "ИП Смирнова", -45_000_00},
		{"2026-10-15", "Ипотека ВТБ", -47_456_90},
		{"2026-10-25", "ООО «Ромашка»", 60_000_00},
		{"2026-11-01", card.Name, 1_234_00},
		{"2026-11-05", "ООО «Ромашка»", 180_000_00},
		{"2026-11-06", "ИП Смирнова", -45_000_00},
		{"2026-11-15", "Ипотека ВТБ", -47_456_90},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %v, want %v", i, got[i], want[i])
		}
	}
	if !f.Events[0].Overdue {
		t.Error("the rent overdue since the 6th is expected today, and says so")
	}
	if len(f.Series) != 40 || f.Series[0].Balance != 6_500_00 || f.Series[5].Balance != -40_956_90 {
		t.Errorf("series: %d days, today %d, the 15th %d", len(f.Series), f.Series[0].Balance, f.Series[5].Balance)
	}
	if f.Lowest.Balance != -40_956_90 || f.Lowest.On.Format(time.DateOnly) != "2026-10-15" {
		t.Errorf("lowest = %+v", f.Lowest)
	}
	// The salary is the 180 000 on the 5th, the advance being less than half
	// of it; until then the money is lowest on the 15th.
	if f.NextIncome == nil || f.NextIncome.On.Format(time.DateOnly) != "2026-11-05" || f.NextIncome.InBase != 180_000_00 {
		t.Fatalf("next income = %+v", f.NextIncome)
	}
	if f.FreeUntilIncome == nil || *f.FreeUntilIncome != -40_956_90 {
		t.Errorf("free until income = %v", f.FreeUntilIncome)
	}
}

func TestAHorizonOutOfBoundsIsRefused(t *testing.T) {
	svc := NewService(fakeAccounts{}, fakePositions{}, fakeSpaces{}, fakeRegulars{}, fakeLoans{}, fakeCards{}, fakeRates{})
	for _, days := range []int{0, MinDays - 1, MaxDays + 1} {
		if _, err := svc.Of(context.Background(), uuid.New(), days); err == nil {
			t.Errorf("%d days accepted", days)
		}
	}
}

// A balance marked after a payment's day holds it already: the overdue rent
// is not expected again today, only next month.
func TestAPaymentBeforeTheBalanceMarkIsInTheBalance(t *testing.T) {
	current := account.WithBalance{
		Account: account.Account{ID: uuid.New(), Name: "Текущий", Type: account.TypeChecking, Currency: "RUB", Status: account.StatusActive},
		Balance: &account.BalancePoint{AsOf: d("2026-10-08"), AmountMinor: 10_000_00},
	}
	svc := NewService(fakeAccounts{current}, fakePositions{}, fakeSpaces{},
		fakeRegulars{{Name: "Аренда", Cadence: recurring.Monthly, Amount: -5_000_00, Currency: "RUB", AccountID: current.ID, Next: d("2026-10-06")}},
		fakeLoans{}, fakeCards{}, fakeRates{})
	svc.now = func() time.Time { return d("2026-10-10") }
	f, err := svc.Of(context.Background(), uuid.New(), 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Events) != 1 || f.Events[0].On.Format(time.DateOnly) != "2026-11-06" || f.Events[0].Overdue {
		t.Errorf("events = %+v, want only the rent of 6 November", f.Events)
	}
}

// What can be spent is never more than the money now: a salary expected today
// may come after the day's spending.
func TestWhatCanBeSpentStartsFromTheMoneyNow(t *testing.T) {
	today := d("2026-10-10")
	events := []Event{
		{On: today, Name: "Зарплата", Kind: KindRegular, InBase: 100_000_00, Overdue: true},
		{On: d("2026-10-12"), Name: "Связь", Kind: KindRegular, InBase: -1_000_00},
		{On: d("2026-11-05"), Name: "Зарплата", Kind: KindRegular, InBase: 100_000_00},
	}
	_, lowest, next, free := lay(20_000_00, events, today, 40)
	if next == nil || next.On.Format(time.DateOnly) != "2026-11-05" {
		t.Fatalf("next = %+v", next)
	}
	if free == nil || *free != 20_000_00 || lowest.Balance != 20_000_00 || !lowest.On.Equal(today) {
		t.Errorf("free = %v, lowest = %+v; want the 20 000 of now", free, lowest)
	}
}
