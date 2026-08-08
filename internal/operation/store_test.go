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

// datep is date as an acquisition date a lot actually knows: portfolio.Lot and
// portfolio.ReleasedLot hold that date as a pointer, nil meaning the lot does
// not know when it was acquired (see portfolio.Lot.AcquiredOn).
func datep(s string) *time.Time {
	d := date(s)
	return &d
}

// acquired renders an acquisition date for a failure message, naming the
// unknown case instead of printing a stand-in date for it.
func acquired(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format("2006-01-02")
}

// sameAcquisition compares two acquisition dates including the unknown case:
// two unknowns match, an unknown never matches a date. Assertions go through
// it rather than calling Equal on a pointer, which panics on an unknown date
// and would turn a lost date into a crash in the test's own reporting.
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
	list, more, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0)
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
	if list, _, _ = f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 1 {
		t.Fatalf("after delete = %d", len(list))
	}
}

// A page size of zero used to be documented as forbidden and accepted anyway,
// and what it produced was the worst answer available: LIMIT 0+1 returns the
// probe row, the trim leaves an EMPTY page, and hasMore comes back true — a
// journal that shows nothing while insisting there is more, behind a «показать
// ещё» button that loads nothing however many times it is pressed. Whether the
// journal continues is the one thing this method exists to answer and the one
// thing a caller cannot check for itself, so it refuses instead of answering
// wrongly. Today's only caller defaults and refuses before it reaches here
// (parsePage, called from handleListByAccount); this is the precondition being
// enforced rather than merely written down for the next one.
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
		ops, more, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, limit, 0)
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
	if list, _, _ := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 0 {
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
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
			t.Errorf("transfer_out lot %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	// Read, not copied: the pieces still live in exactly one place, attached to
	// the operation that stores them.
	if n := f.lotRows(t, srcLeg.ID); n != 0 {
		t.Errorf("transfer_out has %d rows of its own, want 0 — the breakdown is one fact with one owner", n)
	}
}

// TestTransferLotWithoutAcquisitionDateRoundTrips pins that the absence of a
// date survives storage as an absence. The column was NOT NULL until migration
// 0008, which meant a piece whose source lot did not know its purchase day
// could not be written at all unless some day was made up for it. Now the row
// holds NULL and reads back as "unknown".
//
// The mixed breakdown below is the realistic shape, not a contrived one: a
// parcel is moved on a second time and part of it came from an earlier
// transfer that carried no dates. The dated piece beside it is what makes the
// test discriminating — a storage layer that lost the distinction by
// substituting a date, or by dropping the undated piece, changes one of these
// two rows and not the other.
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
	// Said outright: the unknown must not come back as the transfer's own date,
	// which is the value this whole change removed from the write path.
	if got.TransferLots[0].AcquiredOn != nil {
		t.Errorf("the undated piece read back dated %s, want unknown", acquired(got.TransferLots[0].AcquiredOn))
	}
	// And the column really holds NULL, rather than the read papering over some
	// substitute value the write put there.
	var nulls int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM operation_transfer_lots WHERE acquired_on IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count null acquired_on: %v", err)
	}
	if nulls != 1 {
		t.Errorf("rows with acquired_on IS NULL = %d, want 1", nulls)
	}
}

// TestNonTransferOperationsHaveNoLots guards against the read attaching one
// operation's breakdown to another: an ordinary buy sitting in the same
// journal as a transfer_in must come back with an empty list.
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

// TestTransferWithoutLotsStillReadable covers the transfers recorded before
// this table existed: there is no breakdown for them and none can be
// invented, so they must simply read back with an empty list and an
// unchanged carried basis.
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

// TestTransferLotFailureRollsBackPair pins that the breakdown is written in
// the same transaction as the pair: a transfer half-recorded without the lots
// it moved is not a state the store may leave behind. The lot below is
// rejected by the table's own CHECK, so if the pieces were written after the
// pair was committed the two operations would survive without them.
func TestTransferLotFailureRollsBackPair(t *testing.T) {
	f := newFixture(t)

	out, in := f.transferPair([]operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("5"), CostMinor: -1, AcquiredOn: datep("2024-02-11")},
	})
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in, nil); err == nil {
		t.Fatal("CreatePair with a rejected lot: want error")
	}
	for _, id := range []uuid.UUID{f.accountID, f.account2ID} {
		ops, _, err := f.store.ListByAccount(f.ctx, f.spaceID, id, 10, 0)
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

// TestCreatePairRollsBackWhatItCannotConfirm is the twin of the test above, for
// the write the departing leg's release now depends on.
//
// Create has always replayed the row AS STORED before committing. CreatePair had
// no such hook, which was defensible while the transfer_out row was inert — the
// engine took a fresh FIFO slice for it and read nothing off the row but its
// quantity. It is not inert any more: the pieces stored in this transaction are
// the ones the source account gives up at every later read (see
// portfolio.Position.releaseRecorded), so the row that will be replayed from now
// on is this one, with whatever the columns rounded to, and not the candidate
// the service checked in memory. This pins that the caller gets to look at that
// row, and that refusing it leaves nothing behind — neither leg, and none of the
// pieces.
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
	// The DEPARTING leg, as stored, breakdown and all: that is the operation the
	// source account replays, so it is the one worth confirming.
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

// importedDeposit is the plainest row a batch delta carries: no instrument, no
// breakdown, one broker record behind it.
func importedDeposit(f fixture, externalID string, on string, amountMinor int64) operation.Operation {
	return operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date(on),
		AmountMinor: amountMinor, Currency: "RUB", Source: "tinvest", ExternalID: &externalID,
	}
}

// TestApplyDeltaRollsBackWhatItCannotConfirm is Create's guard at batch size,
// and it covers the half a single insert does not have: the REMOVALS. A delta
// that deletes some rows and writes others is one transaction or it is a
// journal in a state nobody asked for — the rows that were to be replaced gone,
// the ones replacing them never written.
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
	// The verifier is handed the rows as the database made them — not the
	// arguments echoed back, or it could not detect anything the database did.
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

	// And it commits when the verifier is satisfied: the old row gone, the new
	// one in its place.
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

// TestApplyDeltaDeletesBeforeItInserts pins the order the two halves run in,
// and it is not a preference: a broker record that was corrected keeps its
// identity, so the row replacing it carries the very external id the row being
// removed still holds. Insert first and the unique index refuses a correction
// that is perfectly legitimate.
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

// TestApplyDeltaRefusesARemovalItCannotFind pins that "remove these ids" means
// all of them: a caller that computed its difference against a journal that has
// since moved must be told, not quietly obeyed in part.
//
// The check is against ErrRemovalCountMismatch specifically, not merely
// err != nil: any error at all used to satisfy this test, which could not
// tell "the removal did not fully match" apart from an unrelated failure of
// the write itself — exactly the kind of ambiguity later callers (see
// Service.ApplyImportDelta) need to be able to rule out.
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

// TestApplyDeltaNamesAnAccountNotInTheSpace pins the sentinel behind an insert
// whose account does not belong to the caller's own space — insertSQL's own
// guard (see its doc), surfaced as a value later callers of ApplyDelta can
// test for instead of a bare pgx.ErrNoRows that explains nothing on its own.
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

// writeDelta applies one delta of n rows on a database of its own and reports
// what it cost.
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
	// Counted through the fixture's own pool, after the measurement, so the
	// check does not appear in it.
	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	cost.written = len(list)
	return cost
}

// TestApplyDeltaCostsTheSameWhateverItsSize is the reason this method exists at
// all: the first load of a broker's history is thousands of operations, and one
// round trip apiece is what makes it unusable. Three hundred rows must cost what
// one row costs.
//
// Two sizes rather than one pinned number, as the transfer breakdown's twin
// test explains: what must be true is not "four", it is "the same".
//
// THE POOL ACQUISITION COUNT IS REPORTED, NOT RELIED ON. A transaction takes
// one connection at Begin and sends everything else down it, so that counter
// reads 1 whatever this code does — it can tell you the delta is one
// transaction, and nothing at all about how many statements travelled inside
// it. The trip count comes off the driver instead (see tripCounter).
func TestApplyDeltaCostsTheSameWhateverItsSize(t *testing.T) {
	small := writeDelta(t, 1)
	large := writeDelta(t, 300)

	// A write that stored nothing is the cheapest write there is, and must not
	// be able to pass a performance test.
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

// TestApplyDeltaKeepsTheCreatedAtItWasGiven pins the one column this path sets
// by hand. Everything a transaction inserts shares one now(), and rows of the
// same date sharing one created_at leave the engine's own listing nothing to
// order them by — so the caller that checked them in an order gets to state it.
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

	// A row that names no time still gets the database's own, as every other
	// write path does.
	third := importedDeposit(f, "op-3", "2026-07-02", 3_000)
	stored, err = f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{third}, nil, nil)
	if err != nil {
		t.Fatalf("ApplyDelta without a created_at: %v", err)
	}
	if stored[0].CreatedAt.IsZero() {
		t.Error("row written with no created_at came back with none, want the database's own")
	}
}

// TestListBySourceAndByIDsCarryTheBreakdown pins that a row these two hand out
// is a row the engine can fold: a transfer without its pieces folds into a
// position with no acquisition dates and a basis nobody can convert, and
// nothing about the row says it is incomplete.
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
