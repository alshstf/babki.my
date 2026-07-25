// Package testdb поднимает одноразовый Postgres в Docker для тестов.
package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// New возвращает пул к чистой БД в контейнере postgres:17-alpine.
// Если Docker недоступен — t.Skip. Контейнер убирается в t.Cleanup.
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
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
