package marketdata

import (
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// NewBackfillFxWorkerWithPause is NewBackfillFxWorker with a caller-chosen
// pause between provider requests. Test-only (this file is compiled into the
// test binary only): a test that exercises a whole chunk would otherwise
// spend backfillChunkDays * backfillPause — three quarters of a minute —
// asleep, on every run of the suite.
func NewBackfillFxWorkerWithPause(
	store *Store, ops operationDater, provider FxProvider, log *slog.Logger, pause time.Duration,
) river.Worker[BackfillFxArgs] {
	w, _ := NewBackfillFxWorker(store, ops, provider, log).(*backfillFxWorker)
	w.pause = pause
	return w
}
