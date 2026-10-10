package cashflow_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
)

type family_ struct {
	ctx     context.Context
	space   uuid.UUID
	owner   uuid.UUID
	ops     *operation.Service
	cats    map[string]uuid.UUID
	report  *cashflow.Service
	md      *marketdata.Store
	account func(name string, typ account.Type, currency string, personal bool) uuid.UUID
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newFamily(t *testing.T) family_ {
	t.Helper()
	pool := testdb.New(t)
	ctx := t.Context()
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	catStore := category.NewStore(pool)
	list, err := catStore.List(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[string]uuid.UUID{}
	for _, c := range list {
		cats[string(c.Kind)+"/"+c.Name] = c.ID
	}
	accStore := account.NewStore(pool)
	opStore := operation.NewStore(pool)
	md := marketdata.NewStore(pool)
	return family_{
		ctx: ctx, space: sp.ID, owner: u.ID, ops: operation.NewService(opStore), cats: cats, md: md,
		report: cashflow.NewService(opStore, accStore, catStore, fam, marketdata.NewConverter(md)),
		account: func(name string, typ account.Type, currency string, personal bool) uuid.UUID {
			var owner *uuid.UUID
			if personal {
				owner = &u.ID
			}
			a, err := accStore.Create(ctx, sp.ID, owner, name, typ, currency, "")
			if err != nil {
				t.Fatal(err)
			}
			return a.ID
		},
	}
}

func (f family_) op(t *testing.T, accountID uuid.UUID, on string, typ operation.Type, amount int64, currency, cat string) {
	t.Helper()
	op := operation.Operation{AccountID: accountID, Type: typ, OccurredOn: day(t, on), AmountMinor: amount, Currency: currency}
	if cat != "" {
		id, ok := f.cats[cat]
		if !ok {
			t.Fatalf("no category %q", cat)
		}
		op.CategoryID = &id
	}
	if _, err := f.ops.Create(f.ctx, f.space, op); err != nil {
		t.Fatalf("%s %s %d: %v", on, typ, amount, err)
	}
}

// line finds a section's line by category, among the top level and children.
func line(lines []cashflow.Line, id uuid.UUID) *cashflow.Line {
	for i := range lines {
		if lines[i].CategoryID != nil && *lines[i].CategoryID == id {
			return &lines[i]
		}
		if l := line(lines[i].Children, id); l != nil {
			return l
		}
	}
	return nil
}

// Filed money is income and spending by category and month, children under
// their parent; unfiled money waits apart; a broker's unfiled money is the
// investments block; groups catch unfiled interest, fees and taxes.
func TestTheReportLaysMoneyOutByCategoryAndMonth(t *testing.T) {
	f := newFamily(t)
	card := f.account("Карта", account.TypeChecking, "RUB", false)
	broker := f.account("Брокер", account.TypeBrokerage, "RUB", false)

	f.op(t, card, "2026-08-05", operation.TypeDeposit, 180_000_00, "RUB", "income/Зарплата")
	f.op(t, card, "2026-08-10", operation.TypeWithdrawal, -640_00, "RUB", "expense/Такси")
	f.op(t, card, "2026-09-10", operation.TypeWithdrawal, -1_000_00, "RUB", "expense/Транспорт")
	f.op(t, card, "2026-09-11", operation.TypeWithdrawal, -4_300_00, "RUB", "expense/Продукты")
	f.op(t, card, "2026-09-12", operation.TypeWithdrawal, -15_000_00, "RUB", "")
	f.op(t, card, "2026-09-13", operation.TypeDeposit, 2_000_00, "RUB", "")
	f.op(t, card, "2026-09-14", operation.TypeFee, -99_00, "RUB", "")
	f.op(t, card, "2026-09-15", operation.TypeInterest, 350_00, "RUB", "")
	f.op(t, broker, "2026-08-01", operation.TypeDeposit, 50_000_00, "RUB", "")
	f.op(t, broker, "2026-09-01", operation.TypeWithdrawal, -10_000_00, "RUB", "")
	f.op(t, broker, "2026-09-02", operation.TypeWithdrawal, -20_000_00, "RUB", "expense/Путешествия")
	f.op(t, broker, "2026-09-03", operation.TypeFee, -50_00, "RUB", "")
	f.op(t, card, "2026-10-01", operation.TypeWithdrawal, -999_00, "RUB", "expense/Продукты") // after the period

	r, err := f.report.Report(f.ctx, f.space, day(t, "2026-08-01"), day(t, "2026-09-30"), cashflow.Whose{})
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseCurrency != "RUB" || len(r.Months) != 2 {
		t.Fatalf("report frame: %s, %d months", r.BaseCurrency, len(r.Months))
	}
	if r.Income.Total != 180_350_00 || !slices.Equal(r.Income.ByMonth, []int64{180_000_00, 350_00}) {
		t.Errorf("income = %d %v", r.Income.Total, r.Income.ByMonth)
	}
	// Taxi + transport + groceries + the trip from the broker + the card's fee.
	if want := int64(640_00 + 1_000_00 + 4_300_00 + 20_000_00 + 99_00); r.Expense.Total != want {
		t.Errorf("spending = %d, want %d", r.Expense.Total, want)
	}
	transport := line(r.Expense.Lines, f.cats["expense/Транспорт"])
	if transport == nil || transport.Total != 1_640_00 || transport.Direct.Total != 1_000_00 ||
		len(transport.Children) != 1 || transport.Children[0].Total != 640_00 {
		t.Errorf("Транспорт = %+v", transport)
	}
	if !slices.Equal(transport.ByMonth, []int64{640_00, 1_000_00}) {
		t.Errorf("Транспорт by month = %v", transport.ByMonth)
	}
	last := r.Expense.Lines[len(r.Expense.Lines)-1]
	if last.Group != cashflow.GroupFee || last.Total != 99_00 {
		t.Errorf("the last spending line = %+v, want the card's fee group", last)
	}
	if r.UnfiledIn.Total != 2_000_00 || r.UnfiledOut.Total != 15_000_00 {
		t.Errorf("unfiled = in %d, out %d", r.UnfiledIn.Total, r.UnfiledOut.Total)
	}
	inv := r.Investments
	if inv.Deposited.Total != 50_000_00 || inv.Withdrawn.Total != 10_000_00 || inv.Costs.Total != 50_00 {
		t.Errorf("investments = %+v", inv)
	}
}

// A foreign row is counted at its day's rate; a currency with no rate at all
// is left out and named.
func TestTheReportConvertsAtTheRowsDay(t *testing.T) {
	f := newFamily(t)
	usd := f.account("Доллары", account.TypeChecking, "USD", false)
	kzt := f.account("Тенге", account.TypeCash, "KZT", false)
	if err := f.md.UpsertFxRates(f.ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: day(t, "2026-09-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: day(t, "2026-09-10"), Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	f.op(t, usd, "2026-09-05", operation.TypeWithdrawal, -10_00, "USD", "expense/Кафе и рестораны")
	f.op(t, usd, "2026-09-12", operation.TypeWithdrawal, -10_00, "USD", "expense/Кафе и рестораны")
	f.op(t, kzt, "2026-09-12", operation.TypeWithdrawal, -5_000_00, "KZT", "expense/Продукты")

	r, err := f.report.Report(f.ctx, f.space, day(t, "2026-09-01"), day(t, "2026-09-30"), cashflow.Whose{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Expense.Total != 900_00+1_000_00 {
		t.Errorf("spending = %d, want 10 $ at 90 and 10 $ at 100", r.Expense.Total)
	}
	if !slices.Equal(r.MissingRates, []string{"KZT"}) || r.LeftOut != 1 {
		t.Errorf("left out = %v, %d", r.MissingRates, r.LeftOut)
	}
}

// A member's report reads their personal accounts, the shared one the rest.
func TestTheReportNarrowsToWhoseAccounts(t *testing.T) {
	f := newFamily(t)
	mine := f.account("Моя карта", account.TypeChecking, "RUB", true)
	ours := f.account("Общая карта", account.TypeChecking, "RUB", false)
	f.op(t, mine, "2026-09-01", operation.TypeWithdrawal, -100_00, "RUB", "expense/Продукты")
	f.op(t, ours, "2026-09-01", operation.TypeWithdrawal, -300_00, "RUB", "expense/Продукты")

	for name, c := range map[string]struct {
		whose cashflow.Whose
		want  int64
	}{
		"the family": {cashflow.Whose{}, 400_00},
		"mine":       {cashflow.Whose{UserID: &f.owner}, 100_00},
		"shared":     {cashflow.Whose{Shared: true}, 300_00},
	} {
		r, err := f.report.Report(f.ctx, f.space, day(t, "2026-09-01"), day(t, "2026-09-30"), c.whose)
		if err != nil {
			t.Fatal(err)
		}
		if r.Expense.Total != c.want {
			t.Errorf("%s: spending = %d, want %d", name, r.Expense.Total, c.want)
		}
	}
}

// A period that ends before it starts, or runs past ten years, is refused.
func TestTheReportRefusesABadPeriod(t *testing.T) {
	f := newFamily(t)
	for _, p := range [][2]string{{"2026-09-30", "2026-09-01"}, {"2010-01-01", "2026-09-30"}} {
		if _, err := f.report.Report(f.ctx, f.space, day(t, p[0]), day(t, p[1]), cashflow.Whose{}); err == nil {
			t.Errorf("%s..%s was accepted", p[0], p[1])
		}
	}
}

// A row naming a member is theirs, wherever it sits; a row naming nobody is
// the account owner's, the family's on a shared account.
func TestARowNamingAMemberIsTheirs(t *testing.T) {
	f := newFamily(t)
	partner := uuid.New()
	ours := f.account("Общая карта", account.TypeChecking, "RUB", false)
	mine := f.account("Моя карта", account.TypeChecking, "RUB", true)
	f.op(t, ours, "2026-09-01", operation.TypeWithdrawal, -300_00, "RUB", "expense/Продукты")
	f.op(t, mine, "2026-09-01", operation.TypeWithdrawal, -100_00, "RUB", "expense/Продукты")
	named := func(memberID uuid.UUID) {
		op := operation.Operation{
			AccountID: ours, Type: operation.TypeWithdrawal, OccurredOn: day(t, "2026-09-02"),
			AmountMinor: -50_00, Currency: "RUB", MemberID: &memberID,
		}
		if _, err := f.ops.Create(f.ctx, f.space, op); err != nil {
			t.Fatalf("a row naming %s: %v", memberID, err)
		}
	}
	named(f.owner)
	// Only a member of the family can be named.
	op := operation.Operation{
		AccountID: ours, Type: operation.TypeWithdrawal, OccurredOn: day(t, "2026-09-02"),
		AmountMinor: -50_00, Currency: "RUB", MemberID: &partner,
	}
	if _, err := f.ops.Create(f.ctx, f.space, op); err == nil {
		t.Error("a stranger was named on a row")
	}

	for name, c := range map[string]struct {
		whose cashflow.Whose
		want  int64
	}{
		"mine":   {cashflow.Whose{UserID: &f.owner}, 100_00 + 50_00},
		"shared": {cashflow.Whose{Shared: true}, 300_00},
	} {
		r, err := f.report.Report(f.ctx, f.space, day(t, "2026-09-01"), day(t, "2026-09-30"), c.whose)
		if err != nil {
			t.Fatal(err)
		}
		// The named row is unfiled: it counts in «не разнесено», not spending.
		got := r.Expense.Total + r.UnfiledOut.Total
		if got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}
