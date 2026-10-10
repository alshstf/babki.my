package category

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
)

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	// parentKey is the foreign key a category's children hold; any other key
	// refusing a removal is an operation's.
	parentKey = "categories_parent_id_fkey"
)

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

const cols = `id, space_id, parent_id, kind, name, archived, position, created_at, updated_at`

func scan(row pgx.Row) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.SpaceID, &c.ParentID, &c.Kind, &c.Name, &c.Archived, &c.Position, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// List returns the space's categories, spending before earning, each level in
// its order. A space that never had categories gets the default set first.
func (s *Store) List(ctx context.Context, spaceID uuid.UUID) ([]Category, error) {
	if err := s.ensureDefaults(ctx, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM categories WHERE space_id = $1
		ORDER BY kind, position, lower(name), id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("category: list: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Category, error) { return scan(row) })
	if err != nil {
		return nil, fmt.Errorf("category: list: %w", err)
	}
	return out, nil
}

// ensureDefaults files the default set for a space that never had one, once:
// the marker row is taken first, so two first requests do not both seed.
func (s *Store) ensureDefaults(ctx context.Context, spaceID uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("category: seed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ct, err := tx.Exec(ctx, `INSERT INTO category_defaults (space_id) VALUES ($1) ON CONFLICT DO NOTHING`, spaceID)
	if err != nil {
		return fmt.Errorf("category: seed: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	for _, kind := range []Kind{KindExpense, KindIncome} {
		for i, sd := range defaults[kind] {
			var parent uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO categories (space_id, kind, name, position)
				VALUES ($1, $2, $3, $4) RETURNING id`, spaceID, kind, sd.name, i).Scan(&parent); err != nil {
				return fmt.Errorf("category: seed %q: %w", sd.name, err)
			}
			for j, child := range sd.children {
				if _, err := tx.Exec(ctx, `INSERT INTO categories (space_id, parent_id, kind, name, position)
					VALUES ($1, $2, $3, $4, $5)`, spaceID, parent, kind, child, j); err != nil {
					return fmt.Errorf("category: seed %q: %w", child, err)
				}
			}
		}
	}
	return tx.Commit(ctx)
}

// Get is one of the space's categories, ErrNotFound for any other.
func (s *Store) Get(ctx context.Context, spaceID, id uuid.UUID) (Category, error) {
	c, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM categories WHERE space_id = $1 AND id = $2`, spaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, fmt.Errorf("category: get: %w", err)
	}
	return c, nil
}

// placeUnder checks that a category of kind may sit under parent: the
// space's, of the same kind, itself on the top level (the tree is two deep).
func (s *Store) placeUnder(ctx context.Context, spaceID uuid.UUID, kind Kind, parentID uuid.UUID) error {
	parent, err := s.Get(ctx, spaceID, parentID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: the parent category is not the family's", family.ErrValidation)
	}
	if err != nil {
		return err
	}
	if parent.Kind != kind {
		return fmt.Errorf("%w: a spending category and an earning one cannot sit under each other", family.ErrValidation)
	}
	if parent.ParentID != nil {
		return fmt.Errorf("%w: categories go two levels deep; the parent is itself under another", family.ErrValidation)
	}
	return nil
}

// Create files a category last among its siblings.
func (s *Store) Create(ctx context.Context, spaceID uuid.UUID, kind Kind, name string, parentID *uuid.UUID) (Category, error) {
	if !kind.Valid() {
		return Category{}, fmt.Errorf("%w: kind is expense or income", family.ErrValidation)
	}
	name, err := cleanName(name)
	if err != nil {
		return Category{}, err
	}
	if err := s.ensureDefaults(ctx, spaceID); err != nil {
		return Category{}, err
	}
	if parentID != nil {
		if err := s.placeUnder(ctx, spaceID, kind, *parentID); err != nil {
			return Category{}, err
		}
	}
	c, err := scan(s.db.QueryRow(ctx, `
		INSERT INTO categories (space_id, parent_id, kind, name, position)
		VALUES ($1, $2, $3, $4, (SELECT COALESCE(max(position) + 1, 0) FROM categories
			WHERE space_id = $1 AND kind = $3 AND parent_id IS NOT DISTINCT FROM $2))
		RETURNING `+cols, spaceID, parentID, kind, name))
	if err != nil {
		return Category{}, mapWriteError("create", err)
	}
	return c, nil
}

// Update is a change of a category's fields; nil leaves a field as it is.
// ParentID set to a nil pointer moves the category to the top level.
type Update struct {
	Name     *string
	ParentID **uuid.UUID
	Archived *bool
	Position *int
}

// Update applies upd. A category with others under it cannot itself go under
// one; archiving a category archives the ones under it.
func (s *Store) Update(ctx context.Context, spaceID, id uuid.UUID, upd Update) (Category, error) {
	cur, err := s.Get(ctx, spaceID, id)
	if err != nil {
		return Category{}, err
	}
	name := cur.Name
	if upd.Name != nil {
		if name, err = cleanName(*upd.Name); err != nil {
			return Category{}, err
		}
	}
	parentID := cur.ParentID
	if upd.ParentID != nil {
		parentID = *upd.ParentID
		if parentID != nil {
			if *parentID == id {
				return Category{}, fmt.Errorf("%w: a category cannot sit under itself", family.ErrValidation)
			}
			if err := s.placeUnder(ctx, spaceID, cur.Kind, *parentID); err != nil {
				return Category{}, err
			}
			var children bool
			if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM categories WHERE parent_id = $1)`, id).Scan(&children); err != nil {
				return Category{}, fmt.Errorf("category: update: %w", err)
			}
			if children {
				return Category{}, fmt.Errorf("%w: categories go two levels deep; this one has categories under it", family.ErrValidation)
			}
		}
	}
	archived, position := cur.Archived, cur.Position
	if upd.Archived != nil {
		archived = *upd.Archived
	}
	if upd.Position != nil {
		position = *upd.Position
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Category{}, fmt.Errorf("category: update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scan(tx.QueryRow(ctx, `
		UPDATE categories SET name = $3, parent_id = $4, archived = $5, position = $6, updated_at = now()
		WHERE space_id = $1 AND id = $2 RETURNING `+cols, spaceID, id, name, parentID, archived, position))
	if err != nil {
		return Category{}, mapWriteError("update", err)
	}
	if archived && !cur.Archived {
		if _, err := tx.Exec(ctx, `UPDATE categories SET archived = true, updated_at = now() WHERE parent_id = $1`, id); err != nil {
			return Category{}, fmt.Errorf("category: archive the ones under %s: %w", id, err)
		}
	}
	// An active category under an archived one would vanish from every list
	// that hides the archive, so bringing it back brings its parent back too.
	if !archived && parentID != nil {
		if _, err := tx.Exec(ctx, `UPDATE categories SET archived = false, updated_at = now() WHERE id = $1 AND archived`, *parentID); err != nil {
			return Category{}, fmt.Errorf("category: bring back the one above %s: %w", id, err)
		}
	}
	return c, tx.Commit(ctx)
}

// Delete removes a category nothing refers to: ErrHasChildren when others sit
// under it, ErrInUse when an operation names it.
func (s *Store) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM categories WHERE space_id = $1 AND id = $2`, spaceID, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			if pgErr.ConstraintName == parentKey {
				return ErrHasChildren
			}
			return ErrInUse
		}
		return fmt.Errorf("category: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func mapWriteError(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return ErrNameTaken
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("category: %s: %w", what, err)
}
