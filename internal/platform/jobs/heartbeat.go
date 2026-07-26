package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// HeartbeatArgs is a periodic pulse job confirming that a worker is alive.
type HeartbeatArgs struct{}

func (HeartbeatArgs) Kind() string { return "heartbeat" }

type heartbeatWorker struct {
	river.WorkerDefaults[HeartbeatArgs]
	log  *slog.Logger
	pool *pgxpool.Pool
}

func (w *heartbeatWorker) Work(ctx context.Context, job *river.Job[HeartbeatArgs]) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := w.pool.Exec(ctx, `
		INSERT INTO meta (key, value) VALUES ('last_heartbeat_at', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, now)
	if err != nil {
		return err
	}
	w.log.Debug("heartbeat", "at", now)
	return nil
}
