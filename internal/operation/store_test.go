package operation_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

type fixture struct {
	store      *operation.Store
	accStore   *account.Store
	pool       *pgxpool.Pool
	spaceID    uuid.UUID
	accountID  uuid.UUID
	account2ID uuid.UUID
	sberID     uuid.UUID
	ctx        context.Context
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID)
	if err != nil {
		t.Fatalf("space: %v", err)
	}
	acc := account.NewStore(pool)
	a1, err := acc.Create(ctx, sp.ID, nil, "Брокер", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("acc1: %v", err)
	}
	a2, err := acc.Create(ctx, sp.ID, nil, "Брокер 2", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("acc2: %v", err)
	}
	sber, err := instrument.NewStore(pool).Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("sber: %v", err)
	}
	return fixture{
		store: operation.NewStore(pool), accStore: acc, pool: pool, spaceID: sp.ID,
		accountID: a1.ID, account2ID: a2.ID, sberID: sber.ID, ctx: ctx,
	}
}

// lotRows counts the transfer-lot rows persisted for one operation, reading
// the table directly: whether the rows are really gone after a delete cannot
// be observed through the store's own API once the operation itself is gone.
func (f fixture) lotRows(t *testing.T, operationID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM operation_transfer_lots WHERE operation_id = $1`,
		operationID).Scan(&n); err != nil {
		t.Fatalf("count transfer lots: %v", err)
	}
	return n
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func TestCreateListDelete(t *testing.T) {
	f := newFixture(t)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("305.5"),
		AmountMinor: -305_500, Currency: "RUB", FeeMinor: 92,
	}
	created, err := f.store.Create(f.ctx, f.spaceID, buy, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || !created.Quantity.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("created = %+v", created)
	}

	// foreign space rejected
	if _, err := f.store.Create(f.ctx, uuid.New(), buy, nil); err == nil {
		t.Fatal("foreign space Create: want error")
	}

	// list DESC
	dep := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-05"), AmountMinor: 100_000_00, Currency: "RUB",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, dep, nil); err != nil {
		t.Fatalf("Create dep: %v", err)
	}
	list, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0)
	if err != nil || len(list) != 2 || list[0].Type != operation.TypeDeposit {
		t.Fatalf("ListByAccount = %+v, %v", list, err)
	}
	// engine order ASC
	asc, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil || len(asc) != 2 || asc[0].Type != operation.TypeBuy {
		t.Fatalf("ListForEngine = %+v, %v", asc, err)
	}

	// delete
	if n, err := f.store.Delete(f.ctx, f.spaceID, created.ID); err != nil || n != 1 {
		t.Fatalf("Delete = %d, %v", n, err)
	}
	if list, _ = f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 1 {
		t.Fatalf("after delete = %d", len(list))
	}
}

func TestTransferPairAtomicity(t *testing.T) {
	f := newFixture(t)

	out := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-10"), Quantity: dec("5"), AmountMinor: 150_000, Currency: "RUB",
	}
	in := operation.Operation{
		AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-10"), Quantity: dec("5"), AmountMinor: 150_000, Currency: "RUB",
	}
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in)
	if err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if cOut.TransferGroupID == nil || cIn.TransferGroupID == nil ||
		*cOut.TransferGroupID != *cIn.TransferGroupID {
		t.Fatalf("group ids: %+v %+v", cOut.TransferGroupID, cIn.TransferGroupID)
	}

	// deleting one deletes the whole group
	if n, err := f.store.Delete(f.ctx, f.spaceID, cIn.ID); err != nil || n != 2 {
		t.Fatalf("Delete group = %d, %v", n, err)
	}

	// pair with a foreign-space destination is fully rejected (atomicity)
	in.AccountID = uuid.New()
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in); err == nil {
		t.Fatal("CreatePair foreign dest: want error")
	}
	if list, _ := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 0 {
		t.Fatalf("orphan out op left: %d", len(list))
	}
}

func TestEarliestOccurredOn(t *testing.T) {
	f := newFixture(t)

	old := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2019-03-12"), AmountMinor: 1000, Currency: "RUB",
	}
	recent := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-20"), AmountMinor: 2000, Currency: "RUB",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, recent, nil); err != nil {
		t.Fatalf("Create recent: %v", err)
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, old, nil); err != nil {
		t.Fatalf("Create old: %v", err)
	}

	got, err := f.store.EarliestOccurredOn(f.ctx)
	if err != nil {
		t.Fatalf("EarliestOccurredOn: %v", err)
	}
	if !got.Equal(date("2019-03-12")) {
		t.Fatalf("EarliestOccurredOn = %v, want 2019-03-12", got)
	}
}

func TestEarliestOccurredOnEmpty(t *testing.T) {
	f := newFixture(t)

	_, err := f.store.EarliestOccurredOn(f.ctx)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("EarliestOccurredOn on empty table: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestDistinctCurrencies(t *testing.T) {
	f := newFixture(t)

	// both fixture accounts are RUB; add operations in RUB and in USD, a
	// currency that does not appear on any account.
	rub := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1000, Currency: "RUB",
	}
	usd := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-02"), AmountMinor: 2000, Currency: "USD",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, rub, nil); err != nil {
		t.Fatalf("Create rub: %v", err)
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, usd, nil); err != nil {
		t.Fatalf("Create usd: %v", err)
	}

	got, err := f.store.DistinctCurrencies(f.ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies: %v", err)
	}
	want := []string{"RUB", "USD"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation DistinctCurrencies = %v, want %v", got, want)
	}

	// USD exists only in operations, not on any account: the two lists
	// must differ, proving operation.Store queries its own table rather
	// than delegating to account currencies.
	accCurrencies, err := f.accStore.DistinctCurrencies(f.ctx)
	if err != nil {
		t.Fatalf("account DistinctCurrencies: %v", err)
	}
	if !reflect.DeepEqual(accCurrencies, []string{"RUB"}) {
		t.Fatalf("account DistinctCurrencies = %v, want [RUB]", accCurrencies)
	}
}

func TestDistinctCurrenciesEmpty(t *testing.T) {
	f := newFixture(t)
	// no operations created

	got, err := f.store.DistinctCurrencies(f.ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies on empty table: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("DistinctCurrencies on empty table = %v, want empty, got %v", got, got)
	}
}

// transferPair builds a 5-unit SBER transfer pair between the fixture's two
// accounts, with the given breakdown riding on the receiving leg.
func (f fixture) transferPair(lots []operation.ReleasedLot) (out, in operation.Operation) {
	out = operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-10"), Quantity: dec("5"), AmountMinor: 80_000, Currency: "RUB",
	}
	in = out
	in.AccountID = f.account2ID
	in.Type = operation.TypeTransferIn
	in.TransferLots = lots
	return out, in
}

func findByType(ops []operation.Operation, typ operation.Type) *operation.Operation {
	for i := range ops {
		if ops[i].Type == typ {
			return &ops[i]
		}
	}
	return nil
}

// TestTransferLotsRoundTrip pins the breakdown surviving a write/read cycle:
// the pieces are stored with the receiving operation and come back in FIFO
// order, each keeping the day its source lot was acquired — the whole point
// of the table, since without it the destination can only date the arrived
// position on the transfer day.
func TestTransferLotsRoundTrip(t *testing.T) {
	f := newFixture(t)

	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("3"), CostMinor: 30_000, AcquiredOn: date("2024-02-11")},
		{Quantity: decimal.RequireFromString("2"), CostMinor: 50_000, AcquiredOn: date("2025-09-04")},
	}
	out, in := f.transferPair(want)
	_, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in)
	if err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if len(cIn.TransferLots) != len(want) {
		t.Fatalf("returned transfer_in lots = %d, want %d", len(cIn.TransferLots), len(want))
	}

	destOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("ListForEngine dest: %v", err)
	}
	got := findByType(destOps, operation.TypeTransferIn)
	if got == nil {
		t.Fatalf("no transfer_in in dest journal")
	}
	if len(got.TransferLots) != len(want) {
		t.Fatalf("stored lots = %+v, want %d pieces", got.TransferLots, len(want))
	}
	for i, w := range want {
		g := got.TransferLots[i]
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !g.AcquiredOn.Equal(w.AcquiredOn) {
			t.Errorf("lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, g.AcquiredOn.Format("2006-01-02"),
				w.Quantity, w.CostMinor, w.AcquiredOn.Format("2006-01-02"))
		}
	}

	// The rows are stored next to the arrival, but they describe the parcel,
	// not the arrival — so the departing leg is read with the very same pieces,
	// in the very same order. Without them it was the last row in the system
	// converting a basis assembled from old purchases at the rate of the day
	// the shares changed brokers, contradicting both of the destination's
	// screens about the same shares.
	srcOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine source: %v", err)
	}
	srcLeg := findByType(srcOps, operation.TypeTransferOut)
	if srcLeg == nil {
		t.Fatalf("no transfer_out in source journal")
	}
	if len(srcLeg.TransferLots) != len(want) {
		t.Fatalf("transfer_out lots = %+v, want the same %d pieces the arriving leg has", srcLeg.TransferLots, len(want))
	}
	for i, w := range want {
		g := srcLeg.TransferLots[i]
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !g.AcquiredOn.Equal(w.AcquiredOn) {
			t.Errorf("transfer_out lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, g.AcquiredOn.Format("2006-01-02"),
				w.Quantity, w.CostMinor, w.AcquiredOn.Format("2006-01-02"))
		}
	}
	// Read, not copied: the pieces still live in exactly one place, attached to
	// the operation that stores them.
	if n := f.lotRows(t, srcLeg.ID); n != 0 {
		t.Errorf("transfer_out has %d rows of its own, want 0 — the breakdown is one fact with one owner", n)
	}
}

// TestNonTransferOperationsHaveNoLots guards against the read attaching one
// operation's breakdown to another: an ordinary buy sitting in the same
// journal as a transfer_in must come back with an empty list.
func TestNonTransferOperationsHaveNoLots(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: 80_000, AcquiredOn: date("2024-02-11")},
	})
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	buy := operation.Operation{
		AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-12"), Quantity: dec("1"), Price: dec("300"),
		AmountMinor: -30_000, Currency: "RUB",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, buy, nil); err != nil {
		t.Fatalf("Create buy: %v", err)
	}

	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	got := findByType(ops, operation.TypeBuy)
	if got == nil {
		t.Fatalf("no buy in journal")
	}
	if len(got.TransferLots) != 0 {
		t.Errorf("buy lots = %+v, want none", got.TransferLots)
	}
}

// TestTransferWithoutLotsStillReadable covers the transfers recorded before
// this table existed: there is no breakdown for them and none can be
// invented, so they must simply read back with an empty list and an
// unchanged carried basis.
func TestTransferWithoutLotsStillReadable(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair(nil)
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}

	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	got := findByType(ops, operation.TypeTransferIn)
	if got == nil {
		t.Fatalf("no transfer_in in dest journal")
	}
	if len(got.TransferLots) != 0 {
		t.Errorf("lots = %+v, want none", got.TransferLots)
	}
	if got.AmountMinor != 80_000 {
		t.Errorf("carried basis = %d, want 80000", got.AmountMinor)
	}
}

// TestTransferLotFailureRollsBackPair pins that the breakdown is written in
// the same transaction as the pair: a transfer half-recorded without the lots
// it moved is not a state the store may leave behind. The lot below is
// rejected by the table's own CHECK, so if the pieces were written after the
// pair was committed the two operations would survive without them.
func TestTransferLotFailureRollsBackPair(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: -1, AcquiredOn: date("2024-02-11")},
	})
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in); err == nil {
		t.Fatal("CreatePair with a rejected lot: want error")
	}
	for _, id := range []uuid.UUID{f.accountID, f.account2ID} {
		ops, err := f.store.ListByAccount(f.ctx, f.spaceID, id, 10, 0)
		if err != nil {
			t.Fatalf("ListByAccount: %v", err)
		}
		if len(ops) != 0 {
			t.Errorf("account %s kept %d operations, want 0 (pair must roll back with its lots)", id, len(ops))
		}
	}
}

// TestDeleteTransferRemovesLots pins that the breakdown cannot outlive the
// operation it describes — the rows go with the transfer, without the store
// having to remember to remove them.
func TestDeleteTransferRemovesLots(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("3"), CostMinor: 30_000, AcquiredOn: date("2024-02-11")},
		{Quantity: decimal.RequireFromString("2"), CostMinor: 50_000, AcquiredOn: date("2025-09-04")},
	})
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in)
	if err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if n := f.lotRows(t, cIn.ID); n != 2 {
		t.Fatalf("lot rows after create = %d, want 2", n)
	}

	// deleting via the *other* leg still takes the whole group with it
	if n, err := f.store.Delete(f.ctx, f.spaceID, cOut.ID); err != nil || n != 2 {
		t.Fatalf("Delete = %d, %v", n, err)
	}
	if n := f.lotRows(t, cIn.ID); n != 0 {
		t.Errorf("lot rows after delete = %d, want 0", n)
	}
}

func TestExternalIDDedup(t *testing.T) {
	f := newFixture(t)
	ext := "broker-op-1"
	op := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1000, Currency: "RUB",
		Source: "csv", ExternalID: &ext,
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, op, nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, op, nil); err == nil {
		t.Fatal("duplicate external_id: want error")
	}
}

// TestCreateRollsBackWhatItCannotConfirm pins the guard that stands
// between the journal and the one thing no amount of care upstream can rule
// out: that the row Postgres keeps is not the row that was checked.
//
// Quantity and split_ratio are stored on a fixed scale, so a value can come
// back from the INSERT a shade different from the one that went in. The service
// brings both onto that scale first (operation.normalizeForStorage), which is
// what makes the two equal today — but "today they are equal" is an argument,
// and this is the mechanism that makes it a property: the row as stored is
// replayed before the transaction commits, and a row that fails leaves nothing
// behind. The same guard protects a transfer's breakdown (see CreatePair).
func TestCreateRollsBackWhatItCannotConfirm(t *testing.T) {
	f := newFixture(t)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}

	refuse := errors.New("the operation as stored no longer replays")
	var seen operation.Operation
	if _, err := f.store.Create(f.ctx, f.spaceID, buy, func(stored operation.Operation) error {
		seen = stored
		return refuse
	}); !errors.Is(err, refuse) {
		t.Fatalf("Create = %v, want the verifier's own error", err)
	}
	// The verifier is handed the row as the database made it — id, created_at
	// and all — not the argument echoed back, or it could not detect anything
	// the database did.
	if seen.ID == uuid.Nil || seen.CreatedAt.IsZero() {
		t.Errorf("verifier saw %+v, want the row as stored", seen)
	}

	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("journal holds %d operations after a refused write, want none — a rolled-back row that survives is the bug this guards", len(list))
	}

	// And it commits when the verifier is satisfied, or it would be a very
	// thorough way of never writing anything.
	created, err := f.store.Create(f.ctx, f.spaceID, buy, func(operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("Create with a satisfied verifier: %v", err)
	}
	if list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID); err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("journal = %+v (%v), want exactly the committed row %s", list, err, created.ID)
	}
}
