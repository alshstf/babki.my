package taxyear

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

var rate16 = func() *decimal.Decimal { r := decimal.NewFromInt(16); return &r }()

// Р-56 Б, the note's example: Мария's two accounts paid 270 000 and 70 000 ₽
// this year, 160 000 free (1 млн × 16 %), 13 % of the 180 000 past it —
// 23 400 ₽, split by their interest: 18 582,35 and 4 817,65 ₽, the kopeck
// of rounding to the larger remainder, the two adding up to the tax.
func TestTheTaxSplitByTheAccountsInterest(t *testing.T) {
	maria := member("Мария", family.RoleOwner)
	year, month := bank("Годовой", account.TypeSavings, &maria.ID, true), bank("Помесячный", account.TypeSavings, &maria.ID, true)
	year.Currency, month.Currency = "RUB", "RUB"
	svc := NewService(fakeAccounts{year, month}, fakeFacts{year.ID: {Interest: 270_000_00}, month.ID: {Interest: 70_000_00}},
		fakeSpaces{residency: "RU", members: []family.Member{maria}}, fakeKeys{rate16}).
		WithInterest(fakeEarning{}, nil, nil, fakeCash{})
	svc.now = func() time.Time { return day("2026-10-17") }
	var shares int64
	for id, want := range map[uuid.UUID]int64{year.ID: 18_582_35, month.ID: 4_817_65} {
		in, err := svc.IncomeOf(t.Context(), uuid.New(), id)
		if err != nil {
			t.Fatal(err)
		}
		if in.Tax == nil || in.Tax.Tax != 23_400_00 || in.Tax.Interest != 340_000_00 || *in.Tax.Free != 160_000_00 || in.Tax.Share != want {
			t.Errorf("share = %+v, want %d", in.Tax, want)
		}
		shares += in.Tax.Share
	}
	if shares != 23_400_00 {
		t.Errorf("the shares add up to %d", shares)
	}
	// The same 70 000 filed under a category marked interest — as a bank's
	// statement brings it — is the account's all the same.
	pct := category.TaxInterest
	interestCat := category.Category{ID: uuid.New(), Kind: category.KindIncome, Name: "Проценты", TaxMark: &pct}
	svc.facts = fakeFacts{year.ID: {Interest: 270_000_00}}
	filed := portfolio.Operation{
		Type: portfolio.TypeDeposit, AccountID: month.ID, OccurredOn: day("2026-09-30"), AmountMinor: 70_000_00, Currency: "RUB", CategoryID: &interestCat.ID,
	}
	svc = svc.WithEarnings(fakeFlows{filed}, fakeCategories{interestCat})
	if in, err := svc.IncomeOf(t.Context(), uuid.New(), month.ID); err != nil || in.Tax == nil || in.Tax.Share != 4_817_65 || in.Year == nil || *in.Year != 70_000_00 {
		t.Errorf("filed under the mark: %+v, year %v, %v", in.Tax, in.Year, err)
	}
	// A broker's account has no income by a rate.
	broker := broker("Т-Банк", &maria.ID, false)
	svc.accounts = fakeAccounts{year, month, broker}
	if _, err := svc.IncomeOf(t.Context(), uuid.New(), broker.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a broker's account: %v", err)
	}
}

// A deposit kept by its balance, «Стабильное будущее» of банк «Россия» for 91
// days from 1 August 2026 with 1 000 000 ₽ at 19,5 %, paying at the end:
// 48 616,44 ₽ for the term, 41 136,99 earned by 17 October (77 days),
// 1 048 616,44 on it on 31 October; this year's interest — the same 48 616,44,
// under the free 160 000: no tax, no share.
func TestADepositsIncome(t *testing.T) {
	maria := member("Мария", family.RoleOwner)
	dep := bank("Стабильное будущее", account.TypeDeposit, &maria.ID, false)
	dep.Currency = "RUB"
	dep.Balance = &account.BalancePoint{AsOf: day("2026-10-01"), AmountMinor: 1_000_000_00}
	term, amount := 91, int64(1_000_000_00)
	svc := NewService(fakeAccounts{dep}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{maria}}, fakeKeys{rate16}).
		WithInterest(fakeEarning{
			bindings: map[uuid.UUID]account.Binding{dep.ID: {Programme: "rossiya-stabilnoe-budushchee", Variant: "main", OpenedOn: day("2026-08-01"), Term: &term, Amount: &amount}},
			history:  map[uuid.UUID][]account.BalancePoint{dep.ID: {*dep.Balance}},
		}, nil, nil, nil)
	svc.now = func() time.Time { return day("2026-10-17") }
	in, err := svc.IncomeOf(t.Context(), uuid.New(), dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Rated || in.Ends == nil || !in.Ends.Equal(day("2026-10-31")) || in.Pays == nil || in.Pays.Monthly ||
		*in.Term != 48_616_44 || *in.Accrued != 41_136_99 || *in.AtEnd != 1_048_616_44 {
		t.Errorf("lines = %+v", in)
	}
	if in.Year == nil || *in.Year != 48_616_44 || *in.YearEnd != 48_616_44 || in.Tax == nil || in.Tax.Tax != 0 || in.Tax.Share != 0 {
		t.Errorf("year = %v, to come %v, tax %+v", in.Year, in.YearEnd, in.Tax)
	}
}

// A savings account kept by its balance, 3 000 000 ₽ from 5 March at 12 %:
// October brings 32 749,77 ₽ on the 31st, the next twelve month-ends
// 407 531,70, to 31 December 98 175,97 — what «Налоги» say is to come.
func TestASavingsAccountsIncome(t *testing.T) {
	maria := member("Мария", family.RoleOwner)
	cushion := bank("Копилка", account.TypeSavings, &maria.ID, false)
	cushion.Currency = "RUB"
	cushion.Balance = &account.BalancePoint{AsOf: day("2026-03-05"), AmountMinor: 3_000_000_00}
	svc := NewService(fakeAccounts{cushion}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{maria}}, fakeKeys{rate16}).
		WithInterest(fakeEarning{
			rates:   map[uuid.UUID][]account.Rate{cushion.ID: fixedFrom("2026-01-01", "12")},
			history: map[uuid.UUID][]account.BalancePoint{cushion.ID: {*cushion.Balance}},
		}, nil, nil, nil)
	svc.now = func() time.Time { return day("2026-10-17") }
	in, err := svc.IncomeOf(t.Context(), uuid.New(), cushion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Rated || *in.Month != 32_749_77 || !in.MonthOn.Equal(day("2026-10-31")) || *in.Twelve != 407_531_70 || *in.YearEnd != 98_175_97 {
		t.Errorf("lines = %+v", in)
	}
	y, err := svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if e := y.People[0].Accounts[0].Estimate; e == nil || e.Ahead != *in.YearEnd || *in.Year != e.Paid+e.Ahead {
		t.Errorf("«Налоги» say %+v, the card %v / %v", e, *in.YearEnd, *in.Year)
	}
}
