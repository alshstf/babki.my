package taxyear

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/valuables"
)

type fakeAccounts []account.WithBalance

func (f fakeAccounts) ListWithBalance(context.Context, uuid.UUID) ([]account.WithBalance, error) {
	return f, nil
}

type fakeFacts map[uuid.UUID]portfolio.YearFacts

func (f fakeFacts) YearFacts(_ context.Context, _, id uuid.UUID, _ int, _ string, _ int) (portfolio.YearFacts, error) {
	return f[id], nil
}

// fakeKeys is the year's highest key rate, none when nil.
type fakeKeys struct{ rate *decimal.Decimal }

func (f fakeKeys) HighestKeyRate(context.Context, time.Time, time.Time) (decimal.Decimal, bool, error) {
	if f.rate == nil {
		return decimal.Zero, false, nil
	}
	return *f.rate, true, nil
}

var rate21 = func() *decimal.Decimal { r := decimal.NewFromInt(21); return &r }()

type fakeSpaces struct {
	residency string
	members   []family.Member
}

func (f fakeSpaces) SpaceByID(context.Context, uuid.UUID) (family.Space, error) {
	return family.Space{TaxResidency: f.residency}, nil
}

func (f fakeSpaces) ListMembers(context.Context, uuid.UUID) ([]family.Member, error) {
	return f.members, nil
}

func member(name string, role family.Role) family.Member {
	return family.Member{User: family.User{ID: uuid.New(), DisplayName: name}, Role: role}
}

func broker(name string, owner *uuid.UUID, foreign bool) account.WithBalance {
	return account.WithBalance{Account: account.Account{
		ID: uuid.New(), Name: name, Type: account.TypeBrokerage, Status: account.StatusActive, OwnerUserID: owner, ForeignBroker: foreign,
	}}
}

// Alex, the owner: two Russian brokers with 2 млн each on papers — each
// withholds 13 %, the 15 % on the 1,6 млн past the threshold is his to pay;
// a shared one at a loss, his by default; a broker abroad whose dividends
// had 10 % taken. Maria: a broker of her own.
func TestTheYearByPerson(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	t1, t2 := broker("Т-Банк", &alex.ID, false), broker("Альфа", &alex.ID, false)
	shared, kz := broker("Общий", nil, false), broker("Freedom KZ", &alex.ID, true)
	mine := broker("Мой", &maria.ID, false)
	f := fakeFacts{
		t1.ID:     {Sales: 2_400_000_00, SalesLong: 400_000_00, Withheld: 6_500_00},
		t2.ID:     {Coupons: 2_000_000_00},
		shared.ID: {Sales: -300_000_00},
		kz.ID:     {Dividends: 100_000_00, Withheld: 10_000_00},
		mine.ID:   {Dividends: 100_000_00},
	}
	svc := NewService(fakeAccounts{t1, t2, shared, kz, mine}, f, fakeSpaces{residency: "RU", members: []family.Member{maria, alex}}, fakeKeys{rate21})
	y, err := svc.Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 2 || y.People[0].UserID != alex.ID || y.Currency != "RUB" || y.DeclareBy.Format("2006-01-02") != "2027-04-30" || y.PayBy.Format("2006-01-02") != "2027-07-15" {
		t.Fatalf("year = %+v", y)
	}
	a := y.People[0]
	// Papers: 2 000 000 (long gain off) + 2 000 000 − 300 000 = 3 700 000;
	// dividends 100 000: base 3 800 000 — 312 000 + 15 % of 1 400 000.
	if a.Base != 3_800_000_00 || a.Tax != 312_000_00+210_000_00 {
		t.Errorf("Alex's base %d, tax %d", a.Base, a.Tax)
	}
	// Each Russian broker 13 % on its 2 000 000; the shared one nothing.
	if a.ByAgents != 520_000_00 || a.Withheld != 6_500_00 || a.Credit != 10_000_00 {
		t.Errorf("by agents %d, withheld %d, credit %d", a.ByAgents, a.Withheld, a.Credit)
	}
	if a.ToPay != 0 || a.Refund != 8_000_00 {
		t.Errorf("to pay %d, refund %d", a.ToPay, a.Refund)
	}
	if len(a.Accounts) != 4 || a.Accounts[0].Securities != 2_000_000_00 || a.Accounts[3].AgentTax != 0 {
		t.Errorf("accounts %+v", a.Accounts)
	}
	m := y.People[1]
	if m.UserID != maria.ID || m.Base != 100_000_00 || m.Tax != 13_000_00 || m.ByAgents != 13_000_00 || m.ToPay != 0 {
		t.Errorf("Maria = %+v", m)
	}
}

// Without the shared loss and with the dividends abroad taxed 13 % by the
// broker's country, the 15 % past the threshold is left to pay.
func TestTheBandAboveTheThresholdIsLeftToPay(t *testing.T) {
	alex := member("Алекс", family.RoleOwner)
	t1, t2 := broker("Т-Банк", &alex.ID, false), broker("Альфа", &alex.ID, false)
	f := fakeFacts{t1.ID: {Sales: 2_000_000_00}, t2.ID: {Sales: 2_000_000_00}}
	y, err := NewService(fakeAccounts{t1, t2}, f, fakeSpaces{residency: "RU", members: []family.Member{alex}}, fakeKeys{rate21}).Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if p := y.People[0]; p.Tax != 552_000_00 || p.ByAgents != 520_000_00 || p.ToPay != 32_000_00 || p.Refund != 0 {
		t.Errorf("person = %+v", p)
	}
}

// A tax taken abroad offsets no more than the Russian tax on the dividends;
// an account with nothing in the year leaves its person out; a country with
// no rules for the year's tax has no estimate.
func TestTheCreditAndWhatIsLeftOut(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	kz, quiet := broker("Freedom KZ", &alex.ID, true), broker("Пустой", &maria.ID, false)
	f := fakeFacts{kz.ID: {Dividends: 100_000_00, Withheld: 30_000_00}}
	y, err := NewService(fakeAccounts{kz, quiet}, f, fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{rate21}).Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 1 || y.People[0].Credit != 13_000_00 || y.People[0].ToPay != 0 {
		t.Errorf("year = %+v", y.People)
	}
	if _, err := NewService(fakeAccounts{kz}, f, fakeSpaces{residency: "DE"}, fakeKeys{rate21}).Of(t.Context(), uuid.New(), 2026); err != ErrNoEstimate {
		t.Errorf("Germany: %v", err)
	}
}

// The year asked: this one when absent; a past one; never one ahead.
func TestTheYearAsked(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for raw, want := range map[string]int{"": 2026, "2025": 2025, "2026": 2026} {
		if got, err := parseYear(raw, now); err != nil || got != want {
			t.Errorf("%q: %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"2027", "1999", "год"} {
		if _, err := parseYear(raw, now); err == nil {
			t.Errorf("%q taken", raw)
		}
	}
}

func bank(name string, typ account.Type, owner *uuid.UUID, kept bool) account.WithBalance {
	return account.WithBalance{Account: account.Account{
		ID: uuid.New(), Name: name, Type: typ, Status: account.StatusActive, OwnerUserID: owner, KeptByOperations: kept,
	}}
}

// Deposits' interest: free up to 1 млн × the year's highest key rate (21 % —
// 210 000); Maria's 400 000 leaves 190 000, 13 % by notice. Alex's interest
// sits above his 2,4 млн on papers, so its tax is at 15 %, all by notice,
// none of it his to pay by return. A deposit kept by its balance is counted
// apart: its interest is not known.
func TestDepositsInterest(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	t1 := broker("Т-Банк", &alex.ID, false)
	alexSavings, mariaSavings := bank("Накопительный", account.TypeSavings, &alex.ID, true), bank("Вклад", account.TypeDeposit, &maria.ID, true)
	byBalance := bank("Вклад остатком", account.TypeDeposit, &maria.ID, false)
	f := fakeFacts{
		t1.ID:           {Sales: 2_400_000_00},
		alexSavings.ID:  {Interest: 1_210_000_00},
		mariaSavings.ID: {Interest: 400_000_00},
	}
	svc := NewService(fakeAccounts{t1, alexSavings, mariaSavings, byBalance}, f, fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{rate21})
	y, err := svc.Of(t.Context(), uuid.New(), 2025)
	if err != nil {
		t.Fatal(err)
	}
	if y.KeyRate == nil || !y.KeyRate.Equal(decimal.NewFromInt(21)) || y.ByBalance != 1 || y.NoticeBy.Format("2006-01-02") != "2026-12-01" {
		t.Fatalf("year = %+v", y)
	}
	a, m := y.People[0], y.People[1]
	if a.Interest != 1_210_000_00 || *a.InterestFree != 210_000_00 || a.Base != 3_400_000_00 || a.Tax != 462_000_00 || a.ByNotice != 150_000_00 || a.ByAgents != 312_000_00 || a.ToPay != 0 || a.Refund != 0 {
		t.Errorf("Alex = %+v", a)
	}
	if len(a.Accounts) != 2 || !a.Accounts[1].Bank || a.Accounts[1].AgentTax != 0 {
		t.Errorf("Alex's accounts = %+v", a.Accounts)
	}
	if m.Base != 190_000_00 || m.Tax != 24_700_00 || m.ByNotice != 24_700_00 || m.ToPay != 0 {
		t.Errorf("Maria = %+v", m)
	}
	// Without the key rate the free part is not known: the interest is shown,
	// not taxed.
	y, err = NewService(fakeAccounts{mariaSavings}, f, fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{}).Of(t.Context(), uuid.New(), 2025)
	if err != nil || y.KeyRate != nil || y.People[0].InterestFree != nil || y.People[0].Tax != 0 || y.People[0].Interest != 400_000_00 {
		t.Errorf("no key rate: %+v, %v", y, err)
	}
}

type fakeFlows []portfolio.Operation

func (f fakeFlows) ListMoneyFlows(context.Context, uuid.UUID, time.Time, time.Time) ([]portfolio.Operation, error) {
	return f, nil
}

type fakeCategories []category.Category

func (f fakeCategories) List(context.Context, uuid.UUID) ([]category.Category, error) { return f, nil }

// The gross behind a net: 87 000 is 100 000 less 13 %; after 2 300 000 paid
// before, 172 000 is 200 000 — 100 000 at 13 % and 100 000 at 15 %.
func TestGrossOfNet(t *testing.T) {
	ru := family.TaxRulesFor("RU")
	bands := ru.MainBands[:]
	if g := grossOf(bands, 0, 87_000_00); g != 100_000_00 {
		t.Errorf("87 000: %d", g)
	}
	if g := grossOf(bands, 2_300_000_00, 172_000_00); g != 200_000_00 {
		t.Errorf("172 000 after 2 300 000: %d", g)
	}
}

// Wages are earnings under a category marked wages, by employer — the row's
// counterparty — whose they are the member who earned them, else the
// account's person. Alex's two employers each pay under 2,4 млн, so each
// withholds 13 % and the 15 % past the threshold on their sum is his to pay.
// Interest filed under the marked category joins the interest; a wage in
// dollars is not counted.
func TestWages(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	card := bank("Карта", account.TypeChecking, nil, true)
	wages, interest := category.TaxWages, category.TaxInterest
	salary := category.Category{ID: uuid.New(), Kind: category.KindIncome, Name: "Зарплата", TaxMark: &wages}
	pct := category.Category{ID: uuid.New(), Kind: category.KindIncome, Name: "Проценты", TaxMark: &interest}
	other := category.Category{ID: uuid.New(), Kind: category.KindIncome, Name: "Подарки"}
	day := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	op := func(cat uuid.UUID, minor int64, who *uuid.UUID, employer, currency string) portfolio.Operation {
		return portfolio.Operation{
			Type: portfolio.TypeDeposit, AccountID: card.ID, OccurredOn: day, AmountMinor: minor, Currency: currency,
			CategoryID: &cat, Counterparty: employer, MemberID: who,
		}
	}
	flows := fakeFlows{
		op(salary.ID, 1_740_000_00, nil, "ООО «Ромашка»", "RUB"),
		op(salary.ID, 1_740_000_00, nil, "ООО «Василёк»", "RUB"),
		op(salary.ID, 87_000_00, &maria.ID, "ООО «Ромашка»", "RUB"),
		op(pct.ID, 300_000_00, &maria.ID, "", "RUB"),
		op(other.ID, 50_000_00, nil, "", "RUB"),
		op(salary.ID, 1_000_00, nil, "Acme", "USD"),
	}
	svc := NewService(fakeAccounts{card}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{rate21}).
		WithEarnings(flows, fakeCategories{salary, pct, other})
	y, err := svc.Of(t.Context(), uuid.New(), 2025)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 2 {
		t.Fatalf("people = %+v", y.People)
	}
	a, m := y.People[0], y.People[1]
	if len(a.Employers) != 2 || a.Employers[0].Name != "ООО «Ромашка»" || a.Employers[0].Gross != 2_000_000_00 || a.Employers[0].Withheld != 260_000_00 {
		t.Errorf("Alex's employers = %+v", a.Employers)
	}
	if a.Wages != 4_000_000_00 || a.WagesTax != 552_000_00 || a.WagesWithheld != 520_000_00 || a.WagesToPay != 32_000_00 || a.Unvalued != 1 {
		t.Errorf("Alex = %+v", a)
	}
	if m.Wages != 100_000_00 || m.WagesToPay != 0 || m.Interest != 300_000_00 || m.ByNotice != 11_700_00 {
		t.Errorf("Maria = %+v", m)
	}
}

type fakeSold []valuables.SoldPiece

func (f fakeSold) SoldIn(context.Context, uuid.UUID, int) ([]valuables.SoldPiece, error) {
	return f, nil
}

// Bars sold: Maria's 50 g of 2024, sold for 525 000 against 410 000, gains
// 115 000 — less than 525 000 less 250 000 — so 115 000 is taxed, by return;
// the bar of 2020 sold free; Alex's 10 g sold for 95 000 at a gain of 35 000
// is under the year's 250 000 — nothing.
func TestPropertySold(t *testing.T) {
	alex, maria := member("Алекс", family.RoleOwner), member("Мария", family.RoleEditor)
	sold := func(owner *uuid.UUID, bought, on string, cost, minor int64) valuables.SoldPiece {
		b, _ := time.Parse(time.DateOnly, bought)
		d, _ := time.Parse(time.DateOnly, on)
		return valuables.SoldPiece{Item: valuables.Item{OwnerUserID: owner, BoughtOn: b, CostMinor: cost, SoldOn: &d, SoldMinor: &minor}}
	}
	pieces := fakeSold{
		sold(&maria.ID, "2024-11-05", "2026-10-01", 410_000_00, 525_000_00),
		sold(&maria.ID, "2020-01-10", "2026-10-01", 300_000_00, 1_100_000_00),
		sold(nil, "2025-05-20", "2026-10-01", 60_000_00, 95_000_00),
	}
	y, err := NewService(fakeAccounts{}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{alex, maria}}, fakeKeys{rate21}).
		WithProperty(pieces).Of(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(y.People) != 2 {
		t.Fatalf("people = %+v", y.People)
	}
	m, a := y.People[1], y.People[0]
	if m.UserID != maria.ID || m.PropertySold != 525_000_00 || m.PropertyGain != 115_000_00 || m.PropertyBase != 115_000_00 || m.PropertyFree != 1 ||
		m.Tax != 14_950_00 || m.ToPay != 14_950_00 {
		t.Errorf("Maria = %+v", m)
	}
	if a.PropertyBase != 0 || a.PropertySold != 95_000_00 || a.Tax != 0 {
		t.Errorf("Alex = %+v", a)
	}
}
