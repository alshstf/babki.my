package category

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// Field is the text of a row a rule looks in.
type Field string

const (
	FieldCounterparty Field = "counterparty"
	FieldNote         Field = "note"
	FieldAny          Field = "any"
	// FieldItem looks in a receipt's item names (decision Р-36), never in a
	// row's own text.
	FieldItem Field = "item"
)

func (f Field) Valid() bool {
	return f == FieldCounterparty || f == FieldNote || f == FieldAny || f == FieldItem
}

// MaxPatternRunes bounds a rule's text, as the schema's CHECK does.
const MaxPatternRunes = 200

// ErrRuleNotFound is a rule that is not the space's: a 404.
var ErrRuleNotFound = fmt.Errorf("category rule: %w", pgx.ErrNoRows)

// Rule files a row under its category when the row's field holds the
// pattern, letter case aside. Rules are tried in their order; the first that
// fits wins.
type Rule struct {
	ID         uuid.UUID
	SpaceID    uuid.UUID
	CategoryID uuid.UUID
	Field      Field
	Pattern    string
	Position   int
	CreatedAt  time.Time
}

// Text is what a rule reads of a row, or Item, of a line of its receipt.
type Text struct {
	Counterparty, Note string
	Item               string
}

// fold is the text as a rule compares it: letter case aside, and «ё» read as
// «е», since banks write «Пятерочка» where a person types «Пятёрочка».
func fold(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "ё", "е")
}

// fits reports whether the rule's pattern occurs in the row's field.
func (r Rule) fits(t Text) bool {
	p := fold(r.Pattern)
	switch r.Field {
	case FieldCounterparty:
		return strings.Contains(fold(t.Counterparty), p)
	case FieldNote:
		return strings.Contains(fold(t.Note), p)
	case FieldItem:
		return t.Item != "" && strings.Contains(fold(t.Item), p)
	}
	return strings.Contains(fold(t.Counterparty), p) || strings.Contains(fold(t.Note), p)
}

// Match is the category the first fitting rule names among those of kind that
// are not archived; nil when no rule fits. rules are in their order.
func Match(rules []Rule, categories map[uuid.UUID]Category, kind Kind, t Text) *uuid.UUID {
	for _, r := range rules {
		c, ok := categories[r.CategoryID]
		if !ok || c.Kind != kind || c.Archived {
			continue
		}
		if r.fits(t) {
			id := r.CategoryID
			return &id
		}
	}
	return nil
}

const ruleCols = `id, space_id, category_id, field, pattern, position, created_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.SpaceID, &r.CategoryID, &r.Field, &r.Pattern, &r.Position, &r.CreatedAt)
	return r, err
}

// Rules are the space's rules in the order they are tried.
func (s *Store) Rules(ctx context.Context, spaceID uuid.UUID) ([]Rule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+ruleCols+` FROM category_rules WHERE space_id = $1 ORDER BY position, created_at, id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("category rules: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rule, error) { return scanRule(row) })
	if err != nil {
		return nil, fmt.Errorf("category rules: %w", err)
	}
	return out, nil
}

// cleanRule checks a rule's parts: a field, a pattern of 1 to MaxPatternRunes
// characters once trimmed, and a category of the space's.
func (s *Store) cleanRule(ctx context.Context, spaceID uuid.UUID, r Rule) (Rule, error) {
	if !r.Field.Valid() {
		return Rule{}, fmt.Errorf("%w: field is counterparty, note, any or item", family.ErrValidation)
	}
	r.Pattern = strings.TrimSpace(r.Pattern)
	if n := utf8.RuneCountInString(r.Pattern); n == 0 || n > MaxPatternRunes {
		return Rule{}, fmt.Errorf("%w: a rule's text is 1 to %d characters", family.ErrValidation, MaxPatternRunes)
	}
	if _, err := s.Get(ctx, spaceID, r.CategoryID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Rule{}, fmt.Errorf("%w: category_id is not one of this family's categories", family.ErrValidation)
		}
		return Rule{}, err
	}
	return r, nil
}

// CreateRule adds a rule after the space's others.
func (s *Store) CreateRule(ctx context.Context, spaceID uuid.UUID, r Rule) (Rule, error) {
	r, err := s.cleanRule(ctx, spaceID, r)
	if err != nil {
		return Rule{}, err
	}
	created, err := scanRule(s.db.QueryRow(ctx, `
		INSERT INTO category_rules (space_id, category_id, field, pattern, position)
		SELECT $1, $2, $3, $4, COALESCE(MAX(position) + 1, 0) FROM category_rules WHERE space_id = $1
		RETURNING `+ruleCols, spaceID, r.CategoryID, r.Field, r.Pattern))
	if err != nil {
		return Rule{}, fmt.Errorf("category rule: create: %w", err)
	}
	return created, nil
}

// RuleUpdate changes what is set.
type RuleUpdate struct {
	CategoryID *uuid.UUID
	Field      *Field
	Pattern    *string
	Position   *int
}

// UpdateRule changes a rule of the space's.
func (s *Store) UpdateRule(ctx context.Context, spaceID, id uuid.UUID, upd RuleUpdate) (Rule, error) {
	cur, err := scanRule(s.db.QueryRow(ctx, `SELECT `+ruleCols+` FROM category_rules WHERE space_id = $1 AND id = $2`, spaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrRuleNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("category rule: %w", err)
	}
	if upd.CategoryID != nil {
		cur.CategoryID = *upd.CategoryID
	}
	if upd.Field != nil {
		cur.Field = *upd.Field
	}
	if upd.Pattern != nil {
		cur.Pattern = *upd.Pattern
	}
	if upd.Position != nil {
		cur.Position = *upd.Position
	}
	if cur, err = s.cleanRule(ctx, spaceID, cur); err != nil {
		return Rule{}, err
	}
	updated, err := scanRule(s.db.QueryRow(ctx, `
		UPDATE category_rules SET category_id = $3, field = $4, pattern = $5, position = $6
		WHERE space_id = $1 AND id = $2 RETURNING `+ruleCols,
		spaceID, id, cur.CategoryID, cur.Field, cur.Pattern, cur.Position))
	if err != nil {
		return Rule{}, fmt.Errorf("category rule: update: %w", err)
	}
	return updated, nil
}

// ReorderRules puts the space's rules in the order ids name them; ids must
// name every rule of the space's once.
func (s *Store) ReorderRules(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("category rules: reorder: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM category_rules WHERE space_id = $1`, spaceID).Scan(&count); err != nil {
		return fmt.Errorf("category rules: reorder: %w", err)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE category_rules r SET position = o.n - 1
		FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, n)
		WHERE r.space_id = $1 AND r.id = o.id`, spaceID, ids)
	if err != nil {
		return fmt.Errorf("category rules: reorder: %w", err)
	}
	if int(ct.RowsAffected()) != count || len(ids) != count {
		return fmt.Errorf("%w: the order must name each of the family's %d rules once", family.ErrValidation, count)
	}
	return tx.Commit(ctx)
}

// DeleteRule removes a rule of the space's.
func (s *Store) DeleteRule(ctx context.Context, spaceID, id uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM category_rules WHERE space_id = $1 AND id = $2`, spaceID, id)
	if err != nil {
		return fmt.Errorf("category rule: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrRuleNotFound
	}
	return nil
}
