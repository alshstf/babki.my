package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

func TestMigrate(t *testing.T) {
	// NewEmpty, not New: this is the one test that has to watch the migrations
	// build a schema from nothing, so it cannot be handed the pre-migrated
	// database every other test gets.
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var instanceID string
	err := pool.QueryRow(ctx,
		`SELECT value FROM meta WHERE key = 'instance_id'`).Scan(&instanceID)
	if err != nil {
		t.Fatalf("meta.instance_id: %v", err)
	}
	if instanceID == "" {
		t.Error("instance_id is empty")
	}

	// Idempotency: running again does not fail.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (second run): %v", err)
	}
}

// tickerUniqueMigration is the version that makes instruments.ticker unique.
// The test below has to stand a database at the version just below it.
const tickerUniqueMigration = 11

// TestMigrate_DuplicateTickersStopTheUpgradeAndSayWhatToDo covers the one
// thing this repository's owner cannot have in his own database but a
// self-hoster upgrading might: two instruments already carrying the same
// ticker. A bare CREATE UNIQUE INDEX would fail there with "could not create
// unique index instruments_ticker_uniq" — a sentence that names a Postgres
// object, not the problem, and leaves the person running the upgrade with
// nowhere to start. Merging the rows instead is not on the table: deciding
// which of two catalog entries is the real one rewrites which instrument the
// journal's operations point at, and the journal is the source of truth here —
// no migration gets to edit it unattended.
//
// So the upgrade stops, names the tickers, and says what to do. This test
// pins all three: that it stops, that the message is actionable, and that
// resolving the duplicate lets the very same upgrade through — a wall with no
// door would be no better than a crash.
func TestMigrate_DuplicateTickersStopTheUpgradeAndSayWhatToDo(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tickerUniqueMigration-1)
	for _, name := range []string{"Сбербанк", "Сбербанк, второй раз"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency) VALUES ('share', $1, 'SBER', 'RUB')`,
			name); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	err := db.Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate succeeded on a catalog holding two instruments under one ticker; want it to stop")
	}
	// The whole diagnosis has to be in the error MESSAGE. Postgres carries
	// DETAIL and HINT as separate fields and pgconn.PgError.Error() prints
	// neither, so anything put there would never reach the operator's console.
	msg := err.Error()
	for _, want := range []string{"SBER", "same ticker", "start the application again"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the migration failure does not mention %q, so it does not say what to fix:\n%s", want, msg)
		}
	}

	// Refused, not half-applied: both rows are untouched and the index is absent.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&rows); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if rows != 2 {
		t.Errorf("instruments left = %d, want 2: a migration that refuses must change nothing", rows)
	}
	if indexExists(t, ctx, pool, "instruments_ticker_uniq") {
		t.Error("the unique index exists although the migration refused to run")
	}

	// And it is not a dead end.
	if _, err := pool.Exec(ctx,
		`DELETE FROM instruments WHERE name = 'Сбербанк, второй раз'`); err != nil {
		t.Fatalf("remove the duplicate: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after the duplicate was resolved: %v", err)
	}
	if !indexExists(t, ctx, pool, "instruments_ticker_uniq") {
		t.Error("the unique index is missing after a successful migration")
	}
}

// upTo brings pool's schema to exactly the given migration version, so a test
// can set up the state a later migration has to deal with.
func upTo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int64) {
	t.Helper()
	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	if err := goose.UpToContext(ctx, sqlDB, "migrations", version); err != nil {
		t.Fatalf("goose up to %d: %v", version, err)
	}
}

func indexExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	return exists
}
