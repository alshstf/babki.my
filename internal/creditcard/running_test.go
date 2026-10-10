package creditcard

import (
	"slices"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// ВТБ «Карта возможностей» (#457): one grace of 110 days from the 1st of the
// first purchase's month, for every purchase while the card is in debt; a new
// one only once the debt is repaid in full; a deadline missed takes it off
// the whole debt.
var vtb110 = Terms{
	Limit: 300_000_00, StatementDay: 1, PaymentDays: 19, GraceKind: Running, GraceDays: 110, RunFrom: FromMonthStart,
	MinPercent: decimal.NewFromInt(3), MinFloor: 0, AnnualRate: decimal.RequireFromString("49.9"),
}

func TestARunningGraceIsSharedUntilTheDebtIsRepaid(t *testing.T) {
	// Paid at each month's 20th: the minimum; September's purchase opens the
	// grace on the 1st of September, so November's is due by the 19th of
	// December too.
	ops := []operation.Operation{
		spend("2026-09-10", 10_000), repay("2026-10-15", 300), repay("2026-11-15", 300), spend("2026-11-25", 5_000),
	}
	st := Work(vtb110, ops, "RUB", d("2026-11-26"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2026-12-19"] != 14_400_00 {
		t.Errorf("grace = %v, want everything by 19.12", got)
	}

	// Repaid in full on the 10th of December: the purchase of the 15th opens
	// a new grace from the 1st of December, to the 20th of March.
	again := append(append([]operation.Operation{}, ops...), repay("2026-12-10", 14_400), spend("2026-12-15", 2_000))
	st = Work(vtb110, again, "RUB", d("2026-12-16"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2027-03-20"] != 2_000_00 {
		t.Errorf("after full repayment: grace = %v, want 2 000 by 20.03", got)
	}

	// Not repaid by the 19th: the grace is off the whole debt from the 20th,
	// the purchase of the 22nd charged from its day, until it is all repaid.
	late := append(append([]operation.Operation{}, ops...), repay("2026-12-15", 300), spend("2026-12-22", 1_000))
	st = Work(vtb110, late, "RUB", d("2026-12-23"), Kinds{})
	if day(st.GraceOffSince) != "2026-12-20" || st.ToRestore != 15_100_00 || len(st.Grace) != 0 {
		t.Errorf("missed: off since %s, to restore %d, grace %v", day(st.GraceOffSince), st.ToRestore, dues(st))
	}
	if len(st.Lost) != 2 || st.Lost[0].Early || day(st.Lost[0].From) != "2026-09-01" || day(st.Lost[0].To) != "2026-12-19" ||
		!st.Lost[1].Early || st.Lost[1].Amount != 1_000_00 {
		t.Errorf("lost = %+v", st.Lost)
	}
	// Repaid in full, the next purchase has a grace again.
	back := slices.Concat(late, []operation.Operation{repay("2026-12-28", 15_100), spend("2027-01-05", 700)})
	st = Work(vtb110, back, "RUB", d("2027-01-06"), Kinds{})
	if !st.GraceOffSince.IsZero() || len(st.Grace) != 1 || dues(st)["2027-04-20"] != 700_00 {
		t.Errorf("back: off since %s, grace %v", day(st.GraceOffSince), dues(st))
	}

	// Counted from the purchase's own day, or the day after.
	from := vtb110
	from.RunFrom = FromNextDay
	if got := dues(Work(from, ops[:1], "RUB", d("2026-09-11"), Kinds{})); got["2026-12-29"] != 10_000_00 {
		t.Errorf("from the next day: %v, want 11.09 + 109 days = 29.12", got)
	}
	bad := vtb110
	bad.RunFrom = ""
	if bad.Validate() == nil {
		t.Error("a running grace with no start was accepted")
	}
}
