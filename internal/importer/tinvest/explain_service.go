package tinvest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// ExplainRows records that one manual operation accounts for these mirror rows
// and asks the connection to rebuild.
//
// The broker sends no corporate actions, so an event arrives as whatever rows
// carried its money: on the owner's account a fund's partial redemption came as a
// withdrawal of 44 380,35 units "to another depositary" and, a fortnight later,
// 2 559,80 ₽ under the bond-redemption type. The owner says what they were; those
// rows stop being projected and the operation stands instead.
//
// The operation goes through the journal's own door and is refused as the journal
// screen would refuse it. It replaces, in the journal's transaction, what those
// rows already produced (entriesOfRows): the redemption cannot be written while
// the transfer_out still holds the units.
//
// The explanation rows are a second write outside that transaction. Keys are
// checked first; if the explanation still cannot be written (another request
// raced), the operation is deleted again, and the following sync re-projects the
// replaced entries. A failed undo is reported.
func (s *Service) ExplainRows(ctx context.Context, p family.Principal, linkID uuid.UUID,
	contentKeys []string, op operation.Operation,
) (Explanation, bool, error) {
	if err := requireOwner(p); err != nil {
		return Explanation{}, false, err
	}
	if len(contentKeys) == 0 {
		return Explanation{}, false, fmt.Errorf("%w: name at least one broker operation to explain", family.ErrValidation)
	}
	link, err := s.store.LinkByID(ctx, p.SpaceID, linkID)
	if err != nil {
		return Explanation{}, false, err
	}
	// The operation belongs to the linked account, whatever the request
	// named.
	op.AccountID = link.AccountID

	rows, err := s.store.MirrorRowsByKeys(ctx, linkID, contentKeys)
	if err != nil {
		return Explanation{}, false, err
	}
	found := map[string]bool{}
	for _, m := range rows {
		found[m.ContentKey] = true
	}
	for _, key := range contentKeys {
		if !found[key] {
			return Explanation{}, false, fmt.Errorf("%w: %q", ErrRowNotInLink, key)
		}
	}
	if err := s.store.attachExplanations(ctx, rows); err != nil {
		return Explanation{}, false, err
	}
	for _, m := range rows {
		if m.ExplainedBy != nil {
			return Explanation{}, false, fmt.Errorf("%w: %q", ErrRowAlreadyExplained, m.ContentKey)
		}
	}

	// What these rows produced, replaced in the same transaction; unparsed
	// rows produced nothing and it is a plain Create.
	replaced, err := s.entriesOfRows(ctx, p.SpaceID, link.AccountID, rows)
	if err != nil {
		return Explanation{}, false, err
	}

	created, err := s.journal.CreateReplacing(ctx, p.SpaceID, op, replaced)
	if err != nil {
		return Explanation{}, false, err
	}
	if err := s.store.CreateExplanations(ctx, linkID, created.ID, contentKeys); err != nil {
		// Deleting the operation restores the journal: the replaced entries come
		// back on the sync below, since nothing explains their rows any more.
		if undo := s.journal.Delete(ctx, p.SpaceID, created.ID); undo != nil {
			s.log.Error("tinvest: an explanation could not be written and its operation could not be taken back",
				"operation", created.ID, "link", linkID, "err", err, "undo_err", undo)
			return Explanation{}, false, fmt.Errorf("tinvest: explanation not written and operation %s left in the journal: %w", created.ID, err)
		}
		if _, requeue := s.rebuildAfterExplanations(ctx, link.ConnectionID); requeue != nil {
			s.log.Error("tinvest: an explanation was undone but the rebuild that restores its rows could not be queued",
				"link", linkID, "err", err, "requeue_err", requeue)
		}
		return Explanation{}, false, err
	}

	queued, err := s.rebuildAfterExplanations(ctx, link.ConnectionID)
	if err != nil {
		return Explanation{}, false, err
	}
	return Explanation{
		LinkID:       linkID,
		ConnectionID: link.ConnectionID,
		SpaceID:      link.SpaceID,
		OperationID:  created.ID,
	}, queued, nil
}

// RemoveExplanation deletes the manual operation; its explanation rows go with
// it by ON DELETE CASCADE, and the next rebuild projects those mirror rows again.
// An explanation cannot outlive its operation, however the operation is
// deleted.
func (s *Service) RemoveExplanation(ctx context.Context, p family.Principal, id uuid.UUID) (bool, error) {
	if err := requireOwner(p); err != nil {
		return false, err
	}
	e, err := s.store.ExplanationByID(ctx, id)
	if err != nil {
		return false, err
	}
	if e.SpaceID != p.SpaceID {
		// Not "forbidden": that would confirm it exists elsewhere.
		return false, ErrExplanationNotFound
	}
	if err := s.journal.Delete(ctx, p.SpaceID, e.OperationID); err != nil {
		return false, err
	}
	return s.rebuildAfterExplanations(ctx, e.ConnectionID)
}

// entriesOfRows is the journal entries these mirror rows produced. It reads the
// account's imported journal and picks by name, keeping the naming rule out of
// SQL (see externalIDPrefix).
func (s *Service) entriesOfRows(ctx context.Context, spaceID, accountID uuid.UUID, rows []MirrorRow) (
	[]uuid.UUID, error,
) {
	journal, err := s.entries.ListBySource(ctx, spaceID, accountID, Source)
	if err != nil {
		return nil, fmt.Errorf("tinvest: read the imported journal of account %s: %w", accountID, err)
	}
	return EntriesOfRows(journal, rows), nil
}

// rebuildAfterExplanations queues the ordinary sync, the one path with a broker
// client for resolving instruments. An inactive connection is not an error: the
// explanation takes effect on its next run, which false says.
func (s *Service) rebuildAfterExplanations(ctx context.Context, connID uuid.UUID) (bool, error) {
	conn, err := s.store.connectionForSync(ctx, connID)
	if err != nil {
		return false, err
	}
	if conn.Status != StatusActive {
		return false, nil
	}
	res, err := EnqueueSync(ctx, s.inserter, connID, TriggerManual)
	if err != nil {
		return false, fmt.Errorf("tinvest: queue a sync after an explanation: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}
