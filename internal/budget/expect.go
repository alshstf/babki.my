package budget

import (
	"context"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
)

// Expected is what a limited category is expected to spend in a month by
// its limit, in the base currency. Covers are the category and its
// subcategories: the rows its limit counts.
type Expected struct {
	Month      time.Time
	CategoryID uuid.UUID
	Covers     []uuid.UUID
	Amount     int64
}

// Expect is what the limits expect to be spent month by month, from today's
// month through last's (budget plan, step 4): this month what is left of
// each limit, a later month the whole limit. What a копилка carried is not
// counted — when it is spent is the family's choice; a subcategory limited
// under a limited parent is in the parent's.
func (s *Service) Expect(ctx context.Context, spaceID uuid.UUID, today, last time.Time) ([]Expected, error) {
	cur, err := s.Month(ctx, spaceID, today)
	if err != nil {
		return nil, err
	}
	limits, err := s.Limits(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return expect(cur, limits, cats, firstOfMonth(last)), nil
}

func expect(cur Month, limits []Limit, cats []category.Category, last time.Time) []Expected {
	byCategory := map[uuid.UUID][]Limit{}
	for _, l := range limits {
		byCategory[l.CategoryID] = append(byCategory[l.CategoryID], l)
	}
	spent := map[uuid.UUID]int64{}
	for _, l := range cur.Lines {
		spent[l.CategoryID] = l.Spent
	}
	parent := map[uuid.UUID]uuid.UUID{}
	children := map[uuid.UUID][]uuid.UUID{}
	for _, c := range cats {
		if c.ParentID != nil {
			parent[c.ID] = *c.ParentID
			children[*c.ParentID] = append(children[*c.ParentID], c.ID)
		}
	}
	var covers func(id uuid.UUID) []uuid.UUID
	covers = func(id uuid.UUID) []uuid.UUID {
		out := []uuid.UUID{id}
		for _, c := range children[id] {
			out = append(out, covers(c)...)
		}
		return out
	}
	var out []Expected
	for m := cur.Month; !m.After(last); m = m.AddDate(0, 1, 0) {
		limited := map[uuid.UUID]bool{}
		for _, c := range ordered(cats) {
			l, ok := inForce(byCategory[c.ID], m)
			if !ok {
				continue
			}
			limited[c.ID] = true
			if p, ok := parent[c.ID]; ok && limited[p] {
				continue
			}
			amount := l.Amount
			if m.Equal(cur.Month) {
				amount = max(l.Amount-spent[c.ID], 0)
			}
			if amount > 0 {
				out = append(out, Expected{Month: m, CategoryID: c.ID, Covers: covers(c.ID), Amount: amount})
			}
		}
	}
	return out
}
