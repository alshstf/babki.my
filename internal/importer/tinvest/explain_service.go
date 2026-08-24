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
// IT REPLACES WHAT THOSE ROWS ALREADY PRODUCED, in the journal's own single
// transaction. Some of the rows an owner explains are not unparsed at all: the
// importer read them, believed them and booked them — a fund's partial
// redemption arrives as a withdrawal of 44 380,35 units "to another
// depositary", which the projection records as a transfer_out. The owner's own
// redemption of those same units cannot be written while that transfer holds
// them, and until this call named them for replacement the answer was «not
// enough quantity: have 16414.65, need 44380.35» — the feature refusing its own
// headline case. So the entries those rows produced are named here (see
// EntriesOfRows) and go in the same transaction the operation arrives in.
//
// THE EXPLANATION ROWS ARE STILL A SECOND WRITE, and cannot honestly be made
// part of that transaction: the operation service owns it, together with the
// account locks that make the engine replay mean anything, and reaching past it
// would be a second write path into the journal. What stands in is the order,
// the undo and the rebuild — every key is checked against this link BEFORE the
// operation is written; an explanation that still cannot be written (the race:
// another request explained one of these rows meanwhile) takes the operation
// back out; and the sync that follows re-projects the mirror, which puts the
// replaced entries back, since nothing now explains those rows. A failure to
// undo is reported rather than swallowed: at that point the journal holds an
// operation explaining nothing, and only saying so is honest.
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

	// What the old reading of these rows put in the journal, so that it goes in
	// the same transaction the new one arrives in. A row that produced nothing —
	// every unparsed row, and that is most of them — contributes nothing here,
	// and the call is then an ordinary Create.
	replaced, err := s.entriesOfRows(ctx, p.SpaceID, link.AccountID, rows)
	if err != nil {
		return Explanation{}, false, err
	}

	created, err := s.journal.CreateReplacing(ctx, p.SpaceID, op, replaced)
	if err != nil {
		return Explanation{}, false, err
	}
	if err := s.store.CreateExplanations(ctx, linkID, created.ID, contentKeys); err != nil {
		// Deleting the operation restores the journal to what it was: the
		// entries removed with it come back on the sync below, because nothing
		// explains their rows any more and the projection is a pure function of
		// the mirror. That is why the sync is queued on this path too.
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

// entriesOfRows is the journal entries these mirror rows produced, which an
// explanation replaces.
//
// It reads the whole imported journal of the account and picks by name, rather
// than asking the database for the names it wants: the account's imported rows
// are what the rebuild reads on every sync anyway, and a query shaped around
// the name's own syntax would put the projection's naming rule into SQL as
// well — see externalIDPrefix for why there is exactly one statement of it.
func (s *Service) entriesOfRows(ctx context.Context, spaceID, accountID uuid.UUID, rows []MirrorRow) (
	[]uuid.UUID, error,
) {
	journal, err := s.entries.ListBySource(ctx, spaceID, accountID, Source)
	if err != nil {
		return nil, fmt.Errorf("tinvest: read the imported journal of account %s: %w", accountID, err)
	}
	return EntriesOfRows(journal, rows), nil
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
