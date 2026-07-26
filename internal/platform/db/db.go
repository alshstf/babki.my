// Package db handles PostgreSQL connection and schema migrations.
package db

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
