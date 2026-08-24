package tinvest

import (
	"testing"

	"github.com/google/uuid"
)

// TestARegistryWriteToALinkedAccountQueuesAFreshCheck. A verdict is a sentence
// about the journal at the moment it was struck, and the registry changes
// journals underneath it: on the live stand the FXUS split landed three seconds
// after that account's reconciliation had finished, and the screen went on
// reporting a difference of 3 771 against 8 830 when the journal had just
// become 8 820.
func TestARegistryWriteToALinkedAccountQueuesAFreshCheck(t *testing.T) {
	f := newFixture(t)
	inserter := &fakeInserter{}
	r := NewRechecker(f.store, inserter, nil)

	queued, err := r.QueueRecheckForAccounts(f.ctx, []uuid.UUID{f.accountID})
	if err != nil {
		t.Fatalf("QueueRecheckForAccounts: %v", err)
	}
	if queued != 1 {
		t.Errorf("queued %d checks, want 1", queued)
	}
	jobs := inserter.queued()
	if len(jobs) != 1 {
		t.Fatalf("the queue took %d jobs, want 1: %+v", len(jobs), jobs)
	}
	if jobs[0].ConnectionID != f.conn.ID {
		t.Errorf("queued a check of connection %s, want %s", jobs[0].ConnectionID, f.conn.ID)
	}
	// THE TRIGGER IS THE REGISTRY'S OWN WORD. Filing this under `schedule` or
	// `manual` would put a sentence on the run log that the run contradicts:
	// the clock did not strike and nobody pressed anything.
	if jobs[0].Trigger != string(TriggerRegistry) {
		t.Errorf("queued under trigger %q, want %q", jobs[0].Trigger, TriggerRegistry)
	}
}

// TestARegistryWriteToAnUnlinkedAccountQueuesNothing. Most accounts in most
// instances are nobody's broker link — a household keeping its own records has
// no connection at all — and a registry write there has no verdict to make
// stale.
func TestARegistryWriteToAnUnlinkedAccountQueuesNothing(t *testing.T) {
	f := newFixture(t)
	inserter := &fakeInserter{}
	r := NewRechecker(f.store, inserter, nil)

	queued, err := r.QueueRecheckForAccounts(f.ctx, []uuid.UUID{uuid.New()})
	if err != nil {
		t.Fatalf("QueueRecheckForAccounts: %v", err)
	}
	if queued != 0 {
		t.Errorf("queued %d checks for an account no connection feeds, want 0", queued)
	}
	if jobs := inserter.queued(); len(jobs) != 0 {
		t.Errorf("the queue took %d jobs for an account no connection feeds: %+v", len(jobs), jobs)
	}
}

// TestASecondWriteWhileACheckIsAlreadyQueuedAddsNothing. The sync is unique per
// connection across every unfinished state, so a sweep touching forty accounts
// of one connection asks for one run — and the one already waiting will read the
// journal as it stands when it runs, which is after this write.
func TestASecondWriteWhileACheckIsAlreadyQueuedAddsNothing(t *testing.T) {
	f := newFixture(t)
	inserter := &fakeInserter{dup: true}
	r := NewRechecker(f.store, inserter, nil)

	queued, err := r.QueueRecheckForAccounts(f.ctx, []uuid.UUID{f.accountID})
	if err != nil {
		t.Fatalf("QueueRecheckForAccounts: %v", err)
	}
	if queued != 0 {
		t.Errorf("counted %d newly queued checks though the queue reported a duplicate, want 0", queued)
	}
}
