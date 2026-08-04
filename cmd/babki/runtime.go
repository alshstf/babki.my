package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/config"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/logging"
)

// rt — shared runtime for all roles: config, logger, database.
type rt struct {
	cfg  *config.Config
	log  *slog.Logger
	pool *pgxpool.Pool
}

// setup loads config, connects to database, and optionally runs migrations.
func setup(ctx context.Context, migrate bool) (*rt, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	// Everything that has a logger handed to it keeps using this same value; the
	// default is installed for the code that has none and cannot be given one
	// without threading a logger through forty-odd call sites — today that is
	// family.WriteError, which logs the error behind every 500 so a failure is
	// diagnosable at all (see its doc), and marketdata.Converter.fetchRates,
	// which reports a batched rate lookup that died while its per-pair fallback
	// carried on answering correctly. Installing it here rather than reaching
	// for a package-level global is what keeps that line inside the configured
	// level and format instead of a second, differently shaped stream.
	slog.SetDefault(log)
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("BABKI_DATABASE_URL is required")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if migrate && cfg.AutoMigrate {
		log.Info("running migrations")
		if err := db.Migrate(ctx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &rt{cfg: cfg, log: log, pool: pool}, nil
}

func (r *rt) close() { r.pool.Close() }
