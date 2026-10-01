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

// Migrate applies all goose migrations and River's own schema migrations.
// Idempotent.
//
// ONE MIGRATION RUN AT A TIME, across processes. Every long-running role
// migrates at start-up, so an instance split into `api` and `worker` starts two
// runs against one database after an upgrade, and two runs applying the same
// file collide half-way through it. The lock is taken on a connection of its
// own and held until the run ends; the second process waits, then finds nothing
// left to apply.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	lock, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire a connection for the lock: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migrate: take the lock: %w", err)
	}
	// Released explicitly, and on a context that outlives a cancelled run: the
	// connection goes back to the pool, and a session lock would go with it.
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
