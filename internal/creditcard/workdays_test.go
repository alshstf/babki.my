package creditcard

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// The calendar: the decreed days off and working Saturdays of 2026 and
// 2027, the Labour Code's holidays and weekends for a year with no decree.
func TestTheWorkingDaysFollowTheDecrees(t *testing.T) {
	for day, off := range map[string]bool{
		"2026-01-09": true,  // 3 January moved here
		"2026-01-12": false, // the first working day of 2026
		"2026-03-09": true,  // 8 March was a Sunday
		"2026-11-04": true,
		"2026-12-31": true,  // 4 January moved here
		"2027-02-20": false, // a working Saturday
		"2027-02-22": true,  // its day off
		"2027-11-05": true,  // 2 January moved here
		"2028-01-03": true,  // no decree: the Labour Code's holiday
		"2028-02-24": false,
		"2028-02-26": true, // a Saturday
	} {
		if got := dayOff(d(day)); got != off {
			t.Errorf("%s: day off = %v, want %v", day, got, off)
		}
	}
}

// Газпромбанк's terms (#452): a deadline on a day off moves to the next
// working day — the statement of 15 October is due on 4 November, a
// holiday, so by the 5th; without the option, the 4th.
func TestADeadlineMovesOffADayOff(t *testing.T) {
	card := Terms{
		Limit: 100_000_00, StatementDay: 15, PaymentDays: 20, GraceKind: FromStatement,
		MinPercent: decimal.NewFromInt(5), MinFloor: 500_00, AnnualRate: decimal.RequireFromString("39.9"),
		ShiftToWorkday: true,
	}
	ops := []operation.Operation{spend("2026-10-01", 10_000)}
	st := Work(card, ops, "RUB", d("2026-10-20"), Kinds{})
	if got := dues(st); len(got) != 1 || got["2026-11-05"] != 10_000_00 || day(st.MinimumOn) != "2026-11-05" {
		t.Errorf("shifted: grace %v, minimum on %s; want both the 5th", got, day(st.MinimumOn))
	}
	card.ShiftToWorkday = false
	st = Work(card, ops, "RUB", d("2026-10-20"), Kinds{})
	if got := dues(st); got["2026-11-04"] != 10_000_00 || day(st.MinimumOn) != "2026-11-04" {
		t.Errorf("not shifted: grace %v, minimum on %s; want the 4th", got, day(st.MinimumOn))
	}
}
