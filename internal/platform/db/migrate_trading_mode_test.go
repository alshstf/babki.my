package db_test

import (
	"context"
	"testing"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

const tradingModeMigration = 26

// The upgrade recovers each mirror row's trading mode from the stored broker
// payload: an exchange board, FINEX_OTC, an unnamed board, and none.
func TestMigrate_TradingModeIsRecoveredFromTheStoredPayload(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tradingModeMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")
	connectionID := insertTinvestConnection(t, ctx, pool, spaceID)
	linkID := insertTinvestLink(t, ctx, pool, connectionID, spaceID, accountID, "2000000001")

	rows := []struct {
		opID string
		raw  string
		want string
	}{
		{"exchange-board", `{"classCode": "TQBR"}`, "TQBR"},
		{"off-exchange", `{"classCode": "FINEX_OTC"}`, "FINEX_OTC"},
		// Kept verbatim; naming it is decided elsewhere.
		{"unnamed-board", `{"classCode": "SPBXM"}`, "SPBXM"},
		{"no-such-field", `{"quantity": "1000"}`, ""},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tinvest_operations_mirror
			    (connection_id, link_id, broker_operation_id, op_type, state, occurred_at, currency, payment, quantity, raw, content_key, last_confirmed_at)
			 VALUES ($1, $2, $3, 'OPERATION_TYPE_BUY', 'OPERATION_STATE_EXECUTED', now(), 'RUB', -1700, 1, $4::jsonb, $3, now())`,
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
			`SELECT class_code FROM tinvest_operations_mirror WHERE broker_operation_id = $1`, r.opID).
			Scan(&got); err != nil {
			t.Fatalf("read class_code of %s: %v", r.opID, err)
		}
		if got != r.want {
			t.Errorf("%s: class_code = %q, want %q", r.opID, got, r.want)
		}
	}
}

// operations.trading_mode is not backfilled: the importer is its only writer
// and fills it on the next rebuild.
func TestMigrate_TheJournalsTradingModeIsNotBackfilled(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, tradingModeMigration-1)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")

	if _, err := pool.Exec(ctx,
		`INSERT INTO operations (space_id, account_id, type, occurred_on, amount_minor, currency, source, external_id)
		 VALUES ($1, $2, 'deposit', '2026-08-01', 100000, 'RUB', 'tinvest', 'op-1')`,
		spaceID, accountID); err != nil {
		t.Fatalf("insert operation: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var mode *string
	if err := pool.QueryRow(ctx,
		`SELECT trading_mode FROM operations WHERE external_id = 'op-1'`).Scan(&mode); err != nil {
		t.Fatalf("read trading_mode: %v", err)
	}
	if mode != nil {
		t.Errorf("trading_mode = %q, want nothing: the journal's copy is the importer's to write, "+
			"and a migration writing it too would be a second writer of one column", *mode)
	}
}

// The broker's instant (migration 33) is not backfilled either.
func TestMigrate_TheJournalsInstantIsNotBackfilled(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	upTo(t, ctx, pool, 32)
	spaceID := insertTinvestSpace(t, ctx, pool)
	accountID := insertTinvestAccount(t, ctx, pool, spaceID, "Т-Инвестиции")
	if _, err := pool.Exec(ctx,
		`INSERT INTO operations (space_id, account_id, type, occurred_on, amount_minor, currency, source, external_id)
		 VALUES ($1, $2, 'deposit', '2026-08-01', 100000, 'RUB', 'tinvest', 'op-1')`,
		spaceID, accountID); err != nil {
		t.Fatalf("insert operation: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var at *string
	if err := pool.QueryRow(ctx, `SELECT occurred_at::text FROM operations WHERE external_id = 'op-1'`).Scan(&at); err != nil {
		t.Fatalf("read occurred_at: %v", err)
	}
	if at != nil {
		t.Errorf("occurred_at = %s, want nothing until the importer writes it", *at)
	}
}
