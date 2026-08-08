// Package db handles PostgreSQL connection and schema migrations.
package db

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Executor is the part of *pgxpool.Pool the domain stores use. They take one
// of these instead of a pool so that a store can be built on an OPEN
// TRANSACTION as easily as on the pool — pgx.Tx implements exactly the same
// four methods — and several stores can then share one transaction and commit
// or roll back together.
//
// That is what `babki seed` needs and nothing else does yet: it writes a space,
// two users, six accounts, their balances, a catalogue of instruments, a
// journal of operations and a demo broker connection, and a failure anywhere
// after the first of those used to leave an instance that could never be seeded
// again (the users exist, so setup is no longer "needed", so the command
// refuses). Everything in production still passes the pool, so this changes no
// behaviour anywhere else.
//
// Begin is part of it because stores already open transactions of their own.
// On a pool that is a transaction; on a pgx.Tx it is a savepoint — which is the
// behaviour wanted here, since an inner failure must not take the whole seed
// down without the outer rollback being what does it.
type Executor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// registerCodecs wires pgx type codecs shared by every connection in the
// pool — currently the shopspring/decimal <-> NUMERIC codec, so
// *decimal.Decimal fields scan directly from and encode directly to
// PostgreSQL NUMERIC columns without manual pgtype.Numeric conversion.
func registerCodecs(_ context.Context, conn *pgx.Conn) error {
	pgxdecimal.Register(conn.TypeMap())
	return nil
}

// PoolConfig parses url into a pgxpool.Config with the shared codecs wired
// via AfterConnect. Exported so test helpers (internal/platform/testdb) can
// build pools that behave identically to production ones.
func PoolConfig(url string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.AfterConnect = registerCodecs
	return cfg, nil
}

// Connect creates a pool and verifies the connection with a ping.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := PoolConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
