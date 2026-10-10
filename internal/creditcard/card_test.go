package creditcard

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The card of decision Р-26's example: a limit of 150 000, statements on the
// 1st, 20 days to pay, a minimum of 3% and not less than 300, 39.9% a year
// once the grace is lost.
var alfa = Terms{
	Limit: 150_000_00, StatementDay: 1, PaymentDays: 20, GraceKind: FromStatement,
	MinPercent: decimal.NewFromInt(3), MinFloor: 300_00, AnnualRate: decimal.RequireFromString("39.9"),
}

func spend(on string, rub int64) operation.Operation {
	return operation.Operation{Type: operation.TypeWithdrawal, OccurredOn: d(on), AmountMinor: -rub * 100, Currency: "RUB"}
}

func repay(on string, rub int64) operation.Operation {
	return operation.Operation{Type: operation.TypeDeposit, OccurredOn: d(on), AmountMinor: rub * 100, Currency: "RUB"}
}

func dues(st Status) map[string]int64 {
	out := map[string]int64{}
	for _, g := range st.Grace {
		out[g.On.Format(time.DateOnly)] = g.Amount
	}
	return out
}

// September's purchases are free until the 21st of October — the statement on
// the 1st and its 20 days; October's until the 21st of November. A payment
// clears the oldest first and counts toward the minimum.
func TestPurchasesStayFreeUntilTheirStatementIsPaid(t *testing.T) {
	ops := []operation.Operation{spend("2026-09-03", 30_000), spend("2026-09-20", 22_300), spend("2026-10-05", 5_000)}

	st := Work(alfa, ops, "RUB", d("2026-10-07"), Kinds{})
	if st.Debt != 57_300_00 || st.Available != 92_700_00 {
		t.Errorf("debt %d available %d, want 57 300 and 92 700", st.Debt, st.Available)
	}
	if got := dues(st); len(got) != 2 || got["2026-10-21"] != 52_300_00 || got["2026-11-21"] != 5_000_00 {
		t.Errorf("grace = %v", got)
	}
	if st.Minimum != 1_569_00 || st.MinimumOn.Format(time.DateOnly) != "2026-10-21" || st.MinimumMissed || st.MinimumEstimate {
		t.Errorf("minimum = %d by %s (missed %v, estimate %v), want 1 569 by 21.10", st.Minimum, st.MinimumOn.Format(time.DateOnly), st.MinimumMissed, st.MinimumEstimate)
	}

	st = Work(alfa, append(ops, repay("2026-10-08", 10_000)), "RUB", d("2026-10-10"), Kinds{})
	if got := dues(st); got["2026-10-21"] != 42_300_00 || got["2026-11-21"] != 5_000_00 {
		t.Errorf("after paying 10 000, grace = %v", got)
	}
	if st.Minimum != 0 {
		t.Errorf("the minimum was paid, %d left", st.Minimum)
	}
}

// Past the 21st with September still owed: its grace is lost, and the interest
// runs from each purchase's day. Cash moved off the card owes interest from
// day one; once the minimum's day has passed paid, the next one is estimated.
func TestAMissedDeadlineLosesTheGrace(t *testing.T) {
	moved := uuid.New()
	ops := []operation.Operation{
		spend("2026-09-03", 30_000), spend("2026-09-20", 22_300), repay("2026-10-08", 10_000),
		{Type: operation.TypeWithdrawal, OccurredOn: d("2026-10-02"), AmountMinor: -1_000_00, Currency: "RUB", TransferGroupID: &moved},
	}
	st := Work(alfa, ops, "RUB", d("2026-10-25"), Kinds{})
	if len(st.Lost) != 1 || st.Lost[0].Amount != 42_300_00 || st.Lost[0].From.Format(time.DateOnly) != "2026-09-01" ||
		st.Lost[0].To.Format(time.DateOnly) != "2026-09-30" {
		t.Fatalf("lost = %+v", st.Lost)
	}
	// 20 000 left of the 3rd's 30 000 for 52 days, 22 300 of the 20th's for 35.
	if want := int64(2_000_000*399*52/365000 + 2_230_000*399*35/365000); abs(st.Lost[0].Interest-want) > 2 {
		t.Errorf("interest = %d, want about %d", st.Lost[0].Interest, want)
	}
	if st.NonGrace != 1_000_00 || st.NonGraceInterest != 25_14 {
		t.Errorf("money moved off: %d with %d interest", st.NonGrace, st.NonGraceInterest)
	}
	if len(st.Grace) != 0 {
		t.Errorf("nothing left in grace: %v", st.Grace)
	}
	if !st.MinimumEstimate || st.MinimumOn.Format(time.DateOnly) != "2026-11-21" || st.Minimum != 1_299_00 {
		t.Errorf("next minimum = %d by %s (estimate %v)", st.Minimum, st.MinimumOn.Format(time.DateOnly), st.MinimumEstimate)
	}
}

// A long grace (СберКарта: 120 days from the start of each month) gives
// September's purchases until the 29th of December; the 31st of a short month
// is its last day.
func TestALongGraceRunsFromThePeriodsStart(t *testing.T) {
	sber := alfa
	sber.GraceKind, sber.GraceDays = Long, 120
	st := Work(sber, []operation.Operation{spend("2026-09-03", 1_000)}, "RUB", d("2026-10-10"), Kinds{})
	if got := dues(st); got["2026-12-29"] != 1_000_00 {
		t.Errorf("grace = %v", got)
	}
	end := alfa
	end.StatementDay = 31
	if from, to := end.period(d("2026-02-15")); from.Format(time.DateOnly) != "2026-01-31" || to.Format(time.DateOnly) != "2026-02-28" {
		t.Errorf("period = %s..%s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
}

// More paid in than owed is the family's own money on the card: the next
// purchase uses it first.
func TestAnOverpaymentCoversTheNextPurchase(t *testing.T) {
	st := Work(alfa, []operation.Operation{repay("2026-09-01", 5_000), spend("2026-09-10", 3_000)}, "RUB", d("2026-09-15"), Kinds{})
	if st.Debt != -2_000_00 || st.Available != 150_000_00 || len(st.Grace) != 0 || st.Minimum != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestACardKnownByItsBalance(t *testing.T) {
	st := ByBalance(alfa, -61_500_00, d("2026-10-10"))
	if st.Debt != 61_500_00 || st.Available != 88_500_00 || st.Minimum != 1_845_00 || !st.MinimumEstimate ||
		st.MinimumOn.Format(time.DateOnly) != "2026-10-21" {
		t.Errorf("status = %+v", st)
	}
}

func TestTermsAreChecked(t *testing.T) {
	bad := []func(*Terms){
		func(x *Terms) { x.StatementDay = 0 },
		func(x *Terms) { x.PaymentDays = 61 },
		func(x *Terms) { x.GraceKind = "x" },
		func(x *Terms) { x.GraceKind, x.GraceDays = Long, 0 },
		func(x *Terms) { x.MinPercent = decimal.NewFromInt(101) },
		func(x *Terms) { x.Limit = -1 },
		func(x *Terms) { r := decimal.NewFromInt(-1); x.OwnRate = &r },
	}
	for i, f := range bad {
		x := alfa
		f(&x)
		if x.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := alfa.Validate(); err != nil {
		t.Error(err)
	}
}

// A month of 30 000 on the card while own money earns 15% is about 370
// roubles; the cashback is added and the bank's fee taken away. A charge filed
// under «Проценты по кредитам» is a cost whatever its type.
func TestTheCardIsWeighedAgainstOwnMoney(t *testing.T) {
	own := decimal.NewFromInt(15)
	terms := alfa
	terms.OwnRate = &own
	cashbackCat, interestCat := uuid.New(), uuid.New()
	kinds := Kinds{Cashback: map[uuid.UUID]bool{cashbackCat: true}, Charges: map[uuid.UUID]bool{interestCat: true}}
	ops := []operation.Operation{
		spend("2026-09-01", 30_000),
		{Type: operation.TypeDeposit, OccurredOn: d("2026-09-15"), AmountMinor: 300_00, Currency: "RUB", CategoryID: &cashbackCat},
		{Type: operation.TypeFee, OccurredOn: d("2026-09-20"), AmountMinor: -99_00, Currency: "RUB"},
		{Type: operation.TypeWithdrawal, OccurredOn: d("2026-09-25"), AmountMinor: -10_00, Currency: "RUB", CategoryID: &interestCat},
		repay("2026-10-01", 30_109),
	}
	b := Weigh(terms, ops, "RUB", d("2026-10-10"), kinds)
	if b.From.Format(time.DateOnly) != "2026-09-01" || !b.OwnRateKnown {
		t.Errorf("benefit = %+v", b)
	}
	// 30 000 for the 1st to the 14th, 29 700 to the 19th, 29 799 to the 24th,
	// 29 809 to the 30th: debt-days at 15% a year.
	days := int64(30_000_00*14 + 29_700_00*5 + 29_799_00*5 + 29_809_00*6)
	if want := days * 15 / 36500; abs(b.OwnEarned-want) > 1 {
		t.Errorf("own money earned %d, want about %d", b.OwnEarned, want)
	}
	if b.Cashback != 300_00 || b.Costs != 109_00 || b.Total != b.OwnEarned+300_00-109_00 {
		t.Errorf("cashback %d costs %d total %d", b.Cashback, b.Costs, b.Total)
	}

	terms.OwnRate = nil
	if b := Weigh(terms, ops, "RUB", d("2026-10-10"), kinds); b.OwnRateKnown || b.OwnEarned != 0 || b.Total != 191_00 {
		t.Errorf("without the own rate: %+v", b)
	}
}
