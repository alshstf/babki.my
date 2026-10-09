package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Progress is what a running job the screen shows says about itself
// (job_progress): whose it is, which stage it is in and how far through. A job
// the screen does not show, or one run outside the queue, gets one that records
// nothing, so a worker reports without asking which it is. A failed write is
// logged and never fails the job.
type Progress struct {
	pool  *pgxpool.Pool
	jobID int64
	log   *slog.Logger

	mu      sync.Mutex
	stage   string
	written time.Time
}

type progressKey struct{}

// ProgressFrom is the running job's progress; never nil.
func ProgressFrom(ctx context.Context) *Progress {
	if p, ok := ctx.Value(progressKey{}).(*Progress); ok {
		return p
	}
	return &Progress{}
}

// Scope says whose job this is: the space (nil for the whole instance, which
// every space sees) and the accounts whose figures are not final until it ends.
func (p *Progress) Scope(ctx context.Context, spaceID *uuid.UUID, accounts []uuid.UUID) {
	if p.pool == nil {
		return
	}
	if accounts == nil {
		accounts = []uuid.UUID{}
	}
	p.write(ctx, `UPDATE job_progress SET space_id = $2, account_ids = $3, updated_at = now() WHERE job_id = $1`,
		spaceID, accounts)
}

// Stage says which stage the job is in and how far through it: done of total,
// total 0 when the stage cannot be counted. Within a stage it writes at most
// once a second, and always on the last unit: a job naming every row it reads
// must not write every row.
func (p *Progress) Stage(ctx context.Context, stage string, done, total int) {
	if p.pool == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	skip := stage == p.stage && now.Sub(p.written) < time.Second && (total == 0 || done < total)
	if !skip {
		p.stage, p.written = stage, now
	}
	p.mu.Unlock()
	if skip {
		return
	}
	p.write(ctx, `UPDATE job_progress SET stage = $2, done = $3, total = $4, updated_at = now() WHERE job_id = $1`,
		stage, max(done, 0), max(total, 0))
}

func (p *Progress) write(ctx context.Context, sql string, args ...any) {
	if _, err := p.pool.Exec(ctx, sql, append([]any{p.jobID}, args...)...); err != nil {
		p.log.Warn("jobs: recording a job's progress failed", "job", p.jobID, "err", err)
	}
}

// progressBeat is how often a running job's row is confirmed alive, and
// ProgressStale how old an unconfirmed row may be before it is no longer shown:
// its process died and nothing will delete it.
const (
	progressBeat  = 30 * time.Second
	ProgressStale = 2 * time.Minute
)

// progressMiddleware keeps a job_progress row for each job of a shown kind
// while it runs, and hands the job its Progress.
type progressMiddleware struct {
	river.MiddlewareDefaults
	pool  *pgxpool.Pool
	shown map[string]bool
	log   *slog.Logger
	beat  time.Duration
}

var _ rivertype.WorkerMiddleware = (*progressMiddleware)(nil)

func newProgressMiddleware(pool *pgxpool.Pool, shown []string, log *slog.Logger) *progressMiddleware {
	m := &progressMiddleware{pool: pool, shown: map[string]bool{}, log: log, beat: progressBeat}
	for _, kind := range shown {
		m.shown[kind] = true
	}
	return m
}

func (m *progressMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if !m.shown[job.Kind] {
		return doInner(ctx)
	}
	// A retry is the same job: its row starts over. Rows of jobs whose process
	// died go here too, long after they stopped being shown.
	if _, err := m.pool.Exec(ctx, `
		WITH gone AS (DELETE FROM job_progress WHERE updated_at < now() - interval '1 hour')
		INSERT INTO job_progress (job_id, kind) VALUES ($1, $2)
		ON CONFLICT (job_id) DO UPDATE SET stage = '', done = 0, total = 0, started_at = now(), updated_at = now()`,
		job.ID, job.Kind); err != nil {
		m.log.Warn("jobs: recording that a job started failed", "job", job.ID, "kind", job.Kind, "err", err)
		return doInner(ctx)
	}
	stop := make(chan struct{})
	beaten := make(chan struct{})
	go func() {
		defer close(beaten)
		t := time.NewTicker(m.beat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, err := m.pool.Exec(ctx, `UPDATE job_progress SET updated_at = now() WHERE job_id = $1`, job.ID); err != nil {
					m.log.Warn("jobs: confirming a running job failed", "job", job.ID, "err", err)
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-beaten
		// Without the job's cancellation: a job stopped by shutdown is gone too.
		if _, err := m.pool.Exec(context.WithoutCancel(ctx), `DELETE FROM job_progress WHERE job_id = $1`, job.ID); err != nil {
			m.log.Warn("jobs: recording that a job ended failed", "job", job.ID, "err", err)
		}
	}()
	return doInner(context.WithValue(ctx, progressKey{}, &Progress{pool: m.pool, jobID: job.ID, log: m.log}))
}

// Running is one job the screen shows, as it last reported.
type Running struct {
	JobID      int64
	Kind       string
	SpaceID    *uuid.UUID
	AccountIDs []uuid.UUID
	Stage      string
	Done       int
	Total      int
	StartedAt  time.Time
}

// RunningFor lists the shown jobs running for space and for the whole
// instance, oldest first; a row not confirmed for ProgressStale is left out.
func RunningFor(ctx context.Context, pool *pgxpool.Pool, spaceID uuid.UUID) ([]Running, error) {
	rows, err := pool.Query(ctx, `
		SELECT job_id, kind, space_id, account_ids, stage, done, total, started_at FROM job_progress
		WHERE (space_id = $1 OR space_id IS NULL) AND updated_at > now() - make_interval(secs => $2)
		ORDER BY started_at, job_id`, spaceID, ProgressStale.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Running{}
	for rows.Next() {
		var r Running
		if err := rows.Scan(&r.JobID, &r.Kind, &r.SpaceID, &r.AccountIDs, &r.Stage, &r.Done, &r.Total, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
