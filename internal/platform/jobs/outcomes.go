package jobs

import (
	"context"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// maxErrorRunes is how much of a failure's text is kept: enough to say what
// went wrong, not a whole response body.
const maxErrorRunes = 300

// outcomeHook records how every attempt of a job ended (see job_outcomes). A
// failed write is logged and never fails the job: the record is about the
// job, not part of its work.
type outcomeHook struct {
	river.HookDefaults
	pool *pgxpool.Pool
	log  *slog.Logger
}

var _ rivertype.HookWorkEnd = (*outcomeHook)(nil)

func (h *outcomeHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) error {
	if job.Kind == (HeartbeatArgs{}).Kind() {
		return err
	}
	var werr error
	if err == nil {
		_, werr = h.pool.Exec(ctx, `
			INSERT INTO job_outcomes (kind, last_success_at) VALUES ($1, now())
			ON CONFLICT (kind) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`, job.Kind)
	} else {
		_, werr = h.pool.Exec(ctx, `
			INSERT INTO job_outcomes (kind, last_failure_at, last_error) VALUES ($1, now(), $2)
			ON CONFLICT (kind) DO UPDATE SET last_failure_at = EXCLUDED.last_failure_at, last_error = EXCLUDED.last_error`,
			job.Kind, truncateRunes(err.Error(), maxErrorRunes))
	}
	if werr != nil {
		h.log.Warn("jobs: recording how a job ended failed", "kind", job.Kind, "err", werr)
	}
	return err
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// Source is one source of outside data, as the jobs that fetch it last ended.
type Source struct {
	Kind          string
	Every         time.Duration
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	LastError     string
}

// Stale reports whether the source has not succeeded for three of its
// intervals — or never has, while it has failed. A source that has neither
// succeeded nor failed has not run yet.
func (s Source) Stale(now time.Time) bool {
	if s.LastSuccessAt == nil {
		return s.LastFailureAt != nil
	}
	return now.Sub(*s.LastSuccessAt) > 3*s.Every
}

// SourceKind is a job that fetches data from outside, with how often it runs.
type SourceKind struct {
	Kind  string
	Every time.Duration
}

// Sources reads how each of kinds last ended, in the order given.
func Sources(ctx context.Context, pool *pgxpool.Pool, kinds []SourceKind) ([]Source, error) {
	rows, err := pool.Query(ctx, `SELECT kind, last_success_at, last_failure_at, last_error FROM job_outcomes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKind := map[string]Source{}
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.Kind, &s.LastSuccessAt, &s.LastFailureAt, &s.LastError); err != nil {
			return nil, err
		}
		byKind[s.Kind] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(kinds))
	for _, k := range kinds {
		s := byKind[k.Kind]
		s.Kind, s.Every = k.Kind, k.Every
		out = append(out, s)
	}
	return out, nil
}
