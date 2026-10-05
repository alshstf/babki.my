package marketdata

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// NewStoreForRows builds a Store over a caller-supplied row source, so tests
// can simulate a read failing mid-stream.
func NewStoreForRows(q querier) *Store { return &Store{db: q} }

// mustBackfillFxWorker builds the worker through the public constructor and
// returns the concrete type.
func mustBackfillFxWorker(
	store *Store,
	ops operationCurrencies,
	accounts accountCurrencies,
	spaces spaceCurrencies,
	provider FxHistoryProvider,
	log *slog.Logger,
) *backfillFxWorker {
	worker := NewBackfillFxWorker(store, ops, accounts, spaces, provider, log)
	w, ok := worker.(*backfillFxWorker)
	if !ok {
		panic(fmt.Sprintf("NewBackfillFxWorker returned %T, want *backfillFxWorker", worker))
	}
	return w
}

// NewBackfillFxWorkerWithClock pins the worker's today, so a run straddling
// midnight cannot disagree with the test's.
func NewBackfillFxWorkerWithClock(
	store *Store,
	ops operationCurrencies,
	accounts accountCurrencies,
	spaces spaceCurrencies,
	provider FxHistoryProvider,
	log *slog.Logger,
	now func() time.Time,
) river.Worker[BackfillFxArgs] {
	w := mustBackfillFxWorker(store, ops, accounts, spaces, provider, log)
	w.now = now
	return w
}
