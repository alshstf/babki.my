package jobs

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// ScheduleEntry is one line of the schedule as a test reads it.
type ScheduleEntry struct {
	Every time.Duration
	Args  river.JobArgs
	Opts  *river.InsertOpts
}

// Schedule exposes the schedule with the options each job is queued under.
func Schedule() []ScheduleEntry {
	out := make([]ScheduleEntry, 0, len(schedule()))
	for _, job := range schedule() {
		out = append(out, ScheduleEntry{Every: job.every, Args: job.args, Opts: scheduledOpts(job.every)})
	}
	return out
}

// AttemptsWithin exposes attemptsWithin.
func AttemptsWithin(interval time.Duration) int { return attemptsWithin(interval) }

// TinvestSyncInterval exposes how often a connection's sync is dispatched.
const TinvestSyncInterval = tinvestSyncInterval

// NewOutcomeHook exposes the hook that records how jobs end.
func NewOutcomeHook(pool *pgxpool.Pool, log *slog.Logger) rivertype.HookWorkEnd {
	return &outcomeHook{pool: pool, log: log}
}
