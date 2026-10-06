package operation_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// A move records the lots it took by number, and the number of an imported
// purchase is its broker record: a correction re-inserts the row under a new id,
// and the move still finds its lot.
func TestAMoveKeepsItsLotThroughAnImportCorrection(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	record := "buy-1"
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: -100_000, Currency: "RUB",
		Source: "tinvest", ExternalID: &record,
	}
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{buy}})
	if err != nil || len(applied) != 1 || len(refused) != 0 {
		t.Fatalf("import the purchase: %d applied, %+v refused, %v", len(applied), refused, err)
	}

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.NewFromInt(4), OccurredOn: date("2026-07-10"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := portfolio.LotID{Origin: "tinvest/buy-1"}
	if len(in.TransferLots) != 1 || in.TransferLots[0].From != want {
		t.Fatalf("the move's pieces = %+v, want one naming %s", in.TransferLots, want)
	}

	corrected := buy
	corrected.Note = "переписано брокером"
	corrected.CreatedAt = applied[0].CreatedAt
	if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{applied[0].ID}, Add: []operation.Operation{corrected},
	}); err != nil || len(refused) != 0 {
		t.Fatalf("correct the purchase: %+v refused, %v", refused, err)
	}

	for account, want := range map[uuid.UUID][2]int64{f.accountID: {6, 60_000}, f.account2ID: {4, 40_000}} {
		journal, err := f.store.ListForEngine(f.ctx, f.spaceID, account)
		if err != nil {
			t.Fatal(err)
		}
		pos, err := portfolio.Compute(journal)
		if err != nil {
			t.Fatalf("the journal no longer replays after the correction: %v", err)
		}
		if p := pos[f.sberID]; !p.Quantity.Equal(decimal.NewFromInt(want[0])) || p.CostMinor != want[1] {
			t.Errorf("account holds %s for %d, want %d for %d", p.Quantity, p.CostMinor, want[0], want[1])
		}
	}
}

// A move across two purchases of one day names each by its own id.
func TestAMoveNamesEachPurchaseItTakes(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	var ids []uuid.UUID
	for _, amount := range []int64{-100_000, -900_000} {
		op, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: amount, Currency: "RUB",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, op.ID)
	}
	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.NewFromInt(15), OccurredOn: date("2026-07-10"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.TransferLots) != 2 || in.TransferLots[0].From.Origin != "op/"+ids[0].String() ||
		in.TransferLots[1].From.Origin != "op/"+ids[1].String() || in.TransferLots[1].CostMinor != 450_000 {
		t.Errorf("pieces = %+v, want ten of the first purchase and five of the second for 4 500 ₽", in.TransferLots)
	}
}

// A move recorded before lots had numbers gets them back from the day
// matching; one whose piece spans two lots of its day is left as it was, and
// both still replay.
func TestMovesRecordedBeforeNumbersAreNumbered(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	for _, amount := range []int64{-100_000, -900_000} {
		if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: amount, Currency: "RUB",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.NewFromInt(15), OccurredOn: date("2026-07-10"),
	})
	if err != nil {
		t.Fatal(err)
	}
	numberedPieces := in.TransferLots
	if _, err := f.pool.Exec(f.ctx, `UPDATE operation_transfer_lots SET from_origin = NULL, from_seq = NULL`); err != nil {
		t.Fatal(err)
	}

	numbered, left, err := svc.NumberLegacyMoves(f.ctx)
	if err != nil || numbered != 1 || left != 0 {
		t.Fatalf("numbered %d, left %d, err %v; want the one move numbered", numbered, left, err)
	}
	stored := f.pieces(t, in.ID)
	for i := range numberedPieces {
		if stored[i].From != numberedPieces[i].From {
			t.Errorf("piece %d names %s, want %s as when it was written", i, stored[i].From, numberedPieces[i].From)
		}
	}

	// One piece of fifteen dated the purchases' day: the day matching took it
	// across both lots, so no single number is true of it.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM operation_transfer_lots WHERE operation_id = $1`, in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO operation_transfer_lots (operation_id, seq, quantity, cost_minor, acquired_on)
		VALUES ($1, 0, 15, 550000, '2026-07-01')`, in.ID); err != nil {
		t.Fatal(err)
	}
	if numbered, left, err := svc.NumberLegacyMoves(f.ctx); err != nil || numbered != 0 || left != 1 {
		t.Errorf("numbered %d, left %d, err %v; want the spanning move left as it was", numbered, left, err)
	}
	if f.pieces(t, in.ID)[0].From != (portfolio.LotID{}) {
		t.Error("the spanning piece was given a number")
	}
}

// pieces is the breakdown stored for an arriving leg, as a reader gets it.
func (f fixture) pieces(t *testing.T, inID uuid.UUID) []operation.ReleasedLot {
	t.Helper()
	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range journal {
		if o.ID == inID {
			return o.TransferLots
		}
	}
	t.Fatalf("no operation %s on the receiving account", inID)
	return nil
}
