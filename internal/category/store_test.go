package category_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/testdb"
)

func newSpace(t *testing.T) (*category.Store, uuid.UUID) {
	t.Helper()
	pool := testdb.New(t)
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(t.Context(), "alex", "A", "h")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := fam.CreateSpaceWithOwner(t.Context(), "S", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return category.NewStore(pool), sp.ID
}

func find(list []category.Category, kind category.Kind, name string) *category.Category {
	for i := range list {
		if list[i].Kind == kind && list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// A family's first look finds the default set, spending first; it is filed
// once, and a family that removed its categories does not get them back.
func TestAFamilyStartsWithTheDefaultSetOnce(t *testing.T) {
	store, space := newSpace(t)
	ctx := t.Context()

	first, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 20 || first[0].Kind != category.KindExpense || first[len(first)-1].Kind != category.KindIncome {
		t.Fatalf("got %d categories, want the default set, spending before earning", len(first))
	}
	transport := find(first, category.KindExpense, "Транспорт")
	taxi := find(first, category.KindExpense, "Такси")
	if transport == nil || taxi == nil || taxi.ParentID == nil || *taxi.ParentID != transport.ID {
		t.Fatalf("Такси should sit under Транспорт: %+v, %+v", transport, taxi)
	}
	if find(first, category.KindExpense, "Подарки") == nil || find(first, category.KindIncome, "Подарки") == nil {
		t.Error("Подарки should be both a spending and an earning category")
	}
	again, err := store.List(ctx, space)
	if err != nil || len(again) != len(first) {
		t.Fatalf("a second look found %d (%v), want the same %d", len(again), err, len(first))
	}

	// Remove every category: children first, then parents.
	for _, c := range again {
		if c.ParentID != nil {
			if err := store.Delete(ctx, space, c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range again {
		if c.ParentID == nil {
			if err := store.Delete(ctx, space, c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	empty, err := store.List(ctx, space)
	if err != nil || len(empty) != 0 {
		t.Errorf("after removing all, got %d (%v), want none: the set is not filed twice", len(empty), err)
	}
}

// The tree is two deep, a child is of its parent's kind, a name is unique on
// its level of its kind, and a parent with children cannot be removed.
func TestTheCategoryTreeKeepsItsRules(t *testing.T) {
	store, space := newSpace(t)
	ctx := t.Context()
	list, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	transport := find(list, category.KindExpense, "Транспорт")
	taxi := find(list, category.KindExpense, "Такси")
	salary := find(list, category.KindIncome, "Зарплата")

	kick, err := store.Create(ctx, space, category.KindExpense, "  Самокаты  ", &transport.ID)
	if err != nil || kick.Name != "Самокаты" || kick.ParentID == nil || *kick.ParentID != transport.ID {
		t.Fatalf("create under Транспорт: %+v, %v", kick, err)
	}
	for name, try := range map[string]func() error{
		"under a child":        func() error { _, err := store.Create(ctx, space, category.KindExpense, "x", &taxi.ID); return err },
		"under the other kind": func() error { _, err := store.Create(ctx, space, category.KindIncome, "x", &transport.ID); return err },
		"a taken name": func() error {
			_, err := store.Create(ctx, space, category.KindExpense, "такси", &transport.ID)
			return err
		},
		"an empty name":   func() error { _, err := store.Create(ctx, space, category.KindExpense, " ", nil); return err },
		"an unknown kind": func() error { _, err := store.Create(ctx, space, category.Kind("x"), "x", nil); return err },
		"a parent under a child": func() error {
			p := &kick.ID
			_, err := store.Update(ctx, space, transport.ID, category.Update{ParentID: &p})
			return err
		},
		"itself as parent": func() error {
			p := &kick.ID
			_, err := store.Update(ctx, space, kick.ID, category.Update{ParentID: &p})
			return err
		},
	} {
		if err := try(); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	if _, err := store.Create(ctx, space, category.KindIncome, "Самокаты", &salary.ID); err != nil {
		t.Errorf("the same name in the other kind: %v", err)
	}

	// To the top level and back.
	var top *uuid.UUID
	moved, err := store.Update(ctx, space, kick.ID, category.Update{ParentID: &top})
	if err != nil || moved.ParentID != nil {
		t.Fatalf("move to the top: %+v, %v", moved, err)
	}

	if err := store.Delete(ctx, space, transport.ID); !errors.Is(err, category.ErrHasChildren) {
		t.Errorf("removing Транспорт: %v, want ErrHasChildren", err)
	}
	if err := store.Delete(ctx, space, uuid.New()); !errors.Is(err, category.ErrNotFound) {
		t.Errorf("removing an unknown category: %v, want ErrNotFound", err)
	}
}

// Archiving a category archives the ones under it; renaming keeps its place.
func TestArchivingACategoryArchivesTheOnesUnderIt(t *testing.T) {
	store, space := newSpace(t)
	ctx := t.Context()
	list, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	home := find(list, category.KindExpense, "Дом")
	yes := true
	if _, err := store.Update(ctx, space, home.ID, category.Update{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	after, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range after {
		if c.ParentID != nil && *c.ParentID == home.ID && !c.Archived {
			t.Errorf("%s under Дом is not archived", c.Name)
		}
	}
	name := "Жильё"
	renamed, err := store.Update(ctx, space, home.ID, category.Update{Name: &name})
	if err != nil || renamed.Name != "Жильё" || renamed.Position != home.Position {
		t.Errorf("rename: %+v, %v", renamed, err)
	}

	// Bringing one back from under the archive brings back the one above it,
	// and only that one.
	rent := find(after, category.KindExpense, "Аренда")
	no := false
	if _, err := store.Update(ctx, space, rent.ID, category.Update{Archived: &no}); err != nil {
		t.Fatal(err)
	}
	back, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	if find(back, category.KindExpense, "Жильё").Archived {
		t.Error("Жильё stays archived above Аренда, which came back")
	}
	if !find(back, category.KindExpense, "Коммунальные платежи").Archived {
		t.Error("Коммунальные платежи came back with Аренда")
	}
}

// Another family's category is not found.
func TestAnotherFamilysCategoryIsNotFound(t *testing.T) {
	store, space := newSpace(t)
	ctx := t.Context()
	list, err := store.List(ctx, space)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uuid.New(), list[0].ID); !errors.Is(err, category.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
