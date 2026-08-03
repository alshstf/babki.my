package marketdata

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// NewStoreForRows is NewStore over a caller-supplied source of rows in place
// of a connection pool. Test-only (this file is compiled into the test binary
// only): it exists so a reader can be run against a result set that yields
// rows and then fails, which is what a connection dying mid-stream looks like
// and which no real Postgres fixture can be asked to reproduce on demand. The
// parameter type is unexported, but an external test package can still pass
// any value whose method set satisfies it — see store_truncated_test.go.
func NewStoreForRows(q querier) *Store { return &Store{db: q} }

// mustBackfillFxWorker builds a backfillFxWorker via the public constructor
// and unwraps it back to the concrete type, so test-only helpers can poke at
// its unexported fields (now). NewBackfillFxWorker always returns
// *backfillFxWorker in practice; the assertion is checked (not blindly
// trusted) because a silent nil dereference on a failed assertion would be a
// far more confusing test failure than this panic.
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

// NewBackfillFxWorkerWithClock is NewBackfillFxWorker with a caller-chosen
// clock in place of time.Now. Test-only (this file is compiled into the test
// binary only): the worker's "today" — the upper end of every range it
// requests — is otherwise read from the wall clock independently of whatever
// a test computes as its own expectation of "today", and a run straddling UTC
// midnight between those two reads would disagree on the date by exactly one
// day.
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
