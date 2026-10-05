package tinvest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Rechecker queues a fresh check of the connections whose accounts another
// writer has just changed.
//
// A reconciliation verdict describes the journal when it was struck. The
// corporate-actions registry changes journals in between: on the live stand it
// wrote FXUS's 1:100 split three seconds after that account's reconciliation,
// which then reported a difference against 8 830 while the journal held 8 820.
//
// It queues a whole sync, not a reconcile-only job: a sync rereads the mirror and
// rebuilds (idempotent for an unchanged history) before comparing, so one piece
// of code produces every verdict. SyncInsertOpts keeps it to one queued run per
// connection.
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

// QueueRecheckForAccounts queues a sync for every connection reconciling one
// of these accounts and returns how many. Most accounts have no connection and
// queue nothing. The journal is already written, so an error is for the caller
// to log, never to undo by; a stale verdict lasts until the hourly run.
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
			// Already queued; it will read the journal after this write.
			r.log.Debug("tinvest: a check of this connection was already queued when the registry wrote to it",
				"connection", connID)
			continue
		}
		queued++
	}
	return queued, nil
}
