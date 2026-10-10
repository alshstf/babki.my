package creditcard

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// A purchase in installments (decision Р-33): equal parts, one with each
// statement after it, its fee on top; the parts in the minimum, a payment
// going to them first; the purchase kept apart from the grace.
func TestInstallmentsAreShownPartByPart(t *testing.T) {
	phone := spend("2026-09-10", 12_000)
	phone.ID = uuid.New()
	kinds := Kinds{Installments: map[uuid.UUID]Plan{phone.ID: {Months: 3, MonthlyFeePercent: decimal.NewFromInt(4)}}}
	ops := []operation.Operation{phone, spend("2026-09-12", 2_000)}

	// The statement of the 1st of October shows the first part: 4 000 and
	// 4% of 12 000. The minimum: 3% of the 2 000 (so 300, the floor) and it.
	st := Work(alfa, ops, "RUB", d("2026-10-05"), kinds)
	if st.Minimum != 300_00+4_480_00 || st.InstallmentsDue != 4_480_00 {
		t.Errorf("minimum %d, parts due %d; want 300 + 4 480", st.Minimum, st.InstallmentsDue)
	}
	if got := dues(st); len(got) != 1 || got["2026-10-21"] != 2_000_00 {
		t.Errorf("grace = %v, want only the 2 000 bought outright", got)
	}
	if len(st.Installments) != 1 || st.Installments[0].Billed != 1 || st.Installments[0].Left != 8_000_00 || st.Installments[0].Next != 4_480_00 {
		t.Errorf("installments = %+v", st.Installments)
	}
	if st.Debt != 2_000_00+8_000_00+4_480_00 {
		t.Errorf("debt = %d, want the purchase, the sum left and the part shown", st.Debt)
	}

	// 4 480 paid: it goes to the part, the purchase bought outright is still
	// owed in full.
	st = Work(alfa, slices.Concat(ops, []operation.Operation{repay("2026-10-15", 4_480)}), "RUB", d("2026-10-16"), kinds)
	if st.InstallmentsDue != 0 || dues(st)["2026-10-21"] != 2_000_00 || st.Minimum != 300_00 {
		t.Errorf("after paying the part: due %d, grace %v, minimum %d", st.InstallmentsDue, dues(st), st.Minimum)
	}

	// Three statements on, it is all shown.
	paid := slices.Concat(ops, []operation.Operation{repay("2026-10-15", 6_480), repay("2026-11-15", 4_480), repay("2026-12-15", 4_480)})
	st = Work(alfa, paid, "RUB", d("2027-01-05"), kinds)
	if len(st.Installments) != 0 || st.Debt != 0 {
		t.Errorf("all paid: installments %+v, debt %d", st.Installments, st.Debt)
	}
}

// A card of installments («Халва»): every purchase in installments for 3
// months, 99 with the first part; the payment is the parts.
func TestACardOfInstallments(t *testing.T) {
	halva := Terms{
		Limit: 100_000_00, StatementDay: 1, PaymentDays: 15, GraceKind: FromStatement,
		AnnualRate: decimal.Zero, MinPercent: decimal.Zero, Installment: Plan{Months: 3, Fee: 99_00},
	}
	st := Work(halva, []operation.Operation{spend("2026-09-10", 1_000)}, "RUB", d("2026-10-02"), Kinds{})
	// 1 000 in three: 333,33, 333,33 and 333,34.
	if st.Minimum != 333_33+99_00 || len(st.Grace) != 0 || day(st.MinimumOn) != "2026-10-16" {
		t.Errorf("minimum %d by %s, grace %v; want 432,33 by 16.10", st.Minimum, day(st.MinimumOn), dues(st))
	}
	st = Work(halva, []operation.Operation{spend("2026-09-10", 1_000), repay("2026-10-10", 433), repay("2026-11-10", 334), repay("2026-12-10", 334)}, "RUB", d("2027-01-02"), Kinds{})
	if st.Debt > 0 || len(st.Installments) != 0 {
		t.Errorf("the last part takes the rounding: debt %d, %+v", st.Debt, st.Installments)
	}
	if (Plan{Months: 61}).Validate() == nil {
		t.Error("61 months were accepted")
	}
}
