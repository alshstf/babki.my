package creditcard

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// Альфа's new line (#460): 60 days from the 1st of the first operation's
// month, its last day moved to the month's end — the memo's case: a first
// operation on March 15, the grace runs out on April 30. The card «без % на
// всё» gives the grace to cash and transfers too.
func TestAlfasGraceCoversMovesToTheMonthsEnd(t *testing.T) {
	card := Terms{
		Limit: 300_000_00, StatementDay: 1, GraceKind: Running, GraceDays: 60, RunFrom: FromMonthStart,
		PayDay: 31, MinPercent: decimal.NewFromInt(3), MinFloor: 300_00, AnnualRate: decimal.RequireFromString("58.99"),
		GraceMoves: true, GraceToMonthEnd: true,
	}
	if err := card.Validate(); err != nil {
		t.Fatal(err)
	}
	cash := map[uuid.UUID]bool{}
	ops := []operation.Operation{moved("2026-03-15", 10_000, cash, true), spend("2026-03-20", 5_000)}
	st := Work(card, ops, "RUB", d("2026-03-25"), Kinds{Cash: cash})
	if got := dues(st); len(got) != 1 || got["2026-04-30"] != 15_000_00 || st.NonGrace != 0 {
		t.Errorf("grace = %v, non-grace %d; want 15 000 by April 30, none without", got, st.NonGrace)
	}

	card.GraceToMonthEnd = false
	if got := dues(Work(card, ops, "RUB", d("2026-03-25"), Kinds{Cash: cash})); got["2026-04-29"] != 15_000_00 {
		t.Errorf("without the month's end: grace = %v, want April 29", got)
	}
	card.GraceMoves = false
	st = Work(card, ops, "RUB", d("2026-03-25"), Kinds{Cash: cash})
	if got := dues(st); got["2026-04-29"] != 5_000_00 || st.NonGrace != 10_000_00 {
		t.Errorf("cash out of the grace: grace = %v, non-grace %d", got, st.NonGrace)
	}
}

// Ozon Банк (#460): a period's purchases are paid by the 16th day after the
// next period («до 80 дней»), those marked «до 140 дней» on Ozon by the 16th
// day after three more.
func TestOzonsPurchasesArePaidStatementsLater(t *testing.T) {
	marked := uuid.New()
	card := Terms{
		Limit: 100_000_00, StatementDay: 10, PaymentDays: 16, GraceKind: FromStatement, GracePeriods: 1,
		GraceCategories: []CategoryPeriods{{CategoryID: marked, Periods: 3}},
		MinPercent:      decimal.NewFromInt(3), AnnualRate: decimal.RequireFromString("39.9"),
	}
	if err := card.Validate(); err != nil {
		t.Fatal(err)
	}
	kinds := Kinds{Graces: map[uuid.UUID]int{marked: 3}}
	onOzon := spend("2026-03-12", 20_000)
	onOzon.CategoryID = &marked
	ops := []operation.Operation{spend("2026-03-12", 8_000), onOzon}

	st := Work(card, ops, "RUB", d("2026-03-20"), kinds)
	if got := dues(st); len(got) != 2 || got["2026-05-26"] != 8_000_00 || got["2026-07-26"] != 20_000_00 {
		t.Errorf("grace = %v, want 8 000 by May 26 and 20 000 by July 26", got)
	}
	// Nothing paid by May 26: the ordinary purchase is lost, the marked one
	// keeps its grace.
	st = Work(card, ops, "RUB", d("2026-06-01"), kinds)
	if len(st.Lost) != 1 || st.Lost[0].Amount != 8_000_00 || !st.Lost[0].Deadline.Equal(d("2026-05-26")) {
		t.Errorf("lost = %+v, want the 8 000 of May 26", st.Lost)
	}
	if got := dues(st); len(got) != 1 || got["2026-07-26"] != 20_000_00 {
		t.Errorf("grace after May = %v", got)
	}
}

func TestGraceRulesKeepToTheirKinds(t *testing.T) {
	for name, card := range map[string]Terms{
		"statements later, running": {GraceKind: Running, GraceDays: 60, RunFrom: FromPurchase, GracePeriods: 1},
		"a category's, long":        {GraceKind: Long, GraceDays: 120, GraceCategories: []CategoryPeriods{{CategoryID: uuid.New(), Periods: 3}}},
		"month's end, statement":    {GraceKind: FromStatement, GraceToMonthEnd: true},
		"13 statements later":       {GraceKind: FromStatement, GracePeriods: 13},
	} {
		card.StatementDay = 1
		if card.Validate() == nil {
			t.Errorf("%s: taken", name)
		}
	}
}
