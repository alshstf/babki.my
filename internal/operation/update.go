package operation

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

// Update rewrites a hand-entered operation in place: same row, same account,
// same type, and the same moment of recording, so it keeps its place among the
// operations of its day. The journal is checked with the row as it would stand,
// under the account's lock, exactly as Create checks a new one.
//
// Only a single row a person owns can be edited — entered by hand or loaded
// from their table (OwnedByHand). A broker's row is the
// importer's (it would be rewritten by the next sync); a transfer, a conversion
// or a spin-off is a pair whose halves must agree, and shares that arrived from
// another broker carry their purchases separately — those are deleted and
// entered again.
func (s *Service) Update(ctx context.Context, spaceID, id uuid.UUID, op Operation) (Operation, error) {
	if err := normalizeForStorage(&op); err != nil {
		return Operation{}, err
	}
	if err := validate(op); err != nil {
		return Operation{}, err
	}
	current, err := s.store.ByID(ctx, spaceID, id)
	if err != nil {
		return Operation{}, err
	}

	var updated Operation
	err = s.store.WithOpenAccountsLocked(ctx, spaceID, []uuid.UUID{current.AccountID}, func(st *Store) error {
		old, err := st.ByID(ctx, spaceID, id)
		if err != nil {
			return err
		}
		if err := editable(old, op); err != nil {
			return err
		}
		op.ID, op.SpaceID, op.Source, op.CreatedAt = old.ID, old.SpaceID, old.Source, old.CreatedAt
		if op.OccurredOn.Equal(old.OccurredOn) {
			op.OccurredAt = old.OccurredAt
		}

		journal, err := st.ListForEngine(ctx, spaceID, old.AccountID)
		if err != nil {
			return err
		}
		if _, err := portfolio.Compute(journalReplacing(journal, op)); err != nil {
			return fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		stored, err := st.update(ctx, spaceID, op)
		if err != nil {
			return err
		}
		// What was checked must be what was written (see Create): replayed
		// again with the row as the columns hold it, before the commit.
		if _, err := portfolio.Compute(journalReplacing(journal, stored)); err != nil {
			return fmt.Errorf("the operation as stored no longer replays on account %s: %v", old.AccountID, err)
		}
		updated = stored
		return nil
	})
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	s.manualWriteDone(ctx, spaceID, updated.AccountID)
	return updated, nil
}

// editable refuses an edit this program cannot make in place.
func editable(old, op Operation) error {
	switch {
	case !OwnedByHand(old.Source):
		return fmt.Errorf("%w: imported operations are managed by the importer", family.ErrValidation)
	case old.TransferGroupID != nil || carriesCostBasis(old):
		return fmt.Errorf("%w: a transfer, a conversion or shares from another broker are deleted and entered again, not edited",
			family.ErrValidation)
	case op.AccountID != old.AccountID:
		return fmt.Errorf("%w: account_id cannot change; delete the operation and enter it on the other account",
			family.ErrValidation)
	case op.Type != old.Type:
		return fmt.Errorf("%w: type cannot change; delete the operation and enter the new one", family.ErrValidation)
	}
	return nil
}

// update writes op's fields over the row of the same id in spaceID, leaving
// its account, type, source and moment of recording alone. An instant the row
// carried goes when its day changes: it was the instant of the old day.
func (s *Store) update(ctx context.Context, spaceID uuid.UUID, op Operation) (Operation, error) {
	return scan(s.db.QueryRow(ctx, `
		UPDATE operations SET
			instrument_id = $3, occurred_on = $4, settled_on = $5, quantity = $6,
			price = $7, amount_minor = $8, currency = $9, fee_minor = $10,
			note = $11, split_ratio = $12,
			occurred_at = CASE WHEN occurred_on = $4 THEN occurred_at END
		WHERE space_id = $1 AND id = $2
		RETURNING `+cols,
		spaceID, op.ID, op.InstrumentID, op.OccurredOn, op.SettledOn, op.Quantity,
		op.Price, op.AmountMinor, op.Currency, op.FeeMinor, op.Note, op.SplitRatio))
}
