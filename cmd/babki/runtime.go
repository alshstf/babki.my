package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/config"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/logging"
	"babki.my/babki/internal/platform/secretbox"
)

// rt — shared runtime for all roles: config, logger, database.
type rt struct {
	cfg  *config.Config
	log  *slog.Logger
	pool *pgxpool.Pool
	// box encrypts and decrypts secrets at rest (the broker token), built
	// once from the key requireEncryptionKey validated. nil for migrate, seed
	// and version.
	box *secretbox.Box
}

// setup loads config, connects to the database and optionally migrates.
// requireEncryptionKey is true for roles that decrypt tokens (all, api, worker)
// and false for migrate, seed and version, which must run before the key exists.
func setup(ctx context.Context, migrate, requireEncryptionKey bool) (*rt, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	// The default logger, for code that has no logger passed in
	// (family.WriteError, marketdata.Converter.fetchRates), so their lines keep
	// the configured level and format.
	slog.SetDefault(log)
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("BABKI_DATABASE_URL is required")
	}
	// Checked before connecting, so a missing key fails on that alone.
	// secretbox.ParseKey's error already names the variable and how to make a
	// value.
	box, err := buildBox(cfg, requireEncryptionKey)
	if err != nil {
		return nil, err
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
	return &rt{cfg: cfg, log: log, pool: pool, box: box}, nil
}

// buildBox parses BABKI_ENCRYPTION_KEY and builds the Box the process uses, so
// the validated key is the used key. nil, nil when the role does not require
// it.
func buildBox(cfg *config.Config, requireEncryptionKey bool) (*secretbox.Box, error) {
	if !requireEncryptionKey {
		return nil, nil
	}
	key, err := secretbox.ParseKey(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	box, err := secretbox.New(key)
	if err != nil || cfg.EncryptionKeyPrevious == "" {
		return box, err
	}
	previous, err := secretbox.ParseKey(cfg.EncryptionKeyPrevious)
	if err != nil {
		return nil, fmt.Errorf("BABKI_ENCRYPTION_KEY_PREVIOUS: %w", err)
	}
	return box.WithPrevious(previous)
}

func (r *rt) close() { r.pool.Close() }
