package category_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
)

// The first rule that fits wins; case and «ё» do not matter; a rule files only
// rows of its category's direction and never into an archived category.
func TestTheFirstFittingRuleNamesTheCategory(t *testing.T) {
	food, cafe, gifts, old := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cats := map[uuid.UUID]category.Category{
		food:  {ID: food, Kind: category.KindExpense, Name: "Продукты"},
		cafe:  {ID: cafe, Kind: category.KindExpense, Name: "Кафе"},
		gifts: {ID: gifts, Kind: category.KindIncome, Name: "Подарки"},
		old:   {ID: old, Kind: category.KindExpense, Name: "Старое", Archived: true},
	}
	rules := []category.Rule{
		{CategoryID: old, Field: category.FieldAny, Pattern: "пятёрочка"},
		{CategoryID: gifts, Field: category.FieldCounterparty, Pattern: "бабушка"},
		{CategoryID: food, Field: category.FieldCounterparty, Pattern: "ПЯТЁРОЧКА"},
		{CategoryID: cafe, Field: category.FieldNote, Pattern: "кофе"},
		{CategoryID: cafe, Field: category.FieldCounterparty, Pattern: "пятерочка"},
	}
	for name, c := range map[string]struct {
		kind category.Kind
		text category.Text
		want *uuid.UUID
	}{
		"ё against е, case aside":      {category.KindExpense, category.Text{Counterparty: "Пятерочка 1234"}, &food},
		"a note rule reads the note":   {category.KindExpense, category.Text{Note: "Кофе с собой"}, &cafe},
		"nor the counterparty":         {category.KindExpense, category.Text{Counterparty: "Кофе-хаус"}, nil},
		"an income rule on a spending": {category.KindExpense, category.Text{Counterparty: "Бабушка"}, nil},
		"an income rule on an income":  {category.KindIncome, category.Text{Counterparty: "бабушка Таня"}, &gifts},
		"nothing fits":                 {category.KindExpense, category.Text{Counterparty: "Лента"}, nil},
	} {
		got := category.Match(rules, cats, c.kind, c.text)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

// Rules are kept in the order they are added and can be reordered, edited and
// removed; a bad one is refused.
func TestRulesAreKeptInTheirOrder(t *testing.T) {
	store, space := newSpace(t)
	ctx := t.Context()
	list, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	food := find(list, category.KindExpense, "Продукты").ID
	taxi := find(list, category.KindExpense, "Такси").ID
	a, err := store.CreateRule(ctx, space, category.Rule{CategoryID: food, Field: category.FieldCounterparty, Pattern: "  Пятёрочка "})
	if err != nil || a.Pattern != "Пятёрочка" {
		t.Fatalf("create = %+v, %v", a, err)
	}
	b, err := store.CreateRule(ctx, space, category.Rule{CategoryID: taxi, Field: category.FieldAny, Pattern: "Яндекс Go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReorderRules(ctx, space, []uuid.UUID{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	rules, err := store.Rules(ctx, space)
	if err != nil || len(rules) != 2 || rules[0].ID != b.ID {
		t.Fatalf("after reorder: %+v, %v", rules, err)
	}
	if err := store.ReorderRules(ctx, space, []uuid.UUID{b.ID, b.ID}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("an order naming one rule twice: %v", err)
	}
	pattern := "Пятерочка"
	if r, err := store.UpdateRule(ctx, space, a.ID, category.RuleUpdate{Pattern: &pattern}); err != nil || r.Pattern != pattern {
		t.Errorf("update = %+v, %v", r, err)
	}
	for name, bad := range map[string]category.Rule{
		"no text":           {CategoryID: food, Field: category.FieldAny, Pattern: "  "},
		"no such field":     {CategoryID: food, Field: "amount", Pattern: "x"},
		"nobody's category": {CategoryID: uuid.New(), Field: category.FieldAny, Pattern: "x"},
	} {
		if _, err := store.CreateRule(ctx, space, bad); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := store.DeleteRule(ctx, space, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRule(ctx, space, a.ID); !errors.Is(err, category.ErrRuleNotFound) {
		t.Errorf("removing it twice: %v", err)
	}
}
