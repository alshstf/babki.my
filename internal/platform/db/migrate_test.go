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
// So the upgrade stops, names the rows, and says what to do. This test pins
// all three: that it stops, that the message is actionable, and that resolving
// the duplicate lets the very same upgrade through — a wall with no door would
// be no better than a crash.
//
// "Names the rows", not just the ticker: the operator has to look at the two
// entries to decide which one to keep, and a message saying only "SBER
// (2 instruments)" starts that with a hunt for them.
func TestMigrate_DuplicateTickersStopTheUpgradeAndSayWhatToDo(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tickerUniqueMigration-1)
	ids := make(map[string]string, 2)
	for _, name := range []string{"Сбербанк", "Сбербанк, второй раз"} {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO instruments (type, name, ticker, currency) VALUES ('share', $1, 'SBER', 'RUB')
			 RETURNING id`,
			name).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
		ids[name] = id
	}

	err := db.Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate succeeded on a catalog holding two instruments under one ticker; want it to stop")
	}
	// The whole diagnosis has to be in the error MESSAGE. Postgres carries
	// DETAIL and HINT as separate fields and pgconn.PgError.Error() prints
	// neither, so anything put there would never reach the operator's console.
	msg := err.Error()
	want := []string{"SBER", "same ticker", "start the application again"}
	// Both rows, by id and by name — either one alone would leave the operator
	// looking for which two entries are meant.
	for name, id := range ids {
		want = append(want, name, id)
	}
	for _, want := range want {
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

// TestMigrate_DuplicatesOutsideTheTradableSetDoNotStopTheUpgrade is the other
// self-hoster, the one the refusal must not touch. Prices are fetched for
// shares, bonds and funds alone (instrument.ListTradable), so those are the
// only rows uniqueness is about — and the only rows migration 0011 refuses. A
// user tracking one coin on two venues has two crypto rows both reading BTC;
// gold vaulted at two brokers is two metal rows both reading XAU; cash and
// hand-made holdings carry no ticker at all, any number of them. None of that
// was ever priced, none of it was ever a bug, and an upgrade that stopped to
// lecture about the quotes job would be refusing over pricing that never
// applied to them.
//
// It is also the half of migration 0011 that its refusal query and its index
// predicate could drift on: they are written out separately, and this test goes
// red if the query alone widens.
func TestMigrate_DuplicatesOutsideTheTradableSetDoNotStopTheUpgrade(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tickerUniqueMigration-1)
	rows := []struct{ kind, name, ticker string }{
		{"crypto", "Биткойн на бирже A", "BTC"},
		{"crypto", "Биткойн на бирже B", "BTC"},
		{"metal", "Золото у брокера A", "XAU"},
		{"metal", "Золото у брокера B", "XAU"},
		{"custom", "Наличные", ""},
		{"custom", "Золотой слиток", ""},
		// A share with a ticker of its own: the upgrade has to go through with
		// tradable rows present, not only on a catalog it has nothing to check.
		{"share", "Сбербанк", "SBER"},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency) VALUES ($1, $2, $3, 'RUB')`,
			r.kind, r.name, r.ticker); err != nil {
			t.Fatalf("insert %q: %v", r.name, err)
		}
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v\nnothing here is ever priced by ticker, so nothing here may stop the upgrade", err)
	}
	if !indexExists(t, ctx, pool, "instruments_ticker_uniq") {
		t.Error("the unique index is missing after a successful migration")
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&left); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if left != len(rows) {
		t.Errorf("instruments left = %d, want %d: the migration must not remove or merge anything", left, len(rows))
	}

	// And the index goes on tolerating them afterwards, which is what the
	// running application will be writing against.
	for _, r := range []struct{ kind, name, ticker string }{
		{"crypto", "Биткойн на бирже C", "BTC"},
		{"metal", "Золото у брокера C", "XAU"},
		{"custom", "Ещё наличные", ""},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency) VALUES ($1, $2, $3, 'RUB')`,
			r.kind, r.name, r.ticker); err != nil {
			t.Errorf("insert %q after the migration: %v — no price is fetched for it, so the ticker is free", r.name, err)
		}
	}
}

// faceValueMigration is the version that makes an instrument's face value and
// its currency an all-or-nothing, positive pair.
const faceValueMigration = 12

// TestMigrate_UnsoundFaceValuesStopTheUpgradeAndSayWhatToDo is #93's answer to
// the rows that are already there. A face value of zero, or one without the
// currency it is denominated in, was writable until the handler learned to
// refuse it — and a write-side rule that leaves such rows behind is not an
// invariant, it is an intention: every reader would still have to derive the
// check, which is exactly what moving it to the write was for.
//
// So the upgrade stops, names the instruments, and says what to do. Repairing
// them is not on the table for the reason migration 0011 refuses to merge
// duplicate tickers: a face value decides what every position in that bond is
// worth, and clearing the pair discards a number somebody entered.
//
// This test pins all three: that it stops, that the message is actionable, and
// that fixing the rows lets the very same upgrade through — a wall with no door
// would be no better than a crash.
func TestMigrate_UnsoundFaceValuesStopTheUpgradeAndSayWhatToDo(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, faceValueMigration-1)
	// Every shape, because they are separate rules and a migration that caught
	// only some would leave the rest in the database. The zero is what creation
	// let through by never checking the value; the empty currency is what it let
	// through by checking the currency's presence and never its shape — and it is
	// the one an IS NULL test cannot see, since '' IS NULL is false; the half pair
	// is what the update let through by checking nothing at all.
	staged := []struct{ name, face string }{
		{"ОФЗ с нулевым номиналом", "0, 'RUB'"},
		{"ОФЗ с пустой валютой номинала", "100000, ''"},
		{"ОФЗ без валюты номинала", "100000, NULL"},
	}
	ids := make(map[string]string, len(staged))
	for _, s := range staged {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO instruments (type, name, ticker, currency, face_value_minor, face_currency)
			 VALUES ('bond', $1, '', 'RUB', `+s.face+`) RETURNING id`, s.name).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", s.name, err)
		}
		ids[s.name] = id
	}

	err := db.Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate succeeded on a catalog holding a face value that is not one; want it to stop")
	}
	// The whole diagnosis has to be in the error MESSAGE: pgconn.PgError.Error()
	// prints neither DETAIL nor HINT.
	msg := err.Error()
	want := []string{"face value", "start the application again"}
	// Every row, by id and by name — either alone leaves the operator hunting
	// for which entries are meant.
	for name, id := range ids {
		want = append(want, name, id)
	}
	for _, want := range want {
		if !strings.Contains(msg, want) {
			t.Errorf("the migration failure does not mention %q, so it does not say what to fix:\n%s", want, msg)
		}
	}

	// Refused, not half-applied: both rows are untouched and the constraint is
	// absent.
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM instruments
		  WHERE face_value_minor = 0 OR face_currency IS NULL OR face_currency = ''`).Scan(&rows); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if rows != len(staged) {
		t.Errorf("unsound instruments left = %d, want %d: a migration that refuses must change nothing", rows, len(staged))
	}
	if constraintExists(t, ctx, pool, "instruments_face_value_sound") {
		t.Error("the constraint exists although the migration refused to run")
	}

	// And it is not a dead end: both repairs the message offers work.
	if _, err := pool.Exec(ctx,
		`UPDATE instruments SET face_value_minor = 100000 WHERE face_value_minor = 0`); err != nil {
		t.Fatalf("record a real face value: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE instruments SET face_currency = 'RUB' WHERE face_currency = ''`); err != nil {
		t.Fatalf("name the currency the face value is in: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE instruments SET face_value_minor = NULL WHERE face_currency IS NULL`); err != nil {
		t.Fatalf("clear the half pair: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after the catalog was fixed: %v", err)
	}
	if !constraintExists(t, ctx, pool, "instruments_face_value_sound") {
		t.Error("the constraint is missing after a successful migration")
	}
}

// TestMigrate_SoundFaceValuesDoNotStopTheUpgrade is the other self-hoster, the
// one the refusal must not touch: an instrument with no face value at all is the
// ordinary case (every share, every fund, and a bond nobody has recorded one
// for), and it must go through untouched.
//
// The second half is what the constraint is FOR. A rule that only guarded the
// upgrade would leave the running application free to write the same state back
// the moment a handler forgot to check.
func TestMigrate_SoundFaceValuesDoNotStopTheUpgrade(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, faceValueMigration-1)
	for _, r := range []struct{ kind, name, face string }{
		{"bond", "ОФЗ 26238", "100000, 'RUB'"},
		{"bond", "Облигация без номинала", "NULL, NULL"},
		{"bond", "Номинал в один минорный юнит", "1, 'USD'"},
		{"share", "Сбербанк", "NULL, NULL"},
		{"custom", "Наличные", "NULL, NULL"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency, face_value_minor, face_currency)
			 VALUES ($1, $2, '', 'RUB', `+r.face+`)`, r.kind, r.name); err != nil {
			t.Fatalf("insert %q: %v", r.name, err)
		}
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v\nnothing here is a broken pair, so nothing here may stop the upgrade", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&left); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if left != 5 {
		t.Errorf("instruments left = %d, want 5: the migration must not remove or change anything", left)
	}

	// And the constraint goes on refusing what the handlers refuse, which is
	// what the running application writes against.
	// "100000, ''" is the one no IS NULL test catches: an empty string is not
	// null, so the pairing equality alone reads it as a whole pair while it
	// denominates the face value in nothing.
	for _, face := range []string{"0, 'RUB'", "-1, 'RUB'", "100000, NULL", "NULL, 'RUB'", "100000, ''"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency, face_value_minor, face_currency)
			 VALUES ('bond', 'Сломанная', '', 'RUB', `+face+`)`); err == nil {
			t.Errorf("the database accepted a face value of (%s) after the migration", face)
		}
	}
	// While the sound shapes stay writable.
	for _, face := range []string{"1, 'RUB'", "NULL, NULL"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (type, name, ticker, currency, face_value_minor, face_currency)
			 VALUES ('bond', 'Целая', '', 'RUB', `+face+`)`); err != nil {
			t.Errorf("the database refused a face value of (%s), which is a sound one: %v", face, err)
		}
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

func constraintExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("look up constraint %s: %v", name, err)
	}
	return exists
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
