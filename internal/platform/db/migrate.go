package db

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLockKey is the advisory lock Migrate holds while it runs. Any constant
// will do; this one spells "babkimig" in ASCII.
const migrateLockKey int64 = 0x6261626b696d6967

// Migrate applies the goose migrations and River's schema migrations. It is
// idempotent, and an advisory lock keeps two processes (say api and worker
// starting together) from migrating at once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	lock, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire a connection for the lock: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migrate: take the lock: %w", err)
	}
	// Unlock explicitly, even after cancellation: the connection returns to the
	// pool and would carry the session lock with it.
	defer func() {
		_, _ = lock.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}
