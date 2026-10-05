package operation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// tripCounter counts what a pool sends to Postgres by tracing the driver. The
// pool's AcquireCount cannot see a write path: a transaction holds one connection
// from Begin to Commit, so it reads 1 whatever happens inside (the runs report it
// anyway). Counting store calls would miss a loop inside one call. So: one per
// Query, QueryRow or Exec, one per batch. Prepares are not counted: one per
// distinct SQL per connection, a constant either way.
type tripCounter struct{ n atomic.Int64 }

func (c *tripCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *tripCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *tripCounter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *tripCounter) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (c *tripCounter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// tracedPool opens a second pool on the fixture's database with the counter,
// so the fixture's own writes and reads stay out of the count.
func tracedPool(t *testing.T, f fixture) (*pgxpool.Pool, *tripCounter) {
	t.Helper()
	counter := &tripCounter{}
	// Config returns a deep copy.
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Connect first: the pool is lazy, and the handshake is not measured.
	if err := pool.Ping(f.ctx); err != nil {
		t.Fatalf("ping traced pool: %v", err)
	}
	return pool, counter
}

// transferOfPieces builds a pair whose parcel came from n monthly purchases
// of one unit, the shape #73 names: the pieces grow, not the transfer.
func transferOfPieces(f fixture, n int) (out, in operation.Operation) {
	pieces := make([]operation.ReleasedLot, 0, n)
	day := date("2016-01-04")
	for range n {
		bought := day
		pieces = append(pieces, operation.ReleasedLot{
			Quantity: decimal.RequireFromString("1"), CostMinor: 10_000, AcquiredOn: &bought,
		})
		day = day.AddDate(0, 1, 0)
	}
	quantity := decimal.NewFromInt(int64(n))
	out = operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-10"), Quantity: &quantity,
		AmountMinor: int64(n) * 10_000, Currency: "RUB",
	}
	in = out
	in.AccountID = f.account2ID
	in.Type = operation.TypeTransferIn
	in.TransferLots = pieces
	return out, in
}

// pairCost is what recording one transfer cost, and what it left behind.
type pairCost struct {
	pieces   int
	trips    int64
	acquires int64
	returned int
	rows     int
}

func (c pairCost) String() string {
	return fmt.Sprintf("%d pieces: %d database round trips (%d pool acquisitions, %d pieces returned, %d rows stored)",
		c.pieces, c.trips, c.acquires, c.returned, c.rows)
}

// writeTransfer records one transfer of n pieces on its own database and
// reports what CreatePair cost.
func writeTransfer(t *testing.T, n int) pairCost {
	t.Helper()
	f := newFixture(t)
	pool, counter := tracedPool(t, f)
	store := operation.NewStore(pool)
	out, in := transferOfPieces(f, n)

	trips, acquires := counter.n.Load(), pool.Stat().AcquireCount()
	_, cIn, err := store.CreatePair(f.ctx, f.spaceID, out, in, nil)
	if err != nil {
		t.Fatalf("CreatePair with %d pieces: %v", n, err)
	}
	return pairCost{
		pieces:   n,
		trips:    counter.n.Load() - trips,
		acquires: pool.Stat().AcquireCount() - acquires,
		returned: len(cIn.TransferLots),
		rows:     f.lotRows(t, cIn.ID),
	}
}

// Recording a transfer costs a fixed number of round trips whatever the
// parcel's history (#73): six times the pieces, the same trips. Two runs are
// compared rather than a number pinned.
func TestTransferBreakdownCostsTheSameWhateverItsSize(t *testing.T) {
	small := writeTransfer(t, 2)
	large := writeTransfer(t, 12)

	// Both runs must have stored and returned every piece, or a write that
	// stored nothing would pass.
	for _, c := range []pairCost{small, large} {
		if c.rows != c.pieces || c.returned != c.pieces {
			t.Fatalf("%s — every piece must be written and returned", c)
		}
	}
	t.Logf("%s", small)
	t.Logf("%s", large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the breakdown: %s, against %s", large, small)
	}
}

// A bad piece in the middle of the batch: Postgres discards the statements
// after it, unread, so the caller must still be told the first failure by its
// index, and the pair must leave nothing behind.
func TestCreatePairReportsTheBreakdownPieceThatFailed(t *testing.T) {
	f := newFixture(t)

	out, in := transferOfPieces(f, 4)
	// The table's CHECK (cost_minor >= 0) refuses this one only.
	in.TransferLots[2].CostMinor = -1

	// Acquire and Release update the idle count under the pool's lock, so no
	// settling time is needed.
	idleBefore := f.pool.Stat().IdleConns()

	_, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
	if err == nil {
		t.Fatal("CreatePair with a piece the table refuses: want an error")
	}
	if !strings.Contains(err.Error(), "transfer lot 2") {
		t.Errorf("CreatePair = %v, want the failure named against piece 2 — the piece that is actually wrong, by the loop's own rule of reporting the first error it reads, not a later one it happens to reach", err)
	}

	// The connection came back idle, not destroyed: pgxpool destroys a
	// connection released mid-transaction, which unread batch results followed
	// by a rollback would cause. That shows only in the pool, not in
	// CreatePair's result.
	if idleAfter := f.pool.Stat().IdleConns(); idleAfter != idleBefore {
		t.Errorf("pool has %d idle connections after a refused breakdown, want %d — the connection was destroyed instead of coming back usable", idleAfter, idleBefore)
	}

	for _, accountID := range []uuid.UUID{f.accountID, f.account2ID} {
		list, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatalf("list journal: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("account %s holds %d operations after a refused breakdown, want none", accountID, len(list))
		}
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM operation_transfer_lots`).Scan(&rows); err != nil {
		t.Fatalf("count transfer lots: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d breakdown rows survived a refused pair, want none — the pieces written before the bad one must go with it", rows)
	}

	// And the store keeps working afterwards.
	healthyOut, healthyIn := transferOfPieces(f, 3)
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, healthyOut, healthyIn, nil); err != nil {
		t.Fatalf("a healthy transfer after a refused one: %v", err)
	}
}

// What is checked before the commit is what the database holds. quantity is
// NUMERIC(30,10), so each piece with an eleventh decimal is rounded on the way in:
// the two pieces below sum exactly to the quantity in hand and not as stored. The
// fixture asserts both readings first, or it would prove nothing. The write path
// never builds such pieces (quantizeLots), which is why this goes through the
// store directly.
func TestCreatePairChecksTheBreakdownTheDatabaseKept(t *testing.T) {
	f := newFixture(t)

	// 1.16666666655 per piece: eleven decimals, one more than the column
	// keeps.
	moved := decimal.RequireFromString("2.3333333331")
	pieces := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("1.16666666655"), CostMinor: 35_000, AcquiredOn: datep("2026-07-01")},
		{Quantity: decimal.RequireFromString("1.16666666655"), CostMinor: 70_000, AcquiredOn: datep("2026-07-02")},
	}
	out := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-05"), Quantity: &moved, AmountMinor: 105_000, Currency: "RUB",
	}
	in := out
	in.AccountID = f.account2ID
	in.Type = operation.TypeTransferIn
	in.TransferLots = pieces

	// First: in hand, the pieces add up.
	if err := portfolio.CheckTransferLots(in); err != nil {
		t.Fatalf("the breakdown AS INTENDED does not add up (%v) — then a refusal below would prove nothing about which of the two readings was checked", err)
	}
	// Second: the column really changes them.
	var kept decimal.Decimal
	if err := f.pool.QueryRow(f.ctx, `SELECT $1::numeric(30,10)`, pieces[0].Quantity).Scan(&kept); err != nil {
		t.Fatalf("ask the column what it keeps: %v", err)
	}
	if kept.Equal(pieces[0].Quantity) {
		t.Fatalf("the column kept %s unchanged — the fixture no longer tells what was written apart from what was meant", kept)
	}

	_, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
	if err == nil {
		t.Fatal("CreatePair accepted a breakdown whose stored pieces do not sum to the quantity moved — this is the 201 followed by a positions screen broken forever")
	}
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("CreatePair = %v, want the engine's own refusal of the stored rows (portfolio.ErrBadOperation)", err)
	}

	// And it left nothing behind: neither leg, and none of the pieces.
	for _, accountID := range []uuid.UUID{f.accountID, f.account2ID} {
		list, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatalf("list journal: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("account %s holds %d operations after a refused pair, want none", accountID, len(list))
		}
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM operation_transfer_lots`).Scan(&rows); err != nil {
		t.Fatalf("count transfer lots: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d breakdown rows survived a refused pair, want none", rows)
	}
}
