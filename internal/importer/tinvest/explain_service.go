package tinvest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// ExplainRows records that one manual operation accounts for these mirror
// rows, and asks the connection to rebuild so the journal shows it.
//
// WHAT IT IS FOR: the broker sends no corporate actions at all, so a real
// event arrives as whatever rows carried its money — on the owner's own
// account, a fund's partial redemption came as a "withdrawal to another
// depositary" of 44 380,35 units and, a fortnight later, a payment of
// 2 559,80 ₽ under the bond-redemption type. No rule over those two rows can
// find the one operation they are. The owner can, and this is where that
// answer is put: the rows stop being projected, and the operation entered here
// is what the journal holds instead.
//
// THE OPERATION GOES THROUGH THE JOURNAL'S OWN DOOR, engine replay and all
// (see journalWriter). It is refused for exactly what the journal screen would
// refuse it for, and the refusal travels back unchanged.
//
// IT IS NOT ONE TRANSACTION, and cannot honestly be made one from here: the
// operation service owns its own, together with the account locks that make
// the engine replay mean anything, and reaching past it would be a second
// write path into the journal. What stands in for atomicity is the order and
// the undo — every key is checked against this link BEFORE the operation is
// written, and an explanation that still cannot be written (the race: another
// request explained one of these rows meanwhile) takes the operation back out
// again. A failure to undo is reported rather than swallowed: at that point
// the journal holds an operation explaining nothing, and only saying so is
// honest.
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
	// THE OPERATION BELONGS TO THE LINKED ACCOUNT, not to whichever account the
	// request named. An explanation on another account would leave the linked
	// one still missing the event and put the money where the broker never had
	// it — and the client cannot be the one to get this right, since it is this
	// link that decides.
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

	created, err := s.journal.Create(ctx, p.SpaceID, op)
	if err != nil {
		return Explanation{}, false, err
	}
	if err := s.store.CreateExplanations(ctx, linkID, created.ID, contentKeys); err != nil {
		if undo := s.journal.Delete(ctx, p.SpaceID, created.ID); undo != nil {
			s.log.Error("tinvest: an explanation could not be written and its operation could not be taken back",
				"operation", created.ID, "link", linkID, "err", err, "undo_err", undo)
			return Explanation{}, false, fmt.Errorf("tinvest: explanation not written and operation %s left in the journal: %w", created.ID, err)
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

// RemoveExplanation takes back what ExplainRows recorded: the manual operation
// goes, and with it — by the explanations table's own foreign key — every row
// it was accounting for, which the next rebuild projects again.
//
// DELETING THE OPERATION IS THE WHOLE ACTION. The explanation rows follow it
// through ON DELETE CASCADE rather than being deleted beside it, so there is
// no order in which this can half-happen and no second rule to keep in step:
// an explanation cannot outlive the operation it names, whether it is removed
// here or the operation is deleted from the journal screen.
func (s *Service) RemoveExplanation(ctx context.Context, p family.Principal, id uuid.UUID) (bool, error) {
	if err := requireOwner(p); err != nil {
		return false, err
	}
	e, err := s.store.ExplanationByID(ctx, id)
	if err != nil {
		return false, err
	}
	if e.SpaceID != p.SpaceID {
		// Not "forbidden": to this space the explanation does not exist, and
		// saying anything else would confirm that it exists somewhere else.
		return false, ErrExplanationNotFound
	}
	if err := s.journal.Delete(ctx, p.SpaceID, e.OperationID); err != nil {
		return false, err
	}
	return s.rebuildAfterExplanations(ctx, e.ConnectionID)
}

// rebuildAfterExplanations asks the connection to sync, which is what runs the
// projection again over a mirror whose explanations have changed.
//
// THE ORDINARY SYNC AND NOT A REBUILD OF ITS OWN. The projection needs the
// broker's passports to resolve instruments, so the one path that has a client
// in hand is the one that can run it; a rebuild-only door would be a second
// way into the journal with a second set of preconditions. A connection that
// is not active cannot be synced at all, and that is not an error here: the
// explanation is written and takes effect the next time the connection runs,
// which is what the false return says.
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
