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

// tripCounter counts what a pool actually sends to Postgres, by tracing the
// driver rather than the store.
//
// It has to sit this low. The obvious instrument — pool.Stat().AcquireCount(),
// which is what the read-path test in http_round_trips_test.go uses — cannot
// see anything here at all: CreatePair opens a transaction, a transaction holds
// ONE connection from Begin to Commit, and every statement it sends afterwards
// travels on that connection without touching the pool again. A hundred
// statements inside a transaction and one statement inside a transaction both
// acquire exactly once, so on a write path that counter is constant by
// construction and would pass whatever this code does. The runs below report it
// anyway, so that the next person to reach for it can see for themselves that it
// does not move.
//
// Counting store calls instead would be worse still, and for the reason this
// project already learned once: a batch replaced by a loop INSIDE one call is
// invisible from above. So the count is taken where the loop would have to
// show: one per Query/QueryRow/Exec the driver sends, one per batch — a batch
// being one write and one wait for the statements it carries, however many
// those are (see pgx.Conn.sendBatchExtendedWithDescription, which queues them
// all and syncs once).
//
// Statement PREPARES are deliberately not counted. They are a round trip too,
// but at most one per distinct SQL per connection on either implementation —
// every piece is written by the same statement — so they are a constant that
// would only add noise to a comparison of two sizes.
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

// tracedPool opens a second pool on the fixture's own database, identical to it
// but for the counter. The fixture's pool is left alone so that the rows the
// fixture itself writes, and the reads the assertions make afterwards, stay out
// of the count.
func tracedPool(t *testing.T, f fixture) (*pgxpool.Pool, *tripCounter) {
	t.Helper()
	counter := &tripCounter{}
	// Config returns a deep copy, so this configures the new pool and nothing
	// else — same connection string, same codecs, same exec mode.
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Connect before anything is measured: the pool is lazy, and the handshake
	// is not what this test is about.
	if err := pool.Ping(f.ctx); err != nil {
		t.Fatalf("ping traced pool: %v", err)
	}
	return pool, counter
}

// transferOfPieces builds a transfer pair whose parcel was assembled from n
// separate purchases — one whole unit bought on a month of its own, which is
// the shape a decade of monthly buying leaves behind, and the case issue #73
// names: the pieces are what grows, not the transfer.
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

// writeTransfer records one transfer of n pieces on a database of its own and
// reports what that single CreatePair cost.
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

// TestTransferBreakdownCostsTheSameWhateverItsSize is the requirement issue #73
// states: recording a transfer costs a FIXED number of round trips, whatever
// the parcel's history. Six times the pieces — the same number of trips.
//
// Two runs of different size rather than one pinned number, deliberately, for
// the reason TestJournalRoundTripsDoNotGrowWithTheData gives about the read
// path: a magic number bakes in today's incidental statements (the BEGIN, the
// two legs, the COMMIT) and the next person to add or remove one edits the
// expectation without ever learning whether the property still holds. What must
// be true is not "five", it is "the same".
func TestTransferBreakdownCostsTheSameWhateverItsSize(t *testing.T) {
	small := writeTransfer(t, 2)
	large := writeTransfer(t, 12)

	// A write that stored nothing is the cheapest write there is, and must not
	// be able to pass a performance test: both runs have to have actually put
	// every piece in the table and handed every piece back.
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

// TestCreatePairReportsTheBreakdownPieceThatFailed pins what a failure PART WAY
// THROUGH the breakdown does, which is the one behaviour a batch could quietly
// have changed: the statements no longer fail one at a time, they are all sent
// before any of them is read.
//
// TestTransferLotFailureRollsBackPair already covers a first piece the table
// refuses. This one puts the bad piece in the MIDDLE, where sending and
// reporting come apart: every statement is already queued and sent before any
// result is read, so the pieces after the bad one are never executed at all —
// Postgres discards them, unread, once it hits the one the CHECK constraint
// rejects, and they come back with no result of their own, not a distinguishable
// error to mix up with the real one. The caller must still be told about the
// piece that is wrong — the first failure, by its own index — and the pair must
// still leave nothing behind, exactly as when each statement waited its turn.
func TestCreatePairReportsTheBreakdownPieceThatFailed(t *testing.T) {
	f := newFixture(t)

	out, in := transferOfPieces(f, 4)
	// The table's own CHECK (cost_minor >= 0) refuses this one, and only this
	// one.
	in.TransferLots[2].CostMinor = -1

	// Idle connections join and leave this count synchronously — Acquire and
	// Release both run under the pool's own lock — so a reading taken right
	// before and right after CreatePair needs no settling time either side.
	idleBefore := f.pool.Stat().IdleConns()

	_, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
	if err == nil {
		t.Fatal("CreatePair with a piece the table refuses: want an error")
	}
	if !strings.Contains(err.Error(), "transfer lot 2") {
		t.Errorf("CreatePair = %v, want the failure named against piece 2 — the piece that is actually wrong, by the loop's own rule of reporting the first error it reads, not a later one it happens to reach", err)
	}

	// The connection came back to the pool IDLE, not destroyed. pgxpool
	// destroys any connection it is asked to release with a transaction status
	// other than idle (see (*pgxpool.Conn).Release), and a batch whose results
	// are left unread when the transaction then rolls back is exactly what
	// leaves a connection in that state — this is the one way the batched
	// version could break a caller that the loop could not. It does not show up
	// as an error CreatePair returns, only as churn in the pool, so it has to be
	// read off the pool rather than off CreatePair's return value.
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

	// A healthy transfer afterwards is a second, independent confirmation. It
	// does not by itself prove the earlier connection survived — the pool has
	// spare capacity to open a fresh one, so this call could pass even if the
	// old connection had been thrown away — but it is here so a reader who
	// doubts the idle-count check above can watch the store actually keep
	// working rather than take the count on faith.
	healthyOut, healthyIn := transferOfPieces(f, 3)
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, healthyOut, healthyIn, nil); err != nil {
		t.Fatalf("a healthy transfer after a refused one: %v", err)
	}
}

// TestCreatePairChecksTheBreakdownTheDatabaseKept is the property the batching
// above was not allowed to cost, and the one this project has been burned by
// twice: what is checked before the commit must be what the DATABASE HOLDS, not
// what the caller was about to write.
//
// quantity is NUMERIC(30,10), so a piece with an eleventh decimal place is
// rounded on the way in — each piece on its own. The fixture below is the
// historical fault in miniature: two pieces that sum EXACTLY to the quantity the
// operation claims to move, and whose stored halves do not, because both round
// up. Checking the pieces in hand therefore passes and checking the pieces in
// the table fails, which is what makes this test able to tell the two apart at
// all. It asserts that outright before asserting anything else — a fixture whose
// two readings happened to agree would prove nothing while looking like proof.
//
// The write path itself never builds such a breakdown (operation.quantizeLots
// brings the pieces onto the scale first), which is exactly why this goes
// through the store directly: the guard exists for the case upstream care did
// not anticipate, and a test that only exercised the anticipated one would not
// be testing the guard.
func TestCreatePairChecksTheBreakdownTheDatabaseKept(t *testing.T) {
	f := newFixture(t)

	// Two purchases of 3.5 units, halved and halved again by a 1:3 reverse
	// split — 3.5 × 0.3333333333 = 1.16666666655, eleven decimal places, one
	// more than the column keeps.
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

	// The two halves of the discrimination, stated before they are relied on.
	// First: as the caller holds them, the pieces add up perfectly, so a check
	// run on them cannot fail and a store that ran it there would commit.
	if err := portfolio.CheckTransferLots(in); err != nil {
		t.Fatalf("the breakdown AS INTENDED does not add up (%v) — then a refusal below would prove nothing about which of the two readings was checked", err)
	}
	// Second: the column really does change these pieces. If it ever stopped
	// doing so — a wider scale, a different type — the fixture would go on
	// passing while testing nothing.
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
