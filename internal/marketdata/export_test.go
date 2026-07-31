package marketdata

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// mustBackfillFxWorker builds a backfillFxWorker via the public constructor
// and unwraps it back to the concrete type, so test-only helpers can poke at
// its unexported fields (pause, now). NewBackfillFxWorker always returns
// *backfillFxWorker in practice; the assertion is checked (not blindly
// trusted) because a silent nil dereference on a failed assertion would be a
// far more confusing test failure than this panic.
func mustBackfillFxWorker(store *Store, ops operationDater, provider FxProvider, log *slog.Logger) *backfillFxWorker {
	worker := NewBackfillFxWorker(store, ops, provider, log)
	w, ok := worker.(*backfillFxWorker)
	if !ok {
		panic(fmt.Sprintf("NewBackfillFxWorker returned %T, want *backfillFxWorker", worker))
	}
	return w
}

// NewBackfillFxWorkerWithPause is NewBackfillFxWorker with a caller-chosen
// pause between provider requests. Test-only (this file is compiled into the
// test binary only): a test that exercises a whole chunk would otherwise
// spend backfillChunkDays * backfillPause — three quarters of a minute —
// asleep, on every run of the suite.
func NewBackfillFxWorkerWithPause(
	store *Store, ops operationDater, provider FxProvider, log *slog.Logger, pause time.Duration,
) river.Worker[BackfillFxArgs] {
	w := mustBackfillFxWorker(store, ops, provider, log)
	w.pause = pause
	return w
}

// NewBackfillFxWorkerWithClock is NewBackfillFxWorkerWithPause with a
// caller-chosen clock in place of time.Now. Test-only: the worker's
// "today" (used when there's no coverage yet) is otherwise read from the
// wall clock independently of whatever a test computes as its own
// expectation of "today", and a run straddling UTC midnight between those
// two reads would disagree on the date by exactly one day.
func NewBackfillFxWorkerWithClock(
	store *Store, ops operationDater, provider FxProvider, log *slog.Logger, pause time.Duration, now func() time.Time,
) river.Worker[BackfillFxArgs] {
	w := mustBackfillFxWorker(store, ops, provider, log)
	w.pause = pause
	w.now = now
	return w
}
