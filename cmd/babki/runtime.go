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
	// box decrypts and encrypts secrets at rest (today, the T-Invest broker
	// token). setup builds it at most once, from the same parsed key that
	// requireEncryptionKey validates — so the key that was checked and the
	// key that gets used are one value, not two independent parses of
	// BABKI_ENCRYPTION_KEY that could silently drift apart. nil for the
	// roles that don't require the key (migrate, seed, version): they must
	// never reach a consumer that dereferences this field.
	box *secretbox.Box
}

// setup loads config, connects to database, and optionally runs migrations.
//
// requireEncryptionKey gates validation of BABKI_ENCRYPTION_KEY: true for the
// roles that decrypt a broker token at runtime (all, api, worker), false for
// migrate, seed and version, which have to keep working on a machine where
// the key has not been provisioned yet — migrate above all, since it is the
// role that exists to run before secrets are.
func setup(ctx context.Context, migrate, requireEncryptionKey bool) (*rt, error) {
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
	// Checked before db.Connect on purpose: a role that needs the key but
	// doesn't have one should fail on that fact alone, without also needing a
	// reachable database to reach the check. secretbox.ParseKey's own error
	// already names BABKI_ENCRYPTION_KEY and the exact command to generate a
	// value it accepts, so it is returned as-is rather than re-wrapped.
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

// buildBox parses BABKI_ENCRYPTION_KEY and builds the *secretbox.Box the
// running process will actually use, in one step: the value that gets
// validated is the value that gets used, so no later code has to parse the
// key a second time to build its own Box (see rt.box's doc for why that
// matters). Returns nil, nil — no key parsed, no error — when the role does
// not require the key, which is what leaves rt.box nil for migrate, seed and
// version.
func buildBox(cfg *config.Config, requireEncryptionKey bool) (*secretbox.Box, error) {
	if !requireEncryptionKey {
		return nil, nil
	}
	key, err := secretbox.ParseKey(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return secretbox.New(key)
}

func (r *rt) close() { r.pool.Close() }
