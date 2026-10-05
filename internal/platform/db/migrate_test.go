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
	// NewEmpty: this test builds the schema from nothing.
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

// tickerUniqueMigration makes instruments.ticker unique.
const tickerUniqueMigration = 11

// An upgrade over duplicate tickers stops with a message naming both rows and
// what to do, and goes through once the duplicate is resolved. Merging rows is
// not an option: it would repoint journal operations.
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
	// Postgres DETAIL and HINT are not in PgError.Error(), so the message must carry everything.
	msg := err.Error()
	want := []string{"SBER", "same ticker", "start the application again"}
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

// Duplicate tickers outside the tradable set (crypto, metals, untickered rows)
// do not stop the upgrade: only shares, bonds and funds are priced by ticker.
// Guards the refusal query and the index predicate against drifting apart.
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
		// A tradable row of its own, so the check runs over something.
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

	// The index keeps tolerating them afterwards.
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

// faceValueMigration makes face value and its currency a positive all-or-nothing pair.
const faceValueMigration = 12

// Unsound face values (zero, no currency, half a pair) stop the upgrade with a
// message naming the rows, and the upgrade goes through once they are fixed.
// Clearing them would discard numbers somebody entered (#93).
func TestMigrate_UnsoundFaceValuesStopTheUpgradeAndSayWhatToDo(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, faceValueMigration-1)
	// Every shape: '' IS NULL is false, so the empty currency needs its own case.
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
	msg := err.Error()
	want := []string{"face value", "start the application again"}
	for name, id := range ids {
		want = append(want, name, id)
	}
	for _, want := range want {
		if !strings.Contains(msg, want) {
			t.Errorf("the migration failure does not mention %q, so it does not say what to fix:\n%s", want, msg)
		}
	}

	// Refused, not half-applied.
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

// Instruments without a face value pass the upgrade, and the constraint then
// refuses unsound writes.
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

	// "100000, ''" is the case an IS NULL test misses.
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

// inventedQuoteDateMigration drops quotes dated by the fetch day, not the session (#92).
const inventedQuoteDateMigration = 13

// The migration deletes the moex provider's quotes, whose dates were the fetch
// day until #96, and leaves other sources' quotes alone: the provider can
// refetch its own, nothing can refetch the rest.
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
	// One row per source on its own day ((instrument_id, on_date) is the key), and
	// two moex rows so deleting only the newest would fail.
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

	// The instrument itself stays.
	var instruments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&instruments); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if instruments != 1 {
		t.Errorf("instruments left = %d, want 1", instruments)
	}

	// The table still accepts the provider's next refresh.
	if _, err := pool.Exec(ctx,
		`INSERT INTO quotes (instrument_id, on_date, price, currency, source)
		 VALUES ($1, '2026-07-21', 306.00, 'RUB', 'moex')`, instrumentID); err != nil {
		t.Errorf("the database refused a fresh moex quote after the migration: %v", err)
	}
}

// The cleanup is a no-op on a quotes table with nothing from moex.
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

// tinvestImportMigration creates the T-Invest import tables.
const tinvestImportMigration = 14

// Deleting a connection removes its links, mirror rows, instrument map and sync
// runs, but not the account or the shared instrument catalog.
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

// Deleting an account removes only its link, not the connection.
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

// content_key is not unique: two broker operations can say the same thing (two
// identical top-ups in one minute), and both must be kept.
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

// One babki account per link, and one link per broker account under a
// connection.
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

	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000002', 'Другой счёт', 'brokerage')`,
		connectionID, spaceID, account1); err == nil {
		t.Error("the database accepted a second link feeding the same account")
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000001', 'Дубль по номеру брокера', 'brokerage')`,
		connectionID, spaceID, account2); err == nil {
		t.Error("the database accepted a second link with the same broker account id under one connection")
	}

	// A different account under a different broker account goes through.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_account_links (connection_id, space_id, account_id, broker_account_id, broker_account_name, broker_account_type)
		 VALUES ($1, $2, $3, '2000000002', 'Второй счёт', 'brokerage')`,
		connectionID, spaceID, account2); err != nil {
		t.Errorf("the database refused a genuinely new link: %v", err)
	}
}

// A broker instrument_uid maps to one catalog entry per connection.
func TestMigrate_TinvestInstrumentMapUniqueConstraint(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestImportMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	var instrument1, instrument2 string
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (type, name, ticker, currency) VALUES ('share', 'Сбербанк', 'SBER', 'RUB') RETURNING id`).
		Scan(&instrument1); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO instruments (type, name, ticker, currency) VALUES ('share', 'Газпром', 'GAZP', 'RUB') RETURNING id`).
		Scan(&instrument2); err != nil {
		t.Fatalf("insert instrument: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_instrument_map (connection_id, instrument_id, instrument_uid) VALUES ($1, $2, 'uid-1')`,
		connectionID, instrument1); err != nil {
		t.Fatalf("insert instrument map row: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_instrument_map (connection_id, instrument_id, instrument_uid) VALUES ($1, $2, 'uid-1')`,
		connectionID, instrument2); err == nil {
		t.Error("the database accepted a second instrument map row with the same (connection_id, instrument_uid)")
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO tinvest_instrument_map (connection_id, instrument_id, instrument_uid) VALUES ($1, $2, 'uid-2')`,
		connectionID, instrument2); err != nil {
		t.Errorf("the database refused a genuinely new instrument map row: %v", err)
	}
}

const tinvestMirrorTickerMigration = 19

// The upgrade recovers tickers from the stored broker payload, including the
// ISIN the broker puts there for an instrument it has forgotten.
func TestMigrate_TinvestTickerIsRecoveredFromTheStoredPayload(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestMirrorTickerMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")
	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	rows := []struct {
		opID string
		raw  string
		want string
	}{
		{"ordinary", `{"ticker": "SBER"}`, "SBER"},
		// The owner's own: a fund the broker has wound up, named by its ISIN.
		{"forgotten-paper", `{"ticker": "RU000A101X68"}`, "RU000A101X68"},
		{"no-such-field", `{"quantity": "1000"}`, ""},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_operations_mirror
			    (connection_id, link_id, broker_operation_id, op_type, state, occurred_at, currency, payment, quantity, raw, content_key, last_confirmed_at)
			 VALUES ($1, $2, $3, 'OPERATION_TYPE_SELL', 'OPERATION_STATE_EXECUTED', now(), 'RUB', 127121, 190, $4::jsonb, $3, now())`,
			connectionID, linkID, r.opID, r.raw); err != nil {
			t.Fatalf("insert mirror row %s: %v", r.opID, err)
		}
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, r := range rows {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT ticker FROM tinvest_operations_mirror WHERE broker_operation_id = $1`, r.opID).
			Scan(&got); err != nil {
			t.Fatalf("read ticker of %s: %v", r.opID, err)
		}
		if got != r.want {
			t.Errorf("%s: ticker = %q, want %q", r.opID, got, r.want)
		}
	}
}

const tinvestQuantityDoneMigration = 16

// The upgrade recovers the executed quantity from the stored payload (#131):
// partly filled, fully filled, and absent.
func TestMigrate_TinvestExecutedQuantityIsRecoveredFromTheStoredPayload(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tinvestQuantityDoneMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")
	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	rows := []struct {
		opID string
		raw  string
		want int64
	}{
		// The owner's own sale of 115 bonds out of an order for 190.
		{"partly-filled", `{"quantity": "190", "quantityRest": "75", "quantityDone": "115"}`, 115},
		{"fully-filled", `{"quantity": "100", "quantityRest": "0", "quantityDone": "100"}`, 100},
		// Absent: zero, which the projection refuses rather than reads as nothing.
		{"no-such-field", `{"quantity": "1000"}`, 0},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_operations_mirror
			    (connection_id, link_id, broker_operation_id, op_type, state, occurred_at, currency, payment, quantity, raw, content_key, last_confirmed_at)
			 VALUES ($1, $2, $3, 'OPERATION_TYPE_SELL', 'OPERATION_STATE_EXECUTED', now(), 'RUB', 127121, 190, $4::jsonb, $3, now())`,
			connectionID, linkID, r.opID, r.raw); err != nil {
			t.Fatalf("insert mirror row %s: %v", r.opID, err)
		}
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, r := range rows {
		var got int64
		if err := pool.QueryRow(ctx,
			`SELECT quantity_done FROM tinvest_operations_mirror WHERE broker_operation_id = $1`, r.opID).
			Scan(&got); err != nil {
			t.Fatalf("read quantity_done of %s: %v", r.opID, err)
		}
		if got != r.want {
			t.Errorf("%s: quantity_done = %d, want %d", r.opID, got, r.want)
		}
	}

	// content_key is left alone: rebuilding it would make every mirror row look
	// gone and re-added.
	var key string
	if err := pool.QueryRow(ctx,
		`SELECT content_key FROM tinvest_operations_mirror WHERE broker_operation_id = 'partly-filled'`).
		Scan(&key); err != nil {
		t.Fatalf("read content_key: %v", err)
	}
	if key != "partly-filled" {
		t.Errorf("content_key = %q, want it untouched by the upgrade", key)
	}
}

func insertTinvestSpace(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO spaces (name) VALUES ('T-Invest test space') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("insert space: %v", err)
	}
	return id
}

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

// insertTinvestConnection inserts a connection with a placeholder ciphertext.
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

// upTo migrates pool to exactly version.
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

// Two roles migrating at start-up (api and worker) both succeed: the second
// waits and finds nothing to do.
func TestTwoMigrationRunsAtOnceBothSucceed(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	const runs = 3
	errs := make(chan error, runs)
	for range runs {
		go func() { errs <- db.Migrate(ctx, pool) }()
	}
	for range runs {
		if err := <-errs; err != nil {
			t.Errorf("Migrate: %v", err)
		}
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM meta WHERE key = 'instance_id'`).Scan(&applied); err != nil || applied != 1 {
		t.Errorf("instance_id rows = %d (%v), want exactly 1 — the first migration ran once", applied, err)
	}
}
