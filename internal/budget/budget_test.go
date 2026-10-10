package budget

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
)

func month(s string) time.Time {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		panic(err)
	}
	return t
}

// The family of decision Р-25's note, its September: groceries 30 000 a
// month, a trip's копилка 5 000 a month since June, presents' 2 000 since
// August; rent and the rest without a limit.
func TestTheNotesSeptember(t *testing.T) {
	food, trips, gifts, rent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cats := []category.Category{
		{ID: rent, Kind: category.KindExpense, Position: 1},
		{ID: food, Kind: category.KindExpense, Position: 2},
		{ID: trips, Kind: category.KindExpense, Position: 3},
		{ID: gifts, Kind: category.KindExpense, Position: 4},
	}
	months := []time.Time{month("2026-06"), month("2026-07"), month("2026-08"), month("2026-09")}
	id := func(u uuid.UUID) *uuid.UUID { return &u }
	r := cashflow.Report{BaseCurrency: "RUB", Months: months}
	r.Expense.ByMonth = []int64{70_000_00, 72_000_00, 71_000_00, 100_790_00}
	r.Expense.Lines = []cashflow.Line{
		{CategoryID: id(rent), Flow: cashflow.Flow{ByMonth: []int64{45_000_00, 45_000_00, 45_000_00, 52_990_00}}},
		{CategoryID: id(food), Flow: cashflow.Flow{ByMonth: []int64{25_000_00, 27_000_00, 26_000_00, 26_000_00}}},
		{CategoryID: id(trips), Flow: cashflow.Flow{ByMonth: []int64{0, 0, 0, 18_500_00}}},
		{CategoryID: id(gifts), Flow: cashflow.Flow{ByMonth: []int64{0, 0, 0, 3_300_00}}},
	}
	limits := map[uuid.UUID][]Limit{
		food:  {{CategoryID: food, From: month("2026-06"), Amount: 30_000_00}},
		trips: {{CategoryID: trips, From: month("2026-06"), Amount: 5_000_00, Rollover: true}},
		gifts: {{CategoryID: gifts, From: month("2026-08"), Amount: 2_000_00, Rollover: true}},
	}
	b := assemble(month("2026-09"), r, cats, limits)
	want := []Line{
		{CategoryID: food, Limit: 30_000_00, Since: month("2026-06"), Spent: 26_000_00, Left: 4_000_00},
		{CategoryID: trips, Limit: 5_000_00, Rollover: true, Since: month("2026-06"), Carried: 15_000_00, Spent: 18_500_00, Left: 1_500_00},
		{CategoryID: gifts, Limit: 2_000_00, Rollover: true, Since: month("2026-08"), Carried: 2_000_00, Spent: 3_300_00, Left: 700_00},
	}
	if len(b.Lines) != len(want) {
		t.Fatalf("lines = %+v", b.Lines)
	}
	for i := range want {
		if b.Lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, b.Lines[i], want[i])
		}
	}
	// 30 000 + 5 000 + 15 000 + 2 000 + 2 000; spent 47 800; the rest of
	// the month's spending is the rent's.
	if b.Planned != 54_000_00 || b.Spent != 47_800_00 || b.Left != 6_200_00 || b.Unlimited != 52_990_00 {
		t.Errorf("planned %d, spent %d, left %d, unlimited %d", b.Planned, b.Spent, b.Left, b.Unlimited)
	}
}

// A копилка carries what is left over, never an overspend; a limit changed
// from a month stands from it; one taken off is no line.
func TestTheKopilkaCarriesOnlyWhatIsLeft(t *testing.T) {
	trips := uuid.New()
	months := []time.Time{month("2026-01"), month("2026-02"), month("2026-03"), month("2026-04")}
	spent := []int64{0, 9_000_00, 0, 1_000_00}
	limits := []Limit{{CategoryID: trips, From: month("2026-01"), Amount: 5_000_00, Rollover: true}}
	// January leaves 5 000; February spends 9 000 of 10 000 — 1 000 goes on;
	// March adds 5 000: April starts with 6 000, spends 1 000 of 11 000.
	l, ok := line(trips, limits, months, spent, month("2026-04"))
	if !ok || l.Carried != 6_000_00 || l.Left != 10_000_00 {
		t.Errorf("April = %+v, want 6 000 carried, 10 000 left", l)
	}
	overspent := []int64{0, 20_000_00, 0, 0}
	if l, _ := line(trips, limits, months, overspent, month("2026-03")); l.Carried != 0 {
		t.Errorf("after an overspend March carries %d, want nothing", l.Carried)
	}
	// From March 8 000 a month: March's limit is the new one, the копилка
	// keeps what it had.
	changed := slices.Concat(limits, []Limit{{CategoryID: trips, From: month("2026-03"), Amount: 8_000_00, Rollover: true}})
	if l, _ := line(trips, changed, months, spent, month("2026-03")); l.Limit != 8_000_00 || l.Carried != 1_000_00 || !l.Since.Equal(month("2026-03")) {
		t.Errorf("March with the new limit = %+v", l)
	}
	// The копилка off from March: nothing carried from then.
	plain := slices.Concat(limits, []Limit{{CategoryID: trips, From: month("2026-03"), Amount: 5_000_00}})
	if l, _ := line(trips, plain, months, spent, month("2026-04")); l.Carried != 0 || l.Left != 4_000_00 {
		t.Errorf("April without the копилка = %+v", l)
	}
	off := slices.Concat(limits, []Limit{{CategoryID: trips, From: month("2026-04")}})
	if _, ok := line(trips, off, months, spent, month("2026-04")); ok {
		t.Error("a limit taken off still has a line")
	}
	if _, ok := line(trips, limits, months, spent, month("2025-12")); ok {
		t.Error("a month before the limit has a line")
	}
}

// A limited subcategory under a limited parent: both lines, the spending
// counted once in the sums.
func TestASubcategoryIsCountedInItsParentOnce(t *testing.T) {
	food, cafe := uuid.New(), uuid.New()
	cats := []category.Category{
		{ID: cafe, Kind: category.KindExpense, ParentID: &food, Position: 1},
		{ID: food, Kind: category.KindExpense, Position: 1},
	}
	id := func(u uuid.UUID) *uuid.UUID { return &u }
	r := cashflow.Report{Months: []time.Time{month("2026-09")}}
	r.Expense.ByMonth = []int64{31_100_00}
	r.Expense.Lines = []cashflow.Line{{
		CategoryID: id(food), Flow: cashflow.Flow{ByMonth: []int64{31_100_00}},
		Children: []cashflow.Line{{CategoryID: id(cafe), Flow: cashflow.Flow{ByMonth: []int64{5_100_00}}}},
	}}
	limits := map[uuid.UUID][]Limit{
		food: {{CategoryID: food, From: month("2026-09"), Amount: 36_000_00}},
		cafe: {{CategoryID: cafe, From: month("2026-09"), Amount: 6_000_00}},
	}
	b := assemble(month("2026-09"), r, cats, limits)
	if len(b.Lines) != 2 || b.Lines[0].CategoryID != food || b.Lines[1].CategoryID != cafe {
		t.Fatalf("lines = %+v, want the parent, then its subcategory", b.Lines)
	}
	if b.Planned != 36_000_00 || b.Spent != 31_100_00 || b.Unlimited != 0 {
		t.Errorf("planned %d, spent %d, unlimited %d", b.Planned, b.Spent, b.Unlimited)
	}
}
