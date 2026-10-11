package budget

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
)

// What the limits expect: this month what is left (nothing when overspent),
// later months the whole limit — the копилка's carried money aside; a
// subcategory limited under its limited parent is in the parent's, which
// covers it; a limit set from a later month starts there.
func TestTheLimitsExpectWhatIsLeft(t *testing.T) {
	food, fruit, trips, cafe := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cats := []category.Category{
		{ID: food, Kind: category.KindExpense, Position: 1},
		{ID: fruit, Kind: category.KindExpense, Position: 1, ParentID: &food},
		{ID: trips, Kind: category.KindExpense, Position: 2},
		{ID: cafe, Kind: category.KindExpense, Position: 3},
	}
	limits := []Limit{
		{CategoryID: food, From: month("2026-01"), Amount: 30_000_00},
		{CategoryID: fruit, From: month("2026-01"), Amount: 5_000_00},
		{CategoryID: trips, From: month("2026-06"), Amount: 5_000_00, Rollover: true},
		{CategoryID: cafe, From: month("2026-11"), Amount: 4_000_00},
	}
	cur := Month{Month: month("2026-10"), Lines: []Line{
		{CategoryID: food, Limit: 30_000_00, Spent: 21_000_00},
		{CategoryID: fruit, Limit: 5_000_00, Spent: 2_000_00},
		{CategoryID: trips, Limit: 5_000_00, Carried: 20_000_00, Spent: 7_000_00},
	}}
	got := expect(cur, limits, cats, month("2026-11"))
	type row struct {
		month    string
		category uuid.UUID
		amount   int64
		covers   int
	}
	var rows []row
	for _, x := range got {
		rows = append(rows, row{x.Month.Format("2006-01"), x.CategoryID, x.Amount, len(x.Covers)})
	}
	want := []row{
		{"2026-10", food, 9_000_00, 2},
		{"2026-11", food, 30_000_00, 2},
		{"2026-11", trips, 5_000_00, 1},
		{"2026-11", cafe, 4_000_00, 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("expected = %+v\nwant %+v", rows, want)
	}
}
