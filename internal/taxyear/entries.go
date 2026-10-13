package taxyear

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/portfolio"
)

// Entry is a wage payment's gross and the tax withheld from it as its
// payslip says, entered by hand over the program's reckoning.
type Entry struct {
	Gross, Withheld int64
}

type operations interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (portfolio.Operation, error)
}

// WithEntries lets the family write a wage payment's gross and withheld tax
// by hand (decision Р-41: the wages' module).
func (s *Service) WithEntries(x db.Executor, ops operations) *Service {
	s.db, s.ops = x, ops
	return s
}

// ErrNotWages: the payment is not filed under a category marked as wages.
var ErrNotWages = fmt.Errorf("%w: the payment is not a wage", family.ErrValidation)

// Enter writes a wage payment's gross and withheld tax.
func (s *Service) Enter(ctx context.Context, spaceID, opID uuid.UUID, e Entry) error {
	if e.Gross <= 0 || e.Withheld < 0 || e.Withheld > e.Gross {
		return fmt.Errorf("%w: the gross is above zero, the tax withheld from zero to the gross", family.ErrValidation)
	}
	if err := s.wage(ctx, spaceID, opID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO wage_entries (operation_id, space_id, gross_minor, withheld_minor)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (operation_id) DO UPDATE SET gross_minor = EXCLUDED.gross_minor, withheld_minor = EXCLUDED.withheld_minor`,
		opID, spaceID, e.Gross, e.Withheld)
	return err
}

// Unenter drops what was written for a payment: the program's reckoning
// stands again.
func (s *Service) Unenter(ctx context.Context, spaceID, opID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wage_entries WHERE space_id = $1 AND operation_id = $2`, spaceID, opID)
	return err
}

// wage says whether the space's payment is an earning filed, whole or in
// part, under a category marked as wages.
func (s *Service) wage(ctx context.Context, spaceID, opID uuid.UUID) error {
	if s.db == nil || s.ops == nil || s.categories == nil {
		return errors.New("taxyear: entries are not set up")
	}
	op, err := s.ops.ByID(ctx, spaceID, opID)
	if err != nil {
		return err
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return err
	}
	wages := map[uuid.UUID]bool{}
	for _, c := range cats {
		if c.TaxMark != nil && *c.TaxMark == category.TaxWages {
			wages[c.ID] = true
		}
	}
	if op.Type != portfolio.TypeDeposit {
		return ErrNotWages
	}
	if op.CategoryID != nil && wages[*op.CategoryID] {
		return nil
	}
	for _, p := range op.Parts {
		if wages[p.CategoryID] {
			return nil
		}
	}
	return ErrNotWages
}

// Entries are the space's written wage payments by their journal row.
func (s *Service) Entries(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]Entry, error) {
	out := map[uuid.UUID]Entry{}
	if s.db == nil {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT operation_id, gross_minor, withheld_minor FROM wage_entries WHERE space_id = $1`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var e Entry
		if err := rows.Scan(&id, &e.Gross, &e.Withheld); err != nil {
			return nil, err
		}
		out[id] = e
	}
	return out, rows.Err()
}
