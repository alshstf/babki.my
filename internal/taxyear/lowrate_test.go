package taxyear

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

// НК РФ ст. 214.2 п. 1: interest of a rouble account at no more than 1 % a
// year all the year is not in the base. Seen on 17 October 2026:
//   - «Копилка», kept by its journal at 0,5 % since 1 January, paid 4 000 ₽
//     and 1 000 ₽ more filed under «Проценты»: out, said so;
//   - «Растущий», 0,5 % to June and 2 % from July, paid 6 000 ₽: in;
//   - «Новый», 0,5 % written in from 1 March only — the start of the year
//     unknown: in;
//   - «Карта», a current account, paid 3 000 ₽: in, its rate not known,
//     counted apart.
func TestARoubleAccountAtNoMoreThanOnePercentIsLeftOut(t *testing.T) {
	maria := member("Мария", family.RoleOwner)
	acc := func(name string, typ account.Type) account.WithBalance {
		a := bank(name, typ, &maria.ID, true)
		a.Currency = "RUB"
		return a
	}
	low, rising, fresh, card := acc("Копилка", account.TypeSavings), acc("Растущий", account.TypeSavings), acc("Новый", account.TypeSavings), acc("Карта", account.TypeChecking)
	rate := func(from, pct string) account.Rate {
		return account.Rate{From: day(from), Kind: account.RateFixed, Percent: decimal.RequireFromString(pct), ByHand: true}
	}
	pct := category.TaxInterest
	interestCat := category.Category{ID: uuid.New(), Kind: category.KindIncome, Name: "Проценты", TaxMark: &pct}
	filed := portfolio.Operation{
		Type: portfolio.TypeDeposit, AccountID: low.ID, OccurredOn: day("2026-09-30"), AmountMinor: 1_000_00, Currency: "RUB", CategoryID: &interestCat.ID,
	}
	svc := NewService(fakeAccounts{low, rising, fresh, card},
		fakeFacts{low.ID: {Interest: 4_000_00}, rising.ID: {Interest: 6_000_00}, fresh.ID: {Interest: 2_000_00}, card.ID: {Interest: 3_000_00}},
		fakeSpaces{residency: "RU", members: []family.Member{maria}}, fakeKeys{rate16}).
		WithInterest(fakeEarning{rates: map[uuid.UUID][]account.Rate{
			low.ID:    {rate("2026-01-01", "0.5")},
			rising.ID: {rate("2026-01-01", "0.5"), rate("2026-07-01", "2")},
			fresh.ID:  {rate("2026-03-01", "0.5")},
		}}, nil, nil, fakeCash{}).
		WithEarnings(fakeFlows{filed}, fakeCategories{interestCat})
	svc.now = func() time.Time { return day("2026-10-17") }
	y, err := svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	p := y.People[0]
	if p.Interest != 11_000_00 || p.InterestChecking != 3_000_00 {
		t.Errorf("interest %d, of current accounts %d; want 11 000 and 3 000", p.Interest, p.InterestChecking)
	}
	flags := map[string][2]bool{}
	for _, a := range p.Accounts {
		flags[a.Name] = [2]bool{a.LowRate, a.Checking}
	}
	want := map[string][2]bool{"Копилка": {true, false}, "Растущий": {false, false}, "Новый": {false, false}, "Карта": {false, true}}
	for name, w := range want {
		if flags[name] != w {
			t.Errorf("%s: low rate, current = %v, want %v", name, flags[name], w)
		}
	}
	// On its card: no income of the year for the tax, no share.
	in, err := svc.IncomeOf(t.Context(), uuid.New(), low.ID)
	if err != nil || !in.LowRate || in.Year != nil || in.Tax == nil || in.Tax.Share != 0 {
		t.Errorf("the card = %+v, %v", in, err)
	}
}
