package operation

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// maxParts is the most categories one row is split across.
const maxParts = 50

// SetParts splits a spending or an earning across categories (decision Р-36):
// two parts at least, each of a category the row's own could be, adding up
// to the row's amount. The row's own category stays; nothing the engine reads
// changes.
func (s *Service) SetParts(ctx context.Context, spaceID, id uuid.UUID, parts []Part) (Operation, error) {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st := NewStore(tx)
	old, err := st.ByID(ctx, spaceID, id)
	if err != nil {
		return Operation{}, err
	}
	if !Categorizable(old) {
		return Operation{}, notCategorizable(old)
	}
	if len(parts) < 2 || len(parts) > maxParts {
		return Operation{}, fmt.Errorf("%w: a row is split in 2 to %d parts; one is the row's own category", family.ErrValidation, maxParts)
	}
	var sum int64
	for _, p := range parts {
		if p.Amount <= 0 {
			return Operation{}, fmt.Errorf("%w: a part's amount is above zero", family.ErrValidation)
		}
		sum += p.Amount
		// A part's category is held to the rules of the row's own; one the row
		// was split under before may stay archived.
		probe, before := old, old
		probe.CategoryID = &p.CategoryID
		before.CategoryID = nil
		for _, q := range old.Parts {
			if q.CategoryID == p.CategoryID {
				before.CategoryID = &q.CategoryID
			}
		}
		if err := st.checkCategory(ctx, spaceID, probe, &before); err != nil {
			return Operation{}, err
		}
	}
	if sum != abs(old.AmountMinor) {
		return Operation{}, fmt.Errorf("%w: the parts add up to %d, the row is %d", family.ErrValidation, sum, abs(old.AmountMinor))
	}
	if err := st.deleteParts(ctx, spaceID, id); err != nil {
		return Operation{}, err
	}
	for i, p := range parts {
		if _, err := tx.Exec(ctx, `INSERT INTO operation_parts (operation_id, space_id, position, category_id, amount_minor)
			VALUES ($1, $2, $3, $4, $5)`, id, spaceID, i, p.CategoryID, p.Amount); err != nil {
			return Operation{}, fmt.Errorf("operation: set parts: %w", err)
		}
	}
	stored, err := st.ByID(ctx, spaceID, id)
	if err != nil {
		return Operation{}, err
	}
	return stored, tx.Commit(ctx)
}

// ClearParts makes a split row its one category's again.
func (s *Service) ClearParts(ctx context.Context, spaceID, id uuid.UUID) (Operation, error) {
	if err := s.store.deleteParts(ctx, spaceID, id); err != nil {
		return Operation{}, err
	}
	return s.store.ByID(ctx, spaceID, id)
}

func (s *Store) deleteParts(ctx context.Context, spaceID, id uuid.UUID) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM operation_parts WHERE space_id = $1 AND operation_id = $2`, spaceID, id); err != nil {
		return fmt.Errorf("operation: delete parts: %w", err)
	}
	return nil
}

// attachParts fills Parts on ops with one query. Parts that no longer add up
// to their row's amount — the row changed by another path — are not the row's.
func (s *Store) attachParts(ctx context.Context, spaceID uuid.UUID, ops []Operation) error {
	if len(ops) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	rows, err := s.db.Query(ctx, `SELECT operation_id, category_id, amount_minor FROM operation_parts
		WHERE space_id = $1 AND operation_id = ANY($2) ORDER BY operation_id, position`, spaceID, ids)
	if err != nil {
		return fmt.Errorf("operation: parts: %w", err)
	}
	type row struct {
		op uuid.UUID
		Part
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.op, &x.CategoryID, &x.Amount)
	})
	if err != nil {
		return err
	}
	byOp := map[uuid.UUID][]Part{}
	for _, x := range list {
		byOp[x.op] = append(byOp[x.op], x.Part)
	}
	for i := range ops {
		parts := byOp[ops[i].ID]
		var sum int64
		for _, p := range parts {
			sum += p.Amount
		}
		if len(parts) > 0 && sum == abs(ops[i].AmountMinor) {
			ops[i].Parts = parts
		}
	}
	return nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
