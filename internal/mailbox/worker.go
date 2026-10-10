package mailbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// CheckArgs is the hourly job that reads every space's mailbox for receipts.
type CheckArgs struct{}

func (CheckArgs) Kind() string { return "mailbox.check" }

type worker struct {
	river.WorkerDefaults[CheckArgs]
	svc *Service
	log *slog.Logger
}

// NewWorker reads the mailboxes; one box's failure is noted on it and the
// others are read all the same.
func NewWorker(svc *Service, log *slog.Logger) river.Worker[CheckArgs] {
	return &worker{svc: svc, log: log}
}

func (w *worker) Timeout(*river.Job[CheckArgs]) time.Duration { return 15 * time.Minute }

func (w *worker) Work(ctx context.Context, _ *river.Job[CheckArgs]) error {
	spaces, err := w.svc.Spaces(ctx)
	if err != nil {
		return err
	}
	for _, id := range spaces {
		if _, _, err := w.svc.Check(ctx, id); err != nil {
			w.log.Error("mailbox: check failed", "space", id, "error", err)
		}
	}
	return nil
}
