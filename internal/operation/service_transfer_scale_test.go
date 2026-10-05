package operation_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// storageScale is the journal's quantity scale (NUMERIC(30,10)), named here
// rather than taken from the service.
const storageScale = int32(10)

// positionsOf replays an account the way GET /accounts/{id}/positions does
// and fails with the error that endpoint would answer 422 with.
func positionsOf(t *testing.T, f fixture, accountID uuid.UUID) map[uuid.UUID]*portfolio.Position {
	t.Helper()
	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	positions, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("positions of %s are unreadable (this is the 422 the account's whole positions screen answers with): %v", accountID, err)
	}
	return positions
}

// transferInOf returns the transfer_in recorded on an account.
func transferInOf(t *testing.T, f fixture, accountID uuid.UUID) operation.Operation {
	t.Helper()
	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	in := findByType(ops, operation.TypeTransferIn)
	if in == nil {
		t.Fatalf("no transfer_in recorded on account %s", accountID)
	}
	return *in
}

// A transfer after a reverse split leaves the receiving account readable.
//
//	buy 3.5 SBER on 01.07 ; buy 3.5 SBER on 02.07
//	reverse split 1:3 on 03.07 -> two lots of 1.16666666655, total 2.3333333331
//	transfer all 2.3333333331 on 05.07
//
// Each piece used to be stored and rounded on its own, up to 1.1666666666, so the
// stored breakdown summed to 2.3333333332 against an operation of 2.3333333331
// and the account answered 422 for good. quantizeLots now stores 1.1666666665 +
// 1.1666666666, the remainder on the last piece.
func TestTransferAfterReverseSplitStaysReadable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("3.5"), Price: dec("100"),
		AmountMinor: -35_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-02")
	buy2.Price = dec("200")
	buy2.AmountMinor = -70_000
	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-03"), SplitRatio: dec("0.3333333333"),
		AmountMinor: 0, Currency: "RUB",
	}
	for _, op := range []operation.Operation{buy1, buy2} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}
	seedSplit(t, f, svc, split)

	// What the source holds after the split, moved in full.
	moved := decimal.RequireFromString("2.3333333331")
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.Equal(moved) {
		t.Fatalf("source quantity after the split = %s, want %s", src.Quantity, moved)
	}

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: moved, OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer of a split position: %v", err)
	}

	// The receiving account's positions must read.
	dest := positionsOf(t, f, f.account2ID)[f.sberID]
	if dest == nil {
		t.Fatalf("no position on the receiving account")
	}
	if !dest.Quantity.Equal(moved) {
		t.Errorf("received quantity = %s, want %s", dest.Quantity, moved)
	}
	if dest.CostMinor != 105_000 {
		t.Errorf("received basis = %d, want 105000 (35000 + 70000, unchanged by the move)", dest.CostMinor)
	}

	// Every stored piece is representable and the remainder went to the
	// last one.
	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("1.1666666665"), CostMinor: 35_000, AcquiredOn: datep("2026-07-01")},
		{Quantity: decimal.RequireFromString("1.1666666666"), CostMinor: 70_000, AcquiredOn: datep("2026-07-02")},
	}
	stored := transferInOf(t, f, f.account2ID).TransferLots
	if len(stored) != len(want) {
		t.Fatalf("stored pieces = %+v, want %d", stored, len(want))
	}
	sum := decimal.Zero
	for i, w := range want {
		g := stored[i]
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
			t.Errorf("stored piece %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
		if g.Quantity.Exponent() < -storageScale {
			t.Errorf("stored piece %d has quantity %s, finer than the %d decimal places the column keeps",
				i, g.Quantity, storageScale)
		}
		sum = sum.Add(g.Quantity)
	}
	if !sum.Equal(moved) {
		t.Errorf("stored pieces sum to %s, but the operation moves %s — this is the mismatch that broke the screen", sum, moved)
	}

	// The 201 describes the stored rows.
	if len(in.TransferLots) != len(stored) {
		t.Fatalf("returned pieces = %+v, stored = %+v", in.TransferLots, stored)
	}
	for i := range stored {
		if !in.TransferLots[i].Quantity.Equal(stored[i].Quantity) {
			t.Errorf("returned piece %d quantity = %s, but %s is what is stored",
				i, in.TransferLots[i].Quantity, stored[i].Quantity)
		}
	}

	// And the source is emptied exactly, with no dust left behind.
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.IsZero() {
		t.Errorf("source quantity after moving everything = %s, want 0", src.Quantity)
	}
}

// The operation's own quantity is truncated to the scale too.
//
//	buy 1 SBER (cost 100) ; buy 5 SBER (cost 500)
//	transfer 1.00000000004
//
// At full precision that is lot 1 plus 4e-11 of lot 2, a piece the column rounds
// to zero and its CHECK refuses. Truncated down (nearest could turn "move
// everything" into an oversell): one unit moves, five stay.
func TestTransferQuantityFinerThanStorageIsTruncated(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1"), Price: dec("1"),
		AmountMinor: -100, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-02")
	buy2.Quantity = dec("5")
	buy2.AmountMinor = -500
	for _, op := range []operation.Operation{buy1, buy2} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("1.00000000004"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer of a quantity finer than the column: %v", err)
	}

	one := decimal.RequireFromString("1")
	for _, leg := range []struct {
		what string
		op   operation.Operation
	}{{"transfer_out", out}, {"transfer_in", in}} {
		if leg.op.Quantity == nil || !leg.op.Quantity.Equal(one) {
			t.Errorf("%s quantity = %v, want 1 (truncated to the scale, never rounded up)", leg.what, leg.op.Quantity)
		}
	}

	stored := transferInOf(t, f, f.account2ID)
	if len(stored.TransferLots) != 1 {
		t.Fatalf("stored pieces = %+v, want exactly one (the 4e-11 tail is not a piece)", stored.TransferLots)
	}
	if pc := stored.TransferLots[0]; !pc.Quantity.Equal(one) || pc.CostMinor != 100 || !sameAcquisition(pc.AcquiredOn, datep("2026-07-01")) {
		t.Errorf("stored piece = %s/%d/%s, want 1/100/2026-07-01",
			pc.Quantity, pc.CostMinor, acquired(pc.AcquiredOn))
	}

	if dest := positionsOf(t, f, f.account2ID)[f.sberID]; !dest.Quantity.Equal(one) || dest.CostMinor != 100 {
		t.Errorf("received position = %s/%d, want 1/100", dest.Quantity, dest.CostMinor)
	}
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.Equal(decimal.RequireFromString("5")) {
		t.Errorf("source keeps %s, want 5 (one whole unit left, nothing shaved off the rest)", src.Quantity)
	}

	// A quantity that is all tail is refused, not stored as zero.
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("0.00000000004"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("transfer of 4e-11 units: err = %v, want ErrValidation", err)
	}
}

// A lot with no shares left (a reverse split rounded them away) travels as a
// piece of its own: no units, its cost, its own day.
//
//	buy 0.4 SBER on 01.07 for 4,00 ; reverse split by 0.0000000001 on 02.07
//	buy 5 SBER on 03.07 for 500,00 ; transfer everything on 05.07
//
// Its 400 used to be folded into the next piece and dated 03.07 (#193).
func TestATransferKeepsAShareLessParcelsMoneyOnItsOwnDay(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	dust := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.4"), Price: dec("10"),
		AmountMinor: -400, Currency: "RUB",
	}
	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("0.0000000001"),
		AmountMinor: 0, Currency: "RUB",
	}
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-03"), Quantity: dec("5"), Price: dec("100"),
		AmountMinor: -50_000, Currency: "RUB",
	}
	for _, op := range []operation.Operation{dust, buy} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}
	seedSplit(t, f, svc, split)

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("5.00000000004"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer including a dust lot: %v", err)
	}

	stored := transferInOf(t, f, f.account2ID).TransferLots
	if len(stored) != 2 {
		t.Fatalf("stored pieces = %+v, want two: the shareless parcel and the shares", stored)
	}
	const wantCost = int64(50_000 + 400)
	if pc := stored[0]; !pc.Quantity.IsZero() || pc.CostMinor != 400 ||
		pc.AcquiredOn == nil || !pc.AcquiredOn.Equal(date("2026-07-01")) {
		t.Errorf("shareless piece = %+v, want no units and 400 acquired 2026-07-01", pc)
	}
	if pc := stored[1]; !pc.Quantity.Equal(decimal.RequireFromString("5")) || pc.CostMinor != 50_000 ||
		pc.AcquiredOn == nil || !pc.AcquiredOn.Equal(date("2026-07-03")) {
		t.Errorf("second piece = %+v, want 5 units and 50000 acquired 2026-07-03", pc)
	}
	if in.AmountMinor != wantCost {
		t.Errorf("carried basis = %d, want %d — the operation's amount must be the sum of the pieces actually written",
			in.AmountMinor, wantCost)
	}
	if dest := positionsOf(t, f, f.account2ID)[f.sberID]; dest.CostMinor != wantCost || len(dest.Lots) != 2 {
		t.Errorf("received basis = %d over %d parcels, want %d over 2", dest.CostMinor, len(dest.Lots), wantCost)
	}
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.IsZero() || src.CostMinor != 0 || len(src.Lots) != 0 {
		t.Errorf("source after moving everything = %s/%d, want 0/0 — no cost may be left behind attached to no shares",
			src.Quantity, src.CostMinor)
	}
}
