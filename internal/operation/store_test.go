package operation_test

import (
	"context"
	"errors"
	"fmt"
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

// newAccount adds a fresh brokerage account to the fixture's space, for tests
// that must not inherit a position from an earlier round.
func (f fixture) newAccount(t *testing.T) uuid.UUID {
	t.Helper()
	a, err := f.accStore.Create(f.ctx, f.spaceID, nil, "Брокер "+uuid.NewString(), account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a.ID
}

// lotRows counts the persisted transfer-lot rows of one operation, read from
// the table directly since the store cannot show them once the operation is
// gone.
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

// datep is an acquisition date as lots hold it: a pointer, nil for unknown.
func datep(s string) *time.Time {
	d := date(s)
	return &d
}

// acquired renders an acquisition date for a failure message.
func acquired(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format("2006-01-02")
}

// sameAcquisition compares acquisition dates, unknown included, without
// dereferencing a nil.
func sameAcquisition(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
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
	list, more, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0, operation.JournalFilter{})
	if err != nil || len(list) != 2 || list[0].Type != operation.TypeDeposit {
		t.Fatalf("ListByAccount = %+v, %v", list, err)
	}
	if more {
		t.Errorf("ListByAccount reported more behind a window of 10 holding the whole two-row journal")
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
	if list, _, _ = f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0, operation.JournalFilter{}); len(list) != 1 {
		t.Fatalf("after delete = %d", len(list))
	}
}

// A zero limit would return the probe row, trim the page to nothing and report
// hasMore: an empty journal with a "show more" that loads nothing. The handler
// refuses it first; this enforces the precondition.
func TestListByAccountRefusesNonPositiveLimit(t *testing.T) {
	f := newFixture(t)

	dep := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-05"), AmountMinor: 100_000_00, Currency: "RUB",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, dep, nil); err != nil {
		t.Fatalf("Create dep: %v", err)
	}

	for _, limit := range []int{0, -1} {
		ops, more, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, limit, 0, operation.JournalFilter{})
		if err == nil {
			t.Errorf("ListByAccount(limit=%d) = %d rows, more=%v, err=nil; want a refusal: an empty page with more=true is a button that loads nothing forever",
				limit, len(ops), more)
		}
		if ops != nil || more {
			t.Errorf("ListByAccount(limit=%d) answered %d rows and more=%v beside its refusal; want nothing at all",
				limit, len(ops), more)
		}
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
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
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
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err == nil {
		t.Fatal("CreatePair foreign dest: want error")
	}
	if list, _, _ := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0, operation.JournalFilter{}); len(list) != 0 {
		t.Fatalf("orphan out op left: %d", len(list))
	}
}

func TestEarliestRecordedDay(t *testing.T) {
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

	got, err := f.store.EarliestRecordedDay(f.ctx)
	if err != nil {
		t.Fatalf("EarliestRecordedDay: %v", err)
	}
	if !got.Equal(date("2019-03-12")) {
		t.Fatalf("EarliestRecordedDay = %v, want 2019-03-12", got)
	}
}

// Stated purchases can predate every operation, and their cost is converted
// at their own days, so the fx backfill must reach them.
func TestEarliestRecordedDayReachesAPurchaseOlderThanEveryOperation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	bought := date("2017-05-04")
	cost := int64(100_000)
	if _, err := svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-06-15"),
		Quantity: *dec("10"), Currency: "RUB",
		Purchases: []operation.StatedPurchase{{Quantity: *dec("10"), CostMinor: &cost, AcquiredOn: &bought}},
	}); err != nil {
		t.Fatalf("CreateArrival: %v", err)
	}
	got, err := f.store.EarliestRecordedDay(f.ctx)
	if err != nil {
		t.Fatalf("EarliestRecordedDay: %v", err)
	}
	if !got.Equal(bought) {
		t.Errorf("EarliestRecordedDay = %v, want the purchase's 2017-05-04 — the arrival itself is 2026", got)
	}
}

func TestEarliestRecordedDayEmpty(t *testing.T) {
	f := newFixture(t)

	_, err := f.store.EarliestRecordedDay(f.ctx)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("EarliestRecordedDay on empty table: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestDistinctCurrencies(t *testing.T) {
	f := newFixture(t)

	// Both fixture accounts are RUB; USD appears only in operations.
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

	// The lists differ: operation.Store reads its own table.
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

// transferPair builds a 5-unit SBER pair between the fixture's accounts with
// the breakdown on the arriving leg.
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

// The breakdown survives a write and read: stored with the arrival, back in
// FIFO order, each piece with its purchase day.
func TestTransferLotsRoundTrip(t *testing.T) {
	f := newFixture(t)

	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("3"), CostMinor: 30_000, AcquiredOn: datep("2024-02-11")},
		{Quantity: decimal.RequireFromString("2"), CostMinor: 50_000, AcquiredOn: datep("2025-09-04")},
	}
	out, in := f.transferPair(want)
	_, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
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
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}

	// The departing leg reads the same pieces in the same order.
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
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
			t.Errorf("transfer_out lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	// Read, not copied: stored once.
	if n := f.lotRows(t, srcLeg.ID); n != 0 {
		t.Errorf("transfer_out has %d rows of its own, want 0 — the breakdown is one fact with one owner", n)
	}
}

// An unknown purchase date is stored as NULL and read back as unknown (the
// column allows NULL since migration 0008). The dated piece beside it catches a
// storage layer that substitutes a date or drops the undated piece.
func TestTransferLotWithoutAcquisitionDateRoundTrips(t *testing.T) {
	f := newFixture(t)

	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("3"), CostMinor: 30_000, AcquiredOn: nil},
		{Quantity: decimal.RequireFromString("2"), CostMinor: 50_000, AcquiredOn: datep("2025-09-04")},
	}
	out, in := f.transferPair(want)
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err != nil {
		t.Fatalf("CreatePair: %v — a piece with no acquisition date must be storable", err)
	}

	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	got := findByType(ops, operation.TypeTransferIn)
	if got == nil {
		t.Fatalf("no transfer_in in dest journal")
	}
	if len(got.TransferLots) != len(want) {
		t.Fatalf("stored lots = %+v, want %d pieces — an undated piece is a piece", got.TransferLots, len(want))
	}
	for i, w := range want {
		g := got.TransferLots[i]
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	// Not the transfer's own date.
	if got.TransferLots[0].AcquiredOn != nil {
		t.Errorf("the undated piece read back dated %s, want unknown", acquired(got.TransferLots[0].AcquiredOn))
	}
	// The column really holds NULL.
	var nulls int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM operation_transfer_lots WHERE acquired_on IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count null acquired_on: %v", err)
	}
	if nulls != 1 {
		t.Errorf("rows with acquired_on IS NULL = %d, want 1", nulls)
	}
}

// An ordinary buy in the same journal as a transfer_in reads back with no
// breakdown.
func TestNonTransferOperationsHaveNoLots(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: 80_000, AcquiredOn: datep("2024-02-11")},
	})
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err != nil {
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

// A transfer with no stored breakdown reads back with an empty list and its
// basis unchanged.
func TestTransferWithoutLotsStillReadable(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair(nil)
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err != nil {
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

// The breakdown is written in the pair's transaction: a lot refused by the
// table's CHECK leaves neither operation behind.
func TestTransferLotFailureRollsBackPair(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: -1, AcquiredOn: datep("2024-02-11")},
	})
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err == nil {
		t.Fatal("CreatePair with a rejected lot: want error")
	}
	for _, id := range []uuid.UUID{f.accountID, f.account2ID} {
		ops, _, err := f.store.ListByAccount(f.ctx, f.spaceID, id, 10, 0, operation.JournalFilter{})
		if err != nil {
			t.Fatalf("ListByAccount: %v", err)
		}
		if len(ops) != 0 {
			t.Errorf("account %s kept %d operations, want 0 (pair must roll back with its lots)", id, len(ops))
		}
	}
}

// The breakdown goes with the transfer it describes.
func TestDeleteTransferRemovesLots(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("3"), CostMinor: 30_000, AcquiredOn: datep("2024-02-11")},
		{Quantity: decimal.RequireFromString("2"), CostMinor: 50_000, AcquiredOn: datep("2025-09-04")},
	})
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
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

// The row as stored is replayed before the commit, and a row that fails leaves
// nothing behind. normalizeForStorage makes stored and checked rows equal today;
// this makes it a property.
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
	// The verifier sees the row as the database made it, id and created_at
	// included.
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

	// And it commits when the verifier is satisfied.
	created, err := f.store.Create(f.ctx, f.spaceID, buy, func(operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("Create with a satisfied verifier: %v", err)
	}
	if list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID); err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("journal = %+v (%v), want exactly the committed row %s", list, err, created.ID)
	}
}

// CreatePair's guard: the departing leg replays the pieces stored here (see
// portfolio.Position.releaseRecorded), so the caller confirms the stored pair, and
// refusing it leaves neither leg nor any piece.
func TestCreatePairRollsBackWhatItCannotConfirm(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: 80_000, AcquiredOn: datep("2026-07-01")},
	})
	refuse := errors.New("the transfer as stored no longer replays on the source account")
	var seen operation.Operation
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, func(storedOut, _ operation.Operation) error {
		seen = storedOut
		return refuse
	}); !errors.Is(err, refuse) {
		t.Fatalf("CreatePair = %v, want the verifier's own error", err)
	}
	// The departing leg as stored, breakdown included: the row the source
	// account replays.
	if seen.ID == uuid.Nil || seen.Type != operation.TypeTransferOut || len(seen.TransferLots) != 1 {
		t.Errorf("verifier saw %+v with %d pieces, want the stored transfer_out carrying the parcel's breakdown",
			seen.Type, len(seen.TransferLots))
	}

	for _, accountID := range []uuid.UUID{f.accountID, f.account2ID} {
		list, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatalf("list journal: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("account %s holds %d operations after a refused pair, want none", accountID, len(list))
		}
	}

	// And it commits when the verifier is satisfied.
	_, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, func(operation.Operation, operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("CreatePair with a satisfied verifier: %v", err)
	}
	if n := f.lotRows(t, cIn.ID); n != 1 {
		t.Errorf("committed pair kept %d breakdown rows, want 1", n)
	}
}

// importedDeposit is the plainest delta row: no instrument, no breakdown.
func importedDeposit(f fixture, externalID string, on string, amountMinor int64) operation.Operation {
	return operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date(on),
		AmountMinor: amountMinor, Currency: "RUB", Source: "tinvest", ExternalID: &externalID,
	}
}

// ApplyDelta's guard covers the removals too: a refused delta leaves the
// replaced rows in place.
func TestApplyDeltaRollsBackWhatItCannotConfirm(t *testing.T) {
	f := newFixture(t)

	existing, err := f.store.Create(f.ctx, f.spaceID, importedDeposit(f, "op-old", "2026-07-01", 1_000), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	refuse := errors.New("the delta as stored no longer replays")
	var seen []operation.Operation
	_, err = f.store.ApplyDelta(f.ctx, f.spaceID,
		[]operation.Operation{importedDeposit(f, "op-new", "2026-07-02", 2_000)},
		[]uuid.UUID{existing.ID},
		func(stored []operation.Operation) error {
			seen = stored
			return refuse
		})
	if !errors.Is(err, refuse) {
		t.Fatalf("ApplyDelta = %v, want the verifier's own error", err)
	}
	// The verifier sees the rows as the database made them.
	if len(seen) != 1 || seen[0].ID == uuid.Nil || seen[0].CreatedAt.IsZero() {
		t.Errorf("verifier saw %+v, want the row as stored", seen)
	}

	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 1 || list[0].ID != existing.ID {
		t.Fatalf("journal = %d rows, want the one row the refused delta was going to replace", len(list))
	}

	// And it commits when satisfied: the old row gone, the new one in place.
	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID,
		[]operation.Operation{importedDeposit(f, "op-new", "2026-07-02", 2_000)},
		[]uuid.UUID{existing.ID},
		func([]operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("ApplyDelta with a satisfied verifier: %v", err)
	}
	list, err = f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 1 || list[0].ID != stored[0].ID || list[0].AmountMinor != 2_000 {
		t.Errorf("journal = %+v, want exactly the committed row", list)
	}
}

// Removals run first: a corrected record keeps its external id, which the
// unique index would refuse alongside the old row.
func TestApplyDeltaDeletesBeforeItInserts(t *testing.T) {
	f := newFixture(t)

	existing, err := f.store.Create(f.ctx, f.spaceID, importedDeposit(f, "op-1", "2026-07-01", 1_000), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID,
		[]operation.Operation{importedDeposit(f, "op-1", "2026-07-01", 2_000)},
		[]uuid.UUID{existing.ID}, nil)
	if err != nil {
		t.Fatalf("replacing a row by its own external id: %v", err)
	}
	if len(stored) != 1 || stored[0].AmountMinor != 2_000 {
		t.Fatalf("stored = %+v, want the corrected row", stored)
	}
	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 1 || list[0].AmountMinor != 2_000 {
		t.Errorf("journal = %+v, want just the corrected row", list)
	}
}

// A removal that cannot find every id fails with ErrRemovalCountMismatch,
// not merely some error.
func TestApplyDeltaRefusesARemovalItCannotFind(t *testing.T) {
	f := newFixture(t)

	existing, err := f.store.Create(f.ctx, f.spaceID, importedDeposit(f, "op-1", "2026-07-01", 1_000), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.store.ApplyDelta(f.ctx, f.spaceID, nil,
		[]uuid.UUID{existing.ID, uuid.New()}, nil); !errors.Is(err, operation.ErrRemovalCountMismatch) {
		t.Fatalf("ApplyDelta with an id that is not there: err = %v, want ErrRemovalCountMismatch", err)
	}
	if list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID); err != nil || len(list) != 1 {
		t.Errorf("journal = %d rows (%v), want the row left alone", len(list), err)
	}
}

// An insert into an account outside the space fails with
// ErrAccountNotInSpace rather than a bare pgx.ErrNoRows.
func TestApplyDeltaNamesAnAccountNotInTheSpace(t *testing.T) {
	f := newFixture(t)

	if _, err := f.store.ApplyDelta(f.ctx, uuid.New(),
		[]operation.Operation{importedDeposit(f, "op-1", "2026-07-01", 1_000)}, nil, nil,
	); !errors.Is(err, operation.ErrAccountNotInSpace) {
		t.Fatalf("ApplyDelta into a space the account is not in: err = %v, want ErrAccountNotInSpace", err)
	}
}

// deltaCost is what applying one delta cost, and what it left behind.
type deltaCost struct {
	rows     int
	trips    int64
	acquires int64
	stored   int
	written  int
}

func (c deltaCost) String() string {
	return fmt.Sprintf("%d rows: %d database round trips (%d pool acquisitions, %d rows returned, %d rows in the journal)",
		c.rows, c.trips, c.acquires, c.stored, c.written)
}

// writeDelta applies one delta of n rows on its own database and reports the
// cost.
func writeDelta(t *testing.T, n int) deltaCost {
	t.Helper()
	f := newFixture(t)
	pool, counter := tracedPool(t, f)
	store := operation.NewStore(pool)

	add := make([]operation.Operation, 0, n)
	for i := range n {
		add = append(add, importedDeposit(f, fmt.Sprintf("op-%d", i), "2026-07-01", int64(1_000+i)))
	}

	trips, acquires := counter.n.Load(), pool.Stat().AcquireCount()
	stored, err := store.ApplyDelta(f.ctx, f.spaceID, add, nil, nil)
	cost := deltaCost{
		rows:     n,
		trips:    counter.n.Load() - trips,
		acquires: pool.Stat().AcquireCount() - acquires,
		stored:   len(stored),
	}
	if err != nil {
		t.Fatalf("ApplyDelta of %d rows: %v", n, err)
	}
	// Counted on the fixture's pool after the measurement.
	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	cost.written = len(list)
	return cost
}

// A first broker load is thousands of rows; three hundred must cost what one
// does. Two sizes, because the claim is "the same", not a number. The pool
// acquisition count is always 1 inside a transaction, so the trip count comes
// from the driver (see tripCounter).
func TestApplyDeltaCostsTheSameWhateverItsSize(t *testing.T) {
	small := writeDelta(t, 1)
	large := writeDelta(t, 300)

	// A write that stored nothing must not pass a performance test.
	for _, c := range []deltaCost{small, large} {
		if c.stored != c.rows || c.written != c.rows {
			t.Fatalf("%s — every row must be written and handed back", c)
		}
		if c.acquires != 1 {
			t.Errorf("%s — one delta is one transaction, so it takes the pool exactly once", c)
		}
	}
	t.Logf("%s", small)
	t.Logf("%s", large)

	if large.trips != small.trips {
		t.Fatalf("round trips grew with the delta: %s, against %s", large, small)
	}
}

// ApplyDelta keeps the created_at it is given: the caller checked the rows in
// an order, and a batch's clock may not separate them.
func TestApplyDeltaKeepsTheCreatedAtItWasGiven(t *testing.T) {
	f := newFixture(t)

	first := importedDeposit(f, "op-1", "2026-07-01", 1_000)
	first.CreatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	second := importedDeposit(f, "op-2", "2026-07-01", 2_000)
	second.CreatedAt = time.Date(2026, 7, 1, 10, 0, 0, 1_000, time.UTC) // one microsecond later

	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{first, second}, nil, nil)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !stored[0].CreatedAt.Equal(first.CreatedAt) || !stored[1].CreatedAt.Equal(second.CreatedAt) {
		t.Errorf("stored created_at = %s / %s, want %s / %s",
			stored[0].CreatedAt, stored[1].CreatedAt, first.CreatedAt, second.CreatedAt)
	}
	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 2 || list[0].AmountMinor != 1_000 || list[1].AmountMinor != 2_000 {
		t.Errorf("journal = %+v, want the two rows in the order they were written", list)
	}

	// A row with no stamp gets the database's.
	third := importedDeposit(f, "op-3", "2026-07-02", 3_000)
	stored, err = f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{third}, nil, nil)
	if err != nil {
		t.Fatalf("ApplyDelta without a created_at: %v", err)
	}
	if stored[0].CreatedAt.IsZero() {
		t.Error("row written with no created_at came back with none, want the database's own")
	}
}

// The demo seed writes a journal row by row under one commit. With now() both
// rows below would share the transaction's instant and ListForEngine could return
// the sell first, an oversell; clock_timestamp() gives each its own.
func TestARunOfWritesInsideOneTransactionGetsAnInstantEach(t *testing.T) {
	f := newFixture(t)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	store := operation.NewStore(tx)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	sell := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}
	if _, err := store.Create(f.ctx, f.spaceID, buy, nil); err != nil {
		t.Fatalf("create the buy: %v", err)
	}
	if _, err := store.Create(f.ctx, f.spaceID, sell, nil); err != nil {
		t.Fatalf("create the sell: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil || len(ops) != 2 {
		t.Fatalf("ListForEngine = %+v, %v; want the two rows", ops, err)
	}
	if ops[0].CreatedAt.Equal(ops[1].CreatedAt) {
		t.Fatalf("both rows were written at %s — two rows the engine's listing orders by "+
			"created_at cannot share one, and one transaction's now() gives them exactly that",
			ops[0].CreatedAt)
	}
	if ops[0].Type != operation.TypeBuy || ops[1].Type != operation.TypeSell {
		t.Errorf("the journal reads back as %s then %s, want buy then sell — the order it was written in",
			ops[0].Type, ops[1].Type)
	}
}

// Rows from ListBySource and ByIDs carry their breakdowns, or they would fold
// undated.
func TestListBySourceAndByIDsCarryTheBreakdown(t *testing.T) {
	f := newFixture(t)

	byHand, err := f.store.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-01"),
		AmountMinor: 1_000, Currency: "RUB",
	}, nil)
	if err != nil {
		t.Fatalf("manual row: %v", err)
	}
	fromImport, err := f.store.Create(f.ctx, f.spaceID, importedDeposit(f, "op-1", "2026-07-02", 2_000), nil)
	if err != nil {
		t.Fatalf("imported row: %v", err)
	}
	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: 80_000, AcquiredOn: datep("2026-07-01")},
	})
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	bySource, err := f.store.ListBySource(f.ctx, f.spaceID, f.accountID, "tinvest")
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	if len(bySource) != 1 || bySource[0].ID != fromImport.ID {
		t.Errorf("ListBySource returned %+v, want only the imported row", bySource)
	}

	byIDs, err := f.store.ByIDs(f.ctx, f.spaceID, []uuid.UUID{byHand.ID, cOut.ID, cIn.ID})
	if err != nil {
		t.Fatalf("ByIDs: %v", err)
	}
	if len(byIDs) != 3 {
		t.Fatalf("ByIDs returned %d rows, want 3", len(byIDs))
	}
	departing := findByType(byIDs, operation.TypeTransferOut)
	if departing == nil || len(departing.TransferLots) != 1 {
		t.Fatalf("departing leg came back with %+v, want the parcel's one piece", departing)
	}
	if !sameAcquisition(departing.TransferLots[0].AcquiredOn, datep("2026-07-01")) {
		t.Errorf("piece acquired %s, want 2026-07-01", acquired(departing.TransferLots[0].AcquiredOn))
	}

	// Another space's ids are nobody else's business.
	other, err := f.store.ByIDs(f.ctx, uuid.New(), []uuid.UUID{byHand.ID})
	if err != nil {
		t.Fatalf("ByIDs in another space: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("ByIDs in another space returned %d rows, want none", len(other))
	}
}
