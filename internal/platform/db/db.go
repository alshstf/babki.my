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

// Executor is what domain stores need from a database: satisfied by both
// *pgxpool.Pool and pgx.Tx, so several stores can share one transaction (as
// `babki seed` does). Begin on a pgx.Tx opens a savepoint.
type Executor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// registerCodecs maps shopspring/decimal to NUMERIC on every connection.
func registerCodecs(_ context.Context, conn *pgx.Conn) error {
	pgxdecimal.Register(conn.TypeMap())
	return nil
}

// PoolConfig parses url into a pool configuration with the shared codecs.
// Test pools use it too, so they behave like production ones.
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
