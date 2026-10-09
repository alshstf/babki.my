package jobs

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/platform/testdb"
)

// A shown job has its row while it runs, with what it says about itself, and
// none once it ends — failed or not. Another job writes nothing, and its
// Progress records nothing.
func TestAShownJobHasItsProgressRowOnlyWhileItRuns(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	m := newProgressMiddleware(pool, []string{"shown"}, slog.Default())
	space, account := uuid.New(), uuid.New()

	row := func() (stage string, done, total int, spaceID *uuid.UUID, accounts []uuid.UUID, ok bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT stage, done, total, space_id, account_ids FROM job_progress WHERE job_id = 7`).
			Scan(&stage, &done, &total, &spaceID, &accounts)
		return stage, done, total, spaceID, accounts, err == nil
	}

	failure := errors.New("broker down")
	err := m.Work(ctx, &rivertype.JobRow{ID: 7, Kind: "shown"}, func(ctx context.Context) error {
		if _, _, _, _, _, ok := row(); !ok {
			t.Error("no row while the job runs")
		}
		p := ProgressFrom(ctx)
		p.Scope(ctx, &space, []uuid.UUID{account})
		p.Stage(ctx, "journal", 2, 5)
		stage, done, total, spaceID, accounts, _ := row()
		if stage != "journal" || done != 2 || total != 5 || spaceID == nil || *spaceID != space ||
			len(accounts) != 1 || accounts[0] != account {
			t.Errorf("row = %q %d/%d, space %v, accounts %v; want journal 2/5 of the space's account",
				stage, done, total, spaceID, accounts)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Errorf("Work returned %v, want the job's own error", err)
	}
	if _, _, _, _, _, ok := row(); ok {
		t.Error("the row outlived its job")
	}

	if err := m.Work(ctx, &rivertype.JobRow{ID: 8, Kind: "other"}, func(ctx context.Context) error {
		ProgressFrom(ctx).Stage(ctx, "x", 1, 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_progress`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows (%v), want none for a job the screen does not show", n, err)
	}
}

// Within a stage a job writes at most once a second, but always its last unit
// and always a new stage.
func TestAStageWritesAtMostOnceASecondButAlwaysItsEnd(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO job_progress (job_id, kind) VALUES (9, 'shown')`); err != nil {
		t.Fatal(err)
	}
	p := &Progress{pool: pool, jobID: 9, log: slog.Default()}
	done := func() (string, int) {
		t.Helper()
		var stage string
		var d int
		if err := pool.QueryRow(ctx, `SELECT stage, done FROM job_progress WHERE job_id = 9`).Scan(&stage, &d); err != nil {
			t.Fatal(err)
		}
		return stage, d
	}
	p.Stage(ctx, "journal", 1, 100)
	p.Stage(ctx, "journal", 2, 100)
	if _, d := done(); d != 1 {
		t.Errorf("done = %d, want 1: the second write came within the second", d)
	}
	p.Stage(ctx, "journal", 100, 100)
	if _, d := done(); d != 100 {
		t.Errorf("done = %d, want 100: the last unit is always written", d)
	}
	p.Stage(ctx, "write", 0, 0)
	if s, _ := done(); s != "write" {
		t.Errorf("stage = %q, want write: a new stage is always written", s)
	}
}

// The screen sees its own space's jobs and the whole instance's, not another
// space's nor a row whose process stopped confirming it.
func TestRunningListsTheSpacesAndTheInstancesLiveJobs(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	mine, theirs := uuid.New(), uuid.New()
	for _, r := range []struct {
		id    int64
		space *uuid.UUID
		age   time.Duration
	}{
		{1, &mine, 0},
		{2, nil, 0},
		{3, &theirs, 0},
		{4, &mine, ProgressStale + time.Minute},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO job_progress (job_id, kind, space_id, started_at, updated_at)
			VALUES ($1, 'shown', $2, now() - $3::interval, now() - $3::interval)`, r.id, r.space, r.age.String()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RunningFor(ctx, pool, mine)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range got {
		ids = append(ids, r.JobID)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("listed %v, want [1 2]: the space's own and the instance's, live", ids)
	}
}
