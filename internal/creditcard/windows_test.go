package creditcard

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// Газпромбанк's «180 дней Премиум» (decision Р-28, #446), a contract made in
// July: periods are calendar months; two months of purchases are free until
// the end of the sixth, counting from the 1st of July; the minimum — 3% of the
// debt, not less than 500, plus the interest and fees in full — is due by the
// end of the next month; a deadline missed takes the grace off everything.
func gpb180() Terms {
	opened := d("2026-07-10")
	return Terms{
		Limit: 300_000_00, StatementDay: 1, GraceKind: Windows, WindowMonths: 2, GraceMonths: 6, OpenedOn: &opened,
		GraceAllLost: true, PayByPeriodEnd: true, ChargesInFull: true,
		MinPercent: decimal.NewFromInt(3), MinFloor: 500_00, AnnualRate: decimal.RequireFromString("59.99"),
	}
}

func day(t time.Time) string { return t.Format(time.DateOnly) }

// The bank's own example: July and August's purchases are paid by the 31st of
// December, September and October's by the 28th of February; with the first
// window paid in time, the second keeps its grace.
func TestWindowsOfPurchasesShareADeadline(t *testing.T) {
	gpb := gpb180()
	ops := []operation.Operation{
		spend("2026-07-03", 5_000), spend("2026-08-25", 25_000), spend("2026-09-05", 3_000), spend("2026-10-20", 12_000),
		// Each statement's minimum, by the end of the next month: 3% of
		// 5 000 is less than 500; then of 29 500, 31 615 and 42 665.
		repay("2026-08-30", 500), repay("2026-09-30", 885), repay("2026-10-30", 950), repay("2026-11-28", 1_280),
	}
	st := Work(gpb, ops, "RUB", d("2026-11-29"), Kinds{})
	if got := dues(st); len(got) != 2 || got["2026-12-31"] != 26_385_00 || got["2027-02-28"] != 15_000_00 {
		t.Errorf("grace = %v, want what is left of 30 000 by 31.12 and 15 000 by 28.02", got)
	}
	if !st.GraceOffSince.IsZero() || len(st.Lost) != 0 {
		t.Errorf("the grace is off since %s, lost %+v", day(st.GraceOffSince), st.Lost)
	}
	if st.Minimum != 0 || day(st.MinimumOn) != "2026-11-30" || st.MinimumMissed {
		t.Errorf("minimum = %d by %s (missed %v), want November's paid by 30.11", st.Minimum, day(st.MinimumOn), st.MinimumMissed)
	}

	paid := slices.Concat(ops, []operation.Operation{repay("2026-12-20", 26_385)})
	st = Work(gpb, paid, "RUB", d("2027-01-10"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2027-02-28"] != 15_000_00 {
		t.Errorf("after the first window repaid: grace = %v", got)
	}
	if !st.GraceOffSince.IsZero() || len(st.Lost) != 0 {
		t.Errorf("the grace is off since %s, lost %+v", day(st.GraceOffSince), st.Lost)
	}
	if st.Minimum != 500_00 || day(st.MinimumOn) != "2027-01-31" {
		t.Errorf("January's minimum = %d by %s, want 500 (3%% of 15 000 is less) by 31.01", st.Minimum, day(st.MinimumOn))
	}
}

// The first window part-paid by its deadline: the grace goes off the whole
// debt — the second window's purchases too, and those made after — until the
// purchases are repaid in full; the purchases from the next day have it again.
func TestAMissedWindowTakesTheGraceOffEverything(t *testing.T) {
	gpb := gpb180()
	ops := []operation.Operation{
		spend("2026-07-03", 5_000), spend("2026-08-25", 25_000), spend("2026-09-05", 3_000), spend("2026-10-20", 12_000),
		repay("2026-08-30", 500), repay("2026-09-30", 885), repay("2026-10-30", 950), repay("2026-11-28", 1_280),
		repay("2026-12-20", 20_000), spend("2027-01-05", 1_000),
	}
	st := Work(gpb, ops, "RUB", d("2027-01-10"), Kinds{})
	if day(st.GraceOffSince) != "2027-01-01" || st.GraceOffByMinimum || st.ToRestore != 22_385_00 {
		t.Errorf("grace off since %s (by minimum %v), to restore %d; want since 01.01 and 22 385", day(st.GraceOffSince), st.GraceOffByMinimum, st.ToRestore)
	}
	if len(st.Grace) != 0 {
		t.Errorf("grace = %v, want none left", dues(st))
	}
	if len(st.Lost) != 3 || st.Lost[0].Early || !st.Lost[1].Early || !st.Lost[2].Early ||
		st.Lost[0].Amount != 6_385_00 || st.Lost[1].Amount != 15_000_00 || st.Lost[2].Amount != 1_000_00 ||
		day(st.Lost[1].From) != "2026-09-01" || day(st.Lost[1].To) != "2026-10-31" {
		t.Errorf("lost = %+v", st.Lost)
	}
	if st.Lost[0].Interest <= 0 || st.Lost[1].Interest <= st.Lost[0].Interest/10 {
		t.Errorf("the interest on what was lost is not counted: %+v", st.Lost)
	}

	// Repaid in full on the 12th: a purchase on the 12th is still charged,
	// one on the 13th is free until the end of June (window January–February).
	again := slices.Concat(ops, []operation.Operation{repay("2027-01-12", 22_385), spend("2027-01-12", 300), spend("2027-01-13", 700)})
	st = Work(gpb, again, "RUB", d("2027-01-14"), Kinds{})
	if !st.GraceOffSince.IsZero() {
		t.Errorf("the grace is still off since %s", day(st.GraceOffSince))
	}
	if got := dues(st); len(got) != 1 || got["2027-06-30"] != 700_00 {
		t.Errorf("grace = %v, want the 13th's 700 by 30.06", got)
	}
	if len(st.Lost) != 1 || st.Lost[0].Amount != 300_00 || !st.Lost[0].Early {
		t.Errorf("lost = %+v, want the 12th's 300 charged", st.Lost)
	}

	// Without the rule, only the window that missed is lost.
	lenient := gpb
	lenient.GraceAllLost = false
	st = Work(lenient, ops, "RUB", d("2027-01-10"), Kinds{})
	if got := dues(st); len(got) != 2 || got["2027-02-28"] != 15_000_00 || got["2027-06-30"] != 1_000_00 || len(st.Lost) != 1 {
		t.Errorf("lenient: grace = %v, lost %+v", got, st.Lost)
	}
}

// A minimum missed takes the grace off too, and only the whole debt repaid
// brings it back.
func TestAMissedMinimumTakesTheGraceOff(t *testing.T) {
	gpb := gpb180()
	ops := []operation.Operation{spend("2026-07-03", 10_000), repay("2026-09-15", 2_000)}
	st := Work(gpb, ops, "RUB", d("2026-09-20"), Kinds{})
	// August's minimum (3% of 10 000, so 500) was due by the 31st of August.
	if day(st.GraceOffSince) != "2026-09-01" || !st.GraceOffByMinimum || st.ToRestore != 8_000_00 {
		t.Errorf("grace off since %s (by minimum %v), to restore %d", day(st.GraceOffSince), st.GraceOffByMinimum, st.ToRestore)
	}
	if len(st.Lost) != 1 || !st.Lost[0].Early {
		t.Errorf("lost = %+v", st.Lost)
	}
}

// The minimum: 3% of the debt for purchases, not less than 500, and the
// interest and fees charged in full; due by the end of the month.
func TestTheMinimumTakesTheChargesInFull(t *testing.T) {
	gpb := gpb180()
	fees := uuid.New()
	kinds := Kinds{Charges: map[uuid.UUID]bool{fees: true}}
	fee := spend("2026-07-31", 590)
	fee.CategoryID = &fees
	interest := operation.Operation{Type: operation.TypeInterest, OccurredOn: d("2026-07-31"), AmountMinor: -1_200_00, Currency: "RUB"}
	ops := []operation.Operation{spend("2026-07-03", 40_000), fee, interest}
	st := Work(gpb, ops, "RUB", d("2026-08-05"), kinds)
	// 3% of 40 000 is 1 200; with 590 and 1 200 charged: 2 990, by the 31st.
	if st.Minimum != 2_990_00 || day(st.MinimumOn) != "2026-08-31" || st.MinimumEstimate {
		t.Errorf("minimum = %d by %s (estimate %v), want 2 990 by 31.08", st.Minimum, day(st.MinimumOn), st.MinimumEstimate)
	}
	// The fee is the bank's, not a purchase the grace covers.
	if got := dues(st); len(got) != 1 || got["2026-12-31"] != 40_000_00 {
		t.Errorf("grace = %v, want the 40 000 only", got)
	}
	plain := gpb
	plain.ChargesInFull = false
	if st := Work(plain, ops, "RUB", d("2026-08-05"), kinds); st.Minimum != 1_253_70 {
		t.Errorf("without the charges in full: minimum = %d, want 3%% of 41 790", st.Minimum)
	}
}

func TestWindowsCountFromTheContractsMonth(t *testing.T) {
	gpb := gpb180()
	for _, c := range []struct{ on, from, deadline string }{
		{"2026-07-01", "2026-07-01", "2026-12-31"},
		{"2026-08-31", "2026-07-01", "2026-12-31"},
		{"2026-09-01", "2026-09-01", "2027-02-28"},
		{"2027-01-15", "2027-01-01", "2027-06-30"},
		// Before the contract's month, should a row predate it.
		{"2026-06-15", "2026-05-01", "2026-10-31"},
	} {
		from, _ := gpb.group(d(c.on))
		if day(from) != c.from || day(gpb.deadline(d(c.on))) != c.deadline {
			t.Errorf("%s: window from %s, deadline %s; want %s and %s", c.on, day(from), day(gpb.deadline(d(c.on))), c.from, c.deadline)
		}
	}
	bad := gpb
	bad.OpenedOn = nil
	if bad.Validate() == nil {
		t.Error("windows with no contract day were accepted")
	}
	bad = gpb
	bad.GraceMonths = 1
	if bad.Validate() == nil {
		t.Error("a deadline inside the window was accepted")
	}
}

// Spending of a category the bank takes for transfers — and of the ones under
// it — has no grace on this card: interest from its day (decision Р-29).
func TestTransferCategoriesHaveNoGrace(t *testing.T) {
	wallets, bets, food := uuid.New(), uuid.New(), uuid.New()
	set := categorySet{kinds: Kinds{}, parent: map[uuid.UUID]uuid.UUID{bets: wallets}}
	terms := alfa
	terms.TransferCategories = []uuid.UUID{wallets}
	kinds := set.of(terms)
	in := func(op operation.Operation, cat uuid.UUID) operation.Operation {
		op.CategoryID = &cat
		return op
	}
	ops := []operation.Operation{
		in(spend("2026-09-03", 1_000), food), in(spend("2026-09-04", 2_000), wallets), in(spend("2026-09-05", 300), bets),
	}
	st := Work(terms, ops, "RUB", d("2026-09-10"), kinds)
	if got := dues(st); len(got) != 1 || got["2026-10-21"] != 1_000_00 {
		t.Errorf("grace = %v, want the food only", got)
	}
	if st.NonGrace != 2_300_00 || st.NonGraceInterest <= 0 {
		t.Errorf("non-grace = %d (interest %d), want the wallet and the bet", st.NonGrace, st.NonGraceInterest)
	}
	if st := Work(alfa, ops, "RUB", d("2026-09-10"), set.of(alfa)); st.NonGrace != 0 {
		t.Errorf("a card without transfer categories: non-grace = %d", st.NonGrace)
	}
}
