package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// TestHeartbeat verifies that the River client starts, the periodic
// heartbeat job (RunOnStart) executes, and leaves a mark in meta.
func TestHeartbeat(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	workers := jobs.NewWorkers(slog.Default(), pool)
	client, err := jobs.NewClient(pool, workers, slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var v string
		err := pool.QueryRow(ctx,
			`SELECT value FROM meta WHERE key = 'last_heartbeat_at'`).Scan(&v)
		if err == nil && v != "" {
			return // success
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("heartbeat did not run within 15s")
}
