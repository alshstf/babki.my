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

// ВТБ's minimum (#459): due by the 20th, the first after the statement; 3% of
// the debt rounded up to 100.
func TestTheMinimumIsDueByADayOfTheMonth(t *testing.T) {
	terms := vtb110
	terms.PayDay, terms.MinRoundUp = 20, 100_00
	st := Work(terms, []operation.Operation{spend("2026-09-10", 10_150)}, "RUB", d("2026-10-05"), Kinds{})
	if st.Minimum != 400_00 || day(st.MinimumOn) != "2026-10-20" {
		t.Errorf("minimum = %d by %s, want 304,50 rounded up to 400 by 20.10", st.Minimum, day(st.MinimumOn))
	}
	// A statement on the 25th: due by the 20th of the next month; rounding
	// never asks more than the debt.
	terms.StatementDay = 25
	st = Work(terms, []operation.Operation{spend("2026-09-10", 50)}, "RUB", d("2026-09-26"), Kinds{})
	if day(st.MinimumOn) != "2026-10-20" || st.Minimum != 50_00 {
		t.Errorf("minimum = %d by %s, want the whole 50 by 20.10", st.Minimum, day(st.MinimumOn))
	}
	both := terms
	both.PayByPeriodEnd = true
	if both.Validate() == nil {
		t.Error("a day of the month and the period's end at once were accepted")
	}
}

// Т-Банк (#461): a minimum missed takes the grace off the purchases of the
// period it was due in, not the rest.
func TestAMissedMinimumTakesOnlyItsPeriodsGrace(t *testing.T) {
	tbank := alfa
	tbank.MissedMinimumPeriod = true
	// September's statement (1 October) is due on the 21st and missed; the
	// purchases of October — on the November statement — lose their grace,
	// November's keep it.
	ops := []operation.Operation{
		spend("2026-09-05", 10_000), spend("2026-10-05", 2_000), spend("2026-10-25", 1_000), spend("2026-11-03", 500),
	}
	st := Work(tbank, ops, "RUB", d("2026-11-05"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2026-12-21"] != 500_00 {
		t.Errorf("grace = %v, want November's 500 only", got)
	}
	var lostOctober int64
	for _, l := range st.Lost {
		if day(l.From) == "2026-10-01" {
			lostOctober = l.Amount
		}
	}
	if lostOctober != 3_000_00 {
		t.Errorf("lost = %+v, want October's 3 000 among them", st.Lost)
	}
}

// Альфа's «Автопродление периода без %» (#473): the 60 days of a running
// grace go on to 150 when the purchases are not repaid by the 60th, for
// 1.9 % of the purchases' debt a month beyond the 60 — the first extension
// free. Paid by the 60th, nothing is charged; past the 150th, the grace is
// off as without the extension.
func TestARunningGraceIsExtendedForAFee(t *testing.T) {
	alfa := Terms{
		Limit: 300_000_00, StatementDay: 10, PaymentDays: 20, GraceKind: Running, GraceDays: 60, RunFrom: FromNextDay,
		MinPercent: decimal.NewFromInt(3), AnnualRate: decimal.RequireFromString("39.99"),
		ExtendDays: 150, ExtendPercent: decimal.RequireFromString("1.9"),
	}
	ops := []operation.Operation{spend("2026-09-10", 10_000)}

	st := Work(alfa, ops, "RUB", d("2026-10-01"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2026-11-09"] != 10_000_00 {
		t.Errorf("grace = %v, want 10 000 by 09.11, the 60th day", got)
	}
	if e := st.Extension; e == nil || day(e.Until) != "2027-02-07" || e.Active || e.MonthlyFee != 190_00 {
		t.Errorf("extension = %+v, want until 07.02 for 190 a month, not yet running", e)
	}

	// Past the 60th day unpaid: due by the 150th, the extension running.
	st = Work(alfa, ops, "RUB", d("2026-11-20"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2027-02-07"] != 10_000_00 || len(st.Lost) != 0 || !st.GraceOffSince.IsZero() {
		t.Errorf("extended: grace %v, lost %+v, off since %s", got, st.Lost, day(st.GraceOffSince))
	}
	if e := st.Extension; e == nil || !e.Active {
		t.Errorf("extension = %+v, want running", e)
	}

	// The first extension: free.
	free := alfa
	free.ExtendFree = true
	if e := Work(free, ops, "RUB", d("2026-11-20"), Kinds{}).Extension; e == nil || e.MonthlyFee != 0 || !e.Free {
		t.Errorf("a free extension = %+v", e)
	}

	// Past the 150th: the grace is off the whole debt.
	st = Work(alfa, ops, "RUB", d("2027-02-10"), Kinds{})
	if day(st.GraceOffSince) != "2027-02-08" || len(st.Grace) != 0 || st.Extension != nil || len(st.Lost) != 1 || day(st.Lost[0].Deadline) != "2027-02-07" {
		t.Errorf("past the extension: off since %s, grace %v, extension %+v, lost %+v", day(st.GraceOffSince), dues(st), st.Extension, st.Lost)
	}

	// No extension past the grace's own days, nor for another kind of grace.
	bad := alfa
	bad.ExtendDays = 60
	if err := bad.Validate(); err == nil {
		t.Error("an extension of no days more was accepted")
	}
	bad = vtb110
	bad.GraceKind, bad.ExtendDays = Long, 150
	if err := bad.Validate(); err == nil {
		t.Error("an extension of a long grace was accepted")
	}
}
