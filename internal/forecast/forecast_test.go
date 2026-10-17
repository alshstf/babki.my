package forecast

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/goals"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/recurring"
	"babki.my/babki/internal/valuables"
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

type fakeBudgets []budget.Expected

func (f fakeBudgets) Expect(context.Context, uuid.UUID, time.Time, time.Time) ([]budget.Expected, error) {
	return f, nil
}

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
		fakeBudgets{},
		fakeRates{},
	)
	svc.now = func() time.Time { return d("2026-10-10").Add(15 * time.Hour) }

	f, err := svc.Of(context.Background(), uuid.New(), 40, false)
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
	svc := NewService(fakeAccounts{}, fakePositions{}, fakeSpaces{}, fakeRegulars{}, fakeLoans{}, fakeCards{}, fakeBudgets{}, fakeRates{})
	for _, days := range []int{0, MinDays - 1, MaxDays + 1} {
		if _, err := svc.Of(context.Background(), uuid.New(), days, false); err == nil {
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
		fakeLoans{}, fakeCards{}, fakeBudgets{}, fakeRates{})
	svc.now = func() time.Time { return d("2026-10-10") }
	f, err := svc.Of(context.Background(), uuid.New(), 40, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Events) != 1 || f.Events[0].On.Format(time.DateOnly) != "2026-11-06" || f.Events[0].Overdue {
		t.Errorf("events = %+v, want only the rent of 6 November", f.Events)
	}
}

type fakeGoals map[uuid.UUID]goals.Reserve

func (f fakeGoals) SetAside(context.Context, uuid.UUID) (map[uuid.UUID]goals.Reserve, error) {
	return f, nil
}

// The savings goals' money is not to be spent (Р-44): the whole savings
// account of the cushion, 70 000 of the holiday's out of the current account
// and, of a sum out of the cash larger than it, the cash there is, are left
// out of what can be spent until the salary; the deposit's goal is not in
// the money to live on at all.
func TestTheGoalsMoneyIsNotToSpend(t *testing.T) {
	acc := func(name string, typ account.Type, minor int64) account.WithBalance {
		return account.WithBalance{
			Account: account.Account{ID: uuid.New(), Name: name, Type: typ, Currency: "RUB", Status: account.StatusActive},
			Balance: &account.BalancePoint{AsOf: d("2026-10-09"), AmountMinor: minor},
		}
	}
	current, savings := acc("Текущий", account.TypeChecking, 150_000_00), acc("Накопительный", account.TypeSavings, 450_000_00)
	cash, deposit := acc("Наличные", account.TypeCash, 20_000_00), acc("Вклад", account.TypeDeposit, 300_000_00)
	svc := NewService(fakeAccounts{current, savings, cash, deposit}, fakePositions{}, fakeSpaces{},
		fakeRegulars{
			{Name: "Аренда", Cadence: recurring.Monthly, Amount: -30_000_00, Currency: "RUB", AccountID: current.ID, Next: d("2026-10-20")},
			{Name: "Зарплата", Cadence: recurring.Monthly, Amount: 100_000_00, Currency: "RUB", AccountID: current.ID, Next: d("2026-11-05")},
		},
		fakeLoans{}, fakeCards{}, fakeBudgets{}, fakeRates{}).WithGoals(fakeGoals{
		savings.ID: {Whole: true},
		current.ID: {Minor: 70_000_00},
		cash.ID:    {Minor: 1_000_000_00},
		deposit.ID: {Whole: true},
	})
	svc.now = func() time.Time { return d("2026-10-10") }
	f, err := svc.Of(context.Background(), uuid.New(), 40, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Start != 620_000_00 || f.Reserved != 540_000_00 || f.FreeUntilIncome == nil || *f.FreeUntilIncome != 50_000_00 {
		t.Errorf("start %d, reserved %d, free %v; want 620 000, 540 000 and 50 000", f.Start, f.Reserved, f.FreeUntilIncome)
	}
	if f.Lowest.Balance != 590_000_00 {
		t.Errorf("lowest = %+v; the goals' money is still there", f.Lowest)
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

// The budget's limits, when asked: what is left of this month's «Продукты»
// in even parts every seven days from today; the rent of «Дом», limited and
// regular, taken off its limit; November's whole limits from its first day,
// the horizon's cut month by its share of the days.
func TestTheBudgetIsSpentWeekByWeek(t *testing.T) {
	current := account.WithBalance{
		Account: account.Account{ID: uuid.New(), Name: "Текущий", Type: account.TypeChecking, Currency: "RUB", Status: account.StatusActive},
		Balance: &account.BalancePoint{AsOf: d("2026-10-10"), AmountMinor: 100_000_00},
	}
	food, home := uuid.New(), uuid.New()
	rent := recurring.Payment{Name: "Аренда", Cadence: recurring.Monthly, Amount: -30_000_00, Currency: "RUB", AccountID: current.ID, Next: d("2026-10-20"), CategoryID: &home}
	svc := NewService(fakeAccounts{current}, fakePositions{}, fakeSpaces{}, fakeRegulars{rent}, fakeLoans{}, fakeCards{},
		fakeBudgets{
			{Month: d("2026-10-01"), CategoryID: food, Covers: []uuid.UUID{food}, Amount: 9_000_00},
			{Month: d("2026-10-01"), CategoryID: home, Covers: []uuid.UUID{home}, Amount: 35_000_00},
			{Month: d("2026-11-01"), CategoryID: food, Covers: []uuid.UUID{food}, Amount: 15_000_00},
		}, fakeRates{})
	svc.now = func() time.Time { return d("2026-10-10") }

	f, err := svc.Of(context.Background(), uuid.New(), 30, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range f.Events {
		if e.Kind == KindBudget {
			got[e.On.Format(time.DateOnly)] = e.InBase
		}
	}
	// October: 9 000 for food and 35 000 − 30 000 of rent for home, over
	// 10, 17, 24 and 31 October. November to 8 November (the horizon): 15 000
	// × 8/30 = 4 000 over 1 and 8 November.
	want := map[string]int64{
		"2026-10-10": -3_500_00, "2026-10-17": -3_500_00, "2026-10-24": -3_500_00, "2026-10-31": -3_500_00,
		"2026-11-01": -2_000_00, "2026-11-08": -2_000_00,
	}
	if len(got) != len(want) {
		t.Fatalf("budget events = %v, want %v", got, want)
	}
	for on, v := range want {
		if got[on] != v {
			t.Errorf("%s: %d, want %d", on, got[on], v)
		}
	}
	without, err := svc.Of(context.Background(), uuid.New(), 30, false)
	if err != nil {
		t.Fatal(err)
	}
	if without.Lowest.Balance-f.Lowest.Balance != 18_000_00 {
		t.Errorf("lowest with the budget %d, without %d: want 18 000 apart", f.Lowest.Balance, without.Lowest.Balance)
	}
}

// A safe box rented till 20.11 at 24 000 a year: the renewal on its day. A
// custody whose term passed on 01.10 at 1 500 a month: expected today, then
// on the 1st. No term, or the journal already repeating about the fee yearly:
// nothing.
func TestAStoragesFeeComesOnItsRenewal(t *testing.T) {
	today, end := d("2026-10-13"), d("2027-01-11")
	at := func(until string, fee int64, period valuables.FeePeriod) valuables.StorageAt {
		st := valuables.StorageAt{AccountID: uuid.New(), Name: "Ячейка ВТБ", Currency: "RUB", Storage: valuables.Storage{Place: valuables.PlaceSafeBox, FeeMinor: &fee, FeePeriod: &period}}
		if until != "" {
			u := d(until)
			st.Storage.Until = &u
		}
		return st
	}
	box := fees(at("2026-11-20", 24_000_00, valuables.FeeYear), nil, today, end)
	if len(box) != 1 || !box[0].On.Equal(d("2026-11-20")) || box[0].Amount != -24_000_00 || box[0].Kind != KindStorage || box[0].Overdue {
		t.Errorf("box = %+v", box)
	}
	custody := fees(at("2026-10-01", 1_500_00, valuables.FeeMonth), nil, today, end)
	var days []string
	for _, e := range custody {
		days = append(days, e.On.Format(time.DateOnly))
	}
	if len(custody) != 4 || !custody[0].Overdue || fmt.Sprint(days) != "[2026-10-13 2026-11-01 2026-12-01 2027-01-01]" {
		t.Errorf("custody = %v", days)
	}
	if got := fees(at("", 24_000_00, valuables.FeeYear), nil, today, end); got != nil {
		t.Errorf("no term = %+v", got)
	}
	journal := []recurring.Payment{{Name: "ВТБ", Cadence: recurring.Yearly, Amount: -23_000_00, Currency: "RUB", Next: d("2026-11-19")}}
	if got := fees(at("2026-11-20", 24_000_00, valuables.FeeYear), journal, today, end); got != nil {
		t.Errorf("in the journal = %+v", got)
	}
}

type fakeEarning map[uuid.UUID][]account.Rate

func (f fakeEarning) AllRates(context.Context, uuid.UUID) (map[uuid.UUID][]account.Rate, error) {
	return f, nil
}

func (fakeEarning) AllBindings(context.Context, uuid.UUID) (map[uuid.UUID]account.Binding, error) {
	return nil, nil
}

func (fakeEarning) AllMarks(context.Context, uuid.UUID) (map[uuid.UUID][]account.Mark, error) {
	return nil, nil
}

// A savings account's interest comes on each month's last day by its rate on
// today's balance, capitalized, and stands for the account's regular incomes:
// 900 000 ₽ at 14 % — 900 000 × 0,14 × 31 / 365 = 10 701,37 ₽ on 31 October
// (the rate written in from the 5th counts for the whole month, as the bank
// pays for it), then 10 479,30 ₽ on 30 November on the balance with October's
// in. A savings account with money and no rate is named; a deposit is not
// money to live on.
func TestASavingsAccountsInterestComesAtTheMonthsEnd(t *testing.T) {
	acc := func(name string, typ account.Type, minor int64) account.WithBalance {
		return account.WithBalance{
			Account: account.Account{ID: uuid.New(), Name: name, Type: typ, Currency: "RUB", Status: account.StatusActive},
			Balance: &account.BalancePoint{AsOf: d("2026-10-09"), AmountMinor: minor},
		}
	}
	savings, bare := acc("Накопительный", account.TypeSavings, 900_000_00), acc("Без ставки", account.TypeSavings, 50_000_00)
	deposit := acc("Вклад", account.TypeDeposit, 300_000_00)
	rate := func(pct string) []account.Rate {
		return []account.Rate{{From: d("2026-10-05"), Kind: account.RateFixed, Percent: decimal.RequireFromString(pct), ByHand: true}}
	}
	svc := NewService(fakeAccounts{savings, bare, deposit}, fakePositions{}, fakeSpaces{},
		fakeRegulars{{Name: "Проценты", Cadence: recurring.Monthly, Amount: 10_000_00, Currency: "RUB", AccountID: savings.ID, Next: d("2026-10-31")}},
		fakeLoans{}, fakeCards{}, fakeBudgets{}, fakeRates{}).WithInterest(fakeEarning{savings.ID: rate("14"), deposit.ID: rate("16")}, nil, nil)
	svc.now = func() time.Time { return d("2026-10-10") }
	f, err := svc.Of(context.Background(), uuid.New(), 60, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range f.Events {
		got = append(got, fmt.Sprintf("%s %s %s %d", e.On.Format(time.DateOnly), e.Kind, e.Name, e.InBase))
	}
	want := []string{"2026-10-31 interest Накопительный 1070137", "2026-11-30 interest Накопительный 1047930"}
	if !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if !slices.Equal(f.Unearning, []string{"Без ставки"}) {
		t.Errorf("unearning = %q", f.Unearning)
	}
	// Interest is no salary: without a regular income nothing is in sight.
	if f.NextIncome != nil {
		t.Errorf("next income = %+v", f.NextIncome)
	}
}
