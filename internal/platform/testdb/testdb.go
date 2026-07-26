// Package testdb spins up a one-time Postgres instance in Docker for tests.
package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"babki.my/babki/internal/platform/db"
)

// New returns a connection pool to a clean database in a postgres:17-alpine container.
// If Docker is unavailable, t.Skip is called. The container is cleaned up in t.Cleanup.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("babki_test"),
		tcpostgres.WithUsername("babki"),
		tcpostgres.WithPassword("babki"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("skip: docker/testcontainers unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	// Use db.PoolConfig (not pgxpool.New) so the test pool registers the same
	// pgx type codecs as production — notably shopspring/decimal <-> NUMERIC —
	// which store tests rely on when scanning into *decimal.Decimal fields.
	cfg, err := db.PoolConfig(url)
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
