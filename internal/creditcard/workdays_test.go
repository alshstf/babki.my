package creditcard

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

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
