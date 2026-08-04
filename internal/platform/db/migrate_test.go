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

// inventedQuoteDateMigration is the version that drops the quotes whose date
// was the day they were fetched rather than the session they belong to.
const inventedQuoteDateMigration = 13

// TestMigrate_QuotesWithAnInventedDateAreDropped is #92. Until #96 the moex
// provider stamped a quote with the caller's clock while the price it carried
// was the previous session's, so the stored date named a day the price does not
// belong to. Most such rows heal by themselves — the next refresh overwrites
// them — but an instrument that STOPS being quoted is never written again, so
// its row would name the wrong day for as long as the database exists. The
// ordinary way into that shape is a bond redeemed off TQOB or TQCB, or a share
// delisted from TQBR — not the owner's frozen FinEx and SPB paper, which this
// provider has never priced at all (see the migration's own comment).
//
// The migration deletes every quote the moex provider wrote, and this pins both
// halves of that: the provider's rows go, and rows from any other source stay.
// The second half is what keeps the demo stand and any hand-recorded price
// alive — a provider can refetch what it owns, and nothing can refetch what it
// does not.
func TestMigrate_QuotesWithAnInventedDateAreDropped(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, inventedQuoteDateMigration-1)
	var instrumentID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (type, name, ticker, currency)
		 VALUES ('share', 'ФинЭкс', 'FXUS', 'RUB') RETURNING id`).Scan(&instrumentID); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	// One row per source, all for the one instrument, each on its own day —
	// (instrument_id, on_date) is the primary key, so one instrument cannot hold
	// two sources on one date. Two moex rows, because "delete the provider's
	// quotes" must not be satisfied by deleting the newest one and leaving the
	// history behind it.
	staged := []struct{ on, source string }{
		{"2026-07-20", "moex"},
		{"2026-07-17", "moex"},
		{"2026-07-16", "seed"},
		{"2026-07-15", "manual"},
	}
	for _, s := range staged {
		if _, err := pool.Exec(ctx,
			`INSERT INTO quotes (instrument_id, on_date, price, currency, source)
			 VALUES ($1, $2, 305.50, 'RUB', $3)`, instrumentID, s.on, s.source); err != nil {
			t.Fatalf("insert a %s quote on %s: %v", s.source, s.on, err)
		}
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var left []string
	rows, err := pool.Query(ctx, `SELECT source || ' ' || on_date::text FROM quotes ORDER BY source, on_date`)
	if err != nil {
		t.Fatalf("read the quotes back: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan a quote: %v", err)
		}
		left = append(left, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the quotes back: %v", err)
	}
	want := []string{"manual 2026-07-15", "seed 2026-07-16"}
	if strings.Join(left, ", ") != strings.Join(want, ", ") {
		t.Errorf("quotes left = [%s], want [%s]\nevery moex row must go (its date may name a day its price does not belong to, and nothing in the row says which), and no other row may — the provider refetches what it owns and nothing refetches the rest",
			strings.Join(left, ", "), strings.Join(want, ", "))
	}

	// The instrument itself is not collateral. Deleting a price must not delete
	// the paper it was a price of.
	var instruments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&instruments); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if instruments != 1 {
		t.Errorf("instruments left = %d, want 1", instruments)
	}

	// And the table goes on accepting what the provider will write back on its
	// next refresh: the migration is a one-off cleanup, not a rule.
	if _, err := pool.Exec(ctx,
		`INSERT INTO quotes (instrument_id, on_date, price, currency, source)
		 VALUES ($1, '2026-07-21', 306.00, 'RUB', 'moex')`, instrumentID); err != nil {
		t.Errorf("the database refused a fresh moex quote after the migration: %v", err)
	}
}

// TestMigrate_AnEmptyQuotesTableSurvivesTheCleanup is the self-hoster who has
// never run the quotes job — a fresh install, or one tracking nothing the moex
// provider covers. A DELETE that matches nothing must be an ordinary no-op, not
// a stumble on the way up. TestMigrate covers the same claim from the other end
// (the whole chain over a database with no tables at all); this one puts real
// rows in the table and leaves every one of them there.
func TestMigrate_AnEmptyQuotesTableSurvivesTheCleanup(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, inventedQuoteDateMigration-1)
	var instrumentID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (type, name, ticker, currency)
		 VALUES ('share', 'Сбербанк', 'SBER', 'RUB') RETURNING id`).Scan(&instrumentID); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO quotes (instrument_id, on_date, price, currency, source)
		 VALUES ($1, '2026-07-20', 305.50, 'RUB', 'seed')`, instrumentID); err != nil {
		t.Fatalf("insert a seed quote: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v\nnothing here was written by a provider, so nothing here may be touched", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotes`).Scan(&left); err != nil {
		t.Fatalf("count quotes: %v", err)
	}
	if left != 1 {
		t.Errorf("quotes left = %d, want 1", left)
	}
}

// tinvestImportMigration is the version that creates the five tables behind
// the T-Invest mirror-and-projection import: a connection (one read-only
// token per space), the accounts it feeds, the raw operation mirror, the
// instrument id map, and the sync run log.
const tinvestImportMigration = 14

// TestMigrate_TinvestConnectionDeleteCascadesEverythingButTheAccount pins the
// first half of the deletion story the design settled on: a connection owns
// its links, its mirror rows, its instrument map and its sync runs, and
// removing it must take all four with it — that is what "the link breaks" is
// supposed to mean when the owner disconnects. It must NOT take the babki
// account or the instrument catalog entry, because neither is owned by the
// connection: an account is a first-class thing the owner created (or the
// import created on their behalf) and can go on holding manually-entered
// operations after the connection is gone, and the instrument catalog is
// shared across the whole instance.
func TestMigrate_TinvestConnectionDeleteCascadesEverythingButTheAccount(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")
	var instrumentID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (type, name, ticker, currency) VALUES ('share', 'Сбербанк', 'SBER', 'RUB') RETURNING id`).
		Scan(&instrumentID); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_operations_mirror
		    (connection_id, link_id, broker_operation_id, op_type, state, occurred_at, currency, payment, raw, content_key, last_confirmed_at)
		 VALUES ($1, $2, 'op-1', 'buy', 'executed', now(), 'RUB', 100.5, '{}'::jsonb, 'key-1', now())`,
		connectionID, linkID); err != nil {
		t.Fatalf("insert mirror row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_instrument_map (connection_id, instrument_id, instrument_uid) VALUES ($1, $2, 'uid-1')`,
		connectionID, instrumentID); err != nil {
		t.Fatalf("insert instrument map row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status) VALUES ($1, $2, 'initial', 'ok')`,
		connectionID, linkID); err != nil {
		t.Fatalf("insert sync run: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM tinvest_connections WHERE id = $1`, connectionID); err != nil {
		t.Fatalf("delete connection: %v", err)
	}

	for _, tbl := range []string{
		"tinvest_account_links", "tinvest_operations_mirror",
		"tinvest_instrument_map", "tinvest_sync_runs",
	} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE connection_id = $1`, connectionID).
			Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if count != 0 {
			t.Errorf("%s left = %d after the connection was deleted, want 0: deleting a connection must cascade to everything it owns", tbl, count)
		}
	}

	var accounts, instruments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id = $1`, accountID).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 1 {
		t.Errorf("accounts left = %d, want 1: deleting a tinvest connection must not touch the account it fed", accounts)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments WHERE id = $1`, instrumentID).Scan(&instruments); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if instruments != 1 {
		t.Errorf("instruments left = %d, want 1: deleting a tinvest connection must not touch the shared catalog", instruments)
	}
}

// TestMigrate_TinvestAccountDeleteCascadesTheLinkButLeavesTheConnection is the
// other direction: the owner can delete an account (a first-class thing of
// its own) without that reaching back into the connection or any of its other
// links. Only the one link that named this account goes.
func TestMigrate_TinvestAccountDeleteCascadesTheLinkButLeavesTheConnection(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	if _, err := pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	var links int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tinvest_account_links WHERE connection_id = $1`, connectionID).
		Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Errorf("links left = %d after the account was deleted, want 0", links)
	}

	var connections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tinvest_connections WHERE id = $1`, connectionID).
		Scan(&connections); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	if connections != 1 {
		t.Errorf("connections left = %d, want 1: deleting an account must not touch the connection it was linked to", connections)
	}
}

// TestMigrate_TinvestConnectionStatusCheckRejectsUnknownValues pins the CHECK
// on tinvest_connections.status: only the three states the sync worker knows
// how to act on ('active', 'token_revoked', 'disabled') are storable.
func TestMigrate_TinvestConnectionStatusCheckRejectsUnknownValues(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_connections (space_id, status, token_ciphertext, token_last4) VALUES ($1, 'bogus', $2, '1234')`,
		spaceID, []byte{0x01}); err == nil {
		t.Error("the database accepted a connection status of 'bogus'")
	}

	for _, status := range []string{"active", "token_revoked", "disabled"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_connections (space_id, status, token_ciphertext, token_last4) VALUES ($1, $2, $3, '1234')`,
			spaceID, status, []byte{0x01}); err != nil {
			t.Errorf("the database refused a connection status of %q, which is a valid one: %v", status, err)
		}
	}
}

// TestMigrate_TinvestSyncRunCheckedColumnsRejectUnknownValues covers all three
// CHECK constraints on tinvest_sync_runs at once: trigger, status and
// reconcile_status are each a closed set the sync worker and the reconciler
// hand-code against, and a stray value in any one of them is exactly as
// dangerous as in the others.
func TestMigrate_TinvestSyncRunCheckedColumnsRejectUnknownValues(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	for _, c := range []struct{ trigger, status, reconcile string }{
		{"bogus", "ok", "not_checked"},
		{"schedule", "bogus", "not_checked"},
		{"schedule", "ok", "bogus"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status, reconcile_status)
			 VALUES ($1, $2, $3, $4, $5)`,
			connectionID, linkID, c.trigger, c.status, c.reconcile); err == nil {
			t.Errorf("the database accepted a sync run with trigger=%q status=%q reconcile_status=%q", c.trigger, c.status, c.reconcile)
		}
	}

	for _, c := range []struct{ trigger, status, reconcile string }{
		{"schedule", "running", "not_checked"},
		{"manual", "ok", "matched"},
		{"initial", "failed", "mismatched"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_sync_runs (connection_id, link_id, trigger, status, reconcile_status)
			 VALUES ($1, $2, $3, $4, $5)`,
			connectionID, linkID, c.trigger, c.status, c.reconcile); err != nil {
			t.Errorf("the database refused a sync run with trigger=%q status=%q reconcile_status=%q, which is valid: %v", c.trigger, c.status, c.reconcile, err)
		}
	}
}

// TestMigrate_TinvestMirrorContentKeyIsNotUnique pins the one property the
// mirror design most depends on getting right: content_key identifies what an
// operation SAYS, not which operation it is, and two broker operations can
// legitimately say the same thing (e.g. two identical top-ups in one minute).
// A unique index here would silently drop the second one — exactly the kind
// of silent loss "honesty over silence" rules out. The mirror is a multiset.
func TestMigrate_TinvestMirrorContentKeyIsNotUnique(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	for _, opID := range []string{"op-1", "op-2"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_operations_mirror
			    (connection_id, link_id, broker_operation_id, op_type, state, occurred_at, currency, payment, raw, content_key, last_confirmed_at)
			 VALUES ($1, $2, $3, 'buy', 'executed', now(), 'RUB', 100.5, '{}'::jsonb, 'same-content-key', now())`,
			connectionID, linkID, opID); err != nil {
			t.Fatalf("insert mirror row %s: %v", opID, err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM tinvest_operations_mirror WHERE content_key = 'same-content-key'`).Scan(&count); err != nil {
		t.Fatalf("count mirror rows: %v", err)
	}
	if count != 2 {
		t.Errorf("mirror rows sharing the content key = %d, want 2: content_key must not be unique", count)
	}
}

// TestMigrate_TinvestAccountLinkUniqueConstraints pins both uniqueness rules
// on tinvest_account_links at once, because they guard two different
// failures: UNIQUE(account_id) keeps one babki account from being fed by two
// links (which would make "the account this operation belongs to" ambiguous),
// and UNIQUE(connection_id, broker_account_id) keeps one broker account from
// being imported twice under the same connection (which would double every
// operation in it).
func TestMigrate_TinvestAccountLinkUniqueConstraints(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	account1 := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции 1")
	account2 := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции 2")

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	insertTinvestLink(t, ctx, pool, connectionID, spaceID, account1, "2000000001")

	// account_id UNIQUE: the same babki account cannot be fed by a second link,
	// even under a different broker account number.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000002', 'Другой счёт', 'brokerage')`,
		connectionID, spaceID, account1); err == nil {
		t.Error("the database accepted a second link feeding the same account")
	}

	// UNIQUE(connection_id, broker_account_id): the same broker account cannot
	// be linked twice under one connection, even to a different babki account.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000001', 'Дубль по номеру брокера', 'brokerage')`,
		connectionID, spaceID, account2); err == nil {
		t.Error("the database accepted a second link with the same broker account id under one connection")
	}

	// A genuinely different account under a genuinely different broker account
	// id must go through — the two checks above must not overlap into refusing
	// this.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000002', 'Второй счёт', 'brokerage')`,
		connectionID, spaceID, account2); err != nil {
		t.Errorf("the database refused a genuinely new link: %v", err)
	}
}

// insertTinvestSpace inserts a bare space for the tinvest migration tests to
// hang connections and accounts off of.
func insertTinvestSpace(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ('T-Invest test space') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("insert space: %v", err)
	}
	return id
}

// insertTinvestAccount inserts a brokerage account under spaceID, the kind an
// account link points at.
func insertTinvestAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, spaceID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (space_id, name, type, currency) VALUES ($1, $2, 'brokerage', 'RUB') RETURNING id`,
		spaceID, name).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", name, err)
	}
	return id
}

// insertTinvestConnection inserts a connection with a throwaway ciphertext —
// its content is never asserted on in these migration tests, only its
// presence and its NOT NULL shape.
func insertTinvestConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, spaceID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tinvest_connections (space_id, token_ciphertext, token_last4) VALUES ($1, $2, '1234') RETURNING id`,
		spaceID, []byte{0x01, 0x02, 0x03}).Scan(&id); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	return id
}

// insertTinvestLink inserts an account link under connectionID pointing at
// accountID, with brokerAccountID as the broker's own account number.
func insertTinvestLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectionID, spaceID, accountID, brokerAccountID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, $4, 'Брокерский счёт', 'brokerage') RETURNING id`,
		connectionID, spaceID, accountID, brokerAccountID).Scan(&id); err != nil {
		t.Fatalf("insert account link %q: %v", brokerAccountID, err)
	}
	return id
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
