package tinvest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Rechecker asks for a fresh check of the connections whose accounts somebody
// else's write has just changed.
//
// # Why it exists
//
// The reconciliation verdict is a sentence about a moment: "your journal and
// the broker agree", or "they differ, by this much". It is computed at the end
// of a sync and stored on the run, and it stays on the screen until the next
// one. Anything that changes the journal in between makes it a statement about
// a journal that no longer exists.
//
// The corporate-actions registry does exactly that. On the live stand the
// registry wrote FXUS's 1:100 split into the owner's journal three seconds
// after that account's reconciliation had finished — both run at startup, and
// the reconciliation happened to go first — so the screen went on reporting a
// difference of 3 771 against 8 830 when the journal had just become 8 820. The
// figure was right when it was struck and wrong by the time anybody read it.
//
// # Why a whole sync and not a reconciliation on its own
//
// There is no reconcile-only job, and adding one would be a second path into
// the same three steps (read the mirror, rebuild the journal, compare with the
// broker) that could come to disagree with the first about any of them. A sync
// re-reads the mirror and rebuilds before it compares, which for an unchanged
// broker history is idempotent — the rebuild computes the same journal and
// applies an empty difference — so the cost of the extra work is one broker
// read, and the benefit is that a verdict is only ever produced by one piece of
// code.
//
// It is also self-limiting: SyncInsertOpts makes a sync unique per connection
// across every unfinished state, so a registry sweep that touches forty
// accounts of one connection queues ONE run, not forty.
type Rechecker struct {
	store *Store
	queue jobInserter
	log   *slog.Logger
}

func NewRechecker(store *Store, queue jobInserter, log *slog.Logger) *Rechecker {
	if log == nil {
		log = slog.Default()
	}
	return &Rechecker{store: store, queue: queue, log: log}
}

// QueueRecheckForAccounts queues a sync for every connection that reconciles
// one of these accounts, and returns how many it queued.
//
// AN ACCOUNT NO CONNECTION FEEDS QUEUES NOTHING, and that is most accounts in
// most instances: a household that keeps its own records has no broker link at
// all, and a registry write there has no verdict to make stale. Returning the
// count rather than a bare error is what lets the caller's log say "one" or
// "none" instead of "done".
//
// A FAILURE HERE IS NOT THE CALLER'S FAILURE. The journal has already been
// written by the time this runs; a queue that cannot take the job leaves a
// stale verdict on the screen until the hourly run replaces it, which is
// exactly where the program stood before this existed. So the error is
// returned for the caller to log and never for it to undo a write by.
func (r *Rechecker) QueueRecheckForAccounts(ctx context.Context, accountIDs []uuid.UUID) (int, error) {
	if len(accountIDs) == 0 {
		return 0, nil
	}
	connections, err := r.store.ConnectionsOfAccounts(ctx, accountIDs)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, connID := range connections {
		res, err := EnqueueSync(ctx, r.queue, connID, TriggerRegistry)
		if err != nil {
			return queued, fmt.Errorf("tinvest: queue a check of connection %s after a registry write: %w", connID, err)
		}
		if res.UniqueSkippedAsDuplicate {
			// A sync of this connection is already waiting. It will read the
			// journal as it stands when it runs, which is after this write, so
			// the verdict it produces is the fresh one — there is nothing to
			// queue and nothing to report as missed.
			r.log.Debug("tinvest: a check of this connection was already queued when the registry wrote to it",
				"connection", connID)
			continue
		}
		queued++
	}
	return queued, nil
}
