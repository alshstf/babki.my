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
