package db_test

import (
	"context"
	"testing"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

const tradingModeMigration = 26

// TestMigrate_TradingModeIsRecoveredFromTheStoredPayload is the upgrade half
// of making the trading mode visible. The mirror has kept the broker's payload
// verbatim from the first day, so an installation that already imported its
// history recovers the mode of every operation from what it stored — nobody
// has to ask the broker again for a field that was in the bytes all along.
//
// The rows are the shapes the live mirror holds on the owner's own account: an
// ordinary Moscow Exchange board, the broker's own over-the-counter dealing in
// the FinEx funds, a Saint Petersburg board this program cannot name, and an
// operation the broker sent no such field for at all — money moving in or out,
// which describes no instrument.
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
		// The owner's own: the funds he bought after exchange trading in them
		// stopped, and the case this whole column exists for.
		{"off-exchange", `{"classCode": "FINEX_OTC"}`, "FINEX_OTC"},
		// A code this program has no source for. The mirror keeps it exactly
		// the same way: what can be NAMED is decided later and elsewhere, and
		// nothing about that decision belongs in the column.
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

// TestMigrate_TheJournalsTradingModeIsNotBackfilled states the other half of
// the same migration, and it is a deliberate ABSENCE rather than an omission.
//
// operations.trading_mode is filled by the importer alone: it recomputes the
// whole desired journal from the mirror and diffs it against what is stored,
// so an imported row gets its mode on the next sync by the one path that
// writes imported rows at all. A backfill here would be a second writer of one
// column, and the two would answer differently the first time a rule changed —
// the class of fault this codebase has watched happen more than once.
//
// The row below is what an existing installation holds: an imported operation
// written before the column existed. It must come out of the migration
// carrying nothing, so that the difference the next rebuild computes is what
// puts the mode there.
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
