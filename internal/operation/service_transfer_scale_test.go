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

// storageScale is how many decimal places the journal keeps for a quantity
// (operations.quantity and operation_transfer_lots.quantity are both
// NUMERIC(30,10)). The tests below assert against the storage contract itself,
// so they name it rather than reaching for the service's unexported constant.
const storageScale = int32(10)

// positionsOf replays an account's journal exactly the way GET
// /accounts/{id}/positions does — store.ListForEngine, then portfolio.Compute
// — and fails the test with the error that endpoint would answer 422 with.
// This is the check that matters for a transfer: a breakdown the engine
// refuses does not spoil one instrument's row, it takes the account's whole
// positions screen down.
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

// transferInOf returns the transfer_in recorded on an account, read back the
// way the rest of the system consumes the journal.
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

// TestTransferAfterReverseSplitStaysReadable is the reviewer's reproduction,
// end to end through the real service, of the branch's worst bug: a
// legitimate transfer that permanently broke the receiving account's
// positions screen.
//
//	buy 3.5 SBER on 01.07 for 350,00 ; buy 3.5 SBER on 02.07 for 700,00
//	reverse split 1:3 on 03.07 (ratio 0.3333333333, the natural way to record it)
//	  → two lots of 1.16666666655, a position of 2.3333333331
//	transfer all 2.3333333331 on 05.07
//
// Lot quantities are not bound to ten decimal places — a split multiplies
// them by a ratio — but the columns that store them are. Each piece was
// written on its own and rounded on its own: both 1.16666666655 pieces rounded
// UP to 1.1666666666, so the stored breakdown summed to 2.3333333332 while the
// stored operation said it moved 2.3333333331. The write path checked the
// exact pieces and was happy (201 Created); every later read checked the
// stored ones and was not, so GET .../positions answered
//
//	422: transfer lots sum to quantity 2.3333333332, but the operation moves 2.3333333331
//
// for that account, forever, for every instrument in it.
//
// The pieces are now brought onto the storage scale as the breakdown is built,
// the remainder going to the last piece (operation.quantizeLots), so what is
// written is exactly what is read back: 1.1666666665 + 1.1666666666, summing
// to the 2.3333333331 that moved. The 1e-10 the first piece gives up shows up
// on the last one — the same "floor the share, the last piece takes the
// remainder" rule releaseFIFO has always applied to costs.
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
	for _, op := range []operation.Operation{buy1, buy2, split} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	// What the source holds after the split, and therefore what the transfer
	// below moves in full: two lots of 3.5 × 0.3333333333.
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

	// The receiving account's positions must READ — the whole point. Before the
	// fix this call is where the reproduction ends, with the 422 above.
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

	// Every stored piece is representable, and the pieces still describe the
	// same two purchases: the remainder went to the last piece, not into thin
	// air.
	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("1.1666666665"), CostMinor: 35_000, AcquiredOn: date("2026-07-01")},
		{Quantity: decimal.RequireFromString("1.1666666666"), CostMinor: 70_000, AcquiredOn: date("2026-07-02")},
	}
	stored := transferInOf(t, f, f.account2ID).TransferLots
	if len(stored) != len(want) {
		t.Fatalf("stored pieces = %+v, want %d", stored, len(want))
	}
	sum := decimal.Zero
	for i, w := range want {
		g := stored[i]
		if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !g.AcquiredOn.Equal(w.AcquiredOn) {
			t.Errorf("stored piece %d = %s/%d/%s, want %s/%d/%s", i,
				g.Quantity, g.CostMinor, g.AcquiredOn.Format("2006-01-02"),
				w.Quantity, w.CostMinor, w.AcquiredOn.Format("2006-01-02"))
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

	// The 201 must describe the rows that exist, not the ones the service had
	// in hand: before the fix these differed in the tenth decimal place, so the
	// API answered with a breakdown that was nowhere in the database.
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

// TestTransferQuantityFinerThanStorageIsTruncated covers the other half of the
// same fault, on the operation's own quantity rather than the pieces'.
// operations.quantity keeps ten decimal places too, and nothing stopped a
// request from asking to move more of them.
//
//	buy 1 SBER on 01.07 (cost 100) ; buy 5 SBER on 02.07 (cost 500)
//	transfer 1.00000000004 on 05.07
//
// Released at full precision that is lot 1 whole plus 4e-11 of lot 2 — a piece
// the quantity column cannot hold at all: it rounds to zero and the table's
// CHECK (quantity > 0) rejects the write, so a perfectly ordinary transfer
// dies with a server error. The quantity is instead brought onto the scale
// first, and DOWNWARD: rounding to nearest could round a "move everything I
// hold" up past the position it is emptying and answer it with an oversell,
// which is a loud refusal of healthy data — the thing this branch must not do.
// One unit moves, five stay, and the 4e-11 stays with them.
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
	if pc := stored.TransferLots[0]; !pc.Quantity.Equal(one) || pc.CostMinor != 100 || !pc.AcquiredOn.Equal(date("2026-07-01")) {
		t.Errorf("stored piece = %s/%d/%s, want 1/100/2026-07-01",
			pc.Quantity, pc.CostMinor, pc.AcquiredOn.Format("2006-01-02"))
	}

	if dest := positionsOf(t, f, f.account2ID)[f.sberID]; !dest.Quantity.Equal(one) || dest.CostMinor != 100 {
		t.Errorf("received position = %s/%d, want 1/100", dest.Quantity, dest.CostMinor)
	}
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.Equal(decimal.RequireFromString("5")) {
		t.Errorf("source keeps %s, want 5 (one whole unit left, nothing shaved off the rest)", src.Quantity)
	}

	// A quantity that is nothing BUT tail cannot be recorded at all, and is
	// refused as the input error it is rather than silently becoming zero.
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("0.00000000004"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("transfer of 4e-11 units: err = %v, want ErrValidation", err)
	}
}

// TestTransferDropsAPieceTooSmallToStoreButKeepsItsCost pins what happens to a
// piece whose quantity is finer than the scale even after the running total is
// accounted for — a dust lot left by a deep reverse split.
//
//	buy 0.4 SBER on 01.07 for 4,00 ; reverse split by 0.0000000001 on 02.07
//	  → that lot is now 4e-11 units, still holding its 400 minor units of cost
//	buy 5 SBER on 03.07 for 500,00 (after the split, so untouched by it)
//	transfer everything on 05.07
//
// The dust lot cannot be stored as a piece: rounded to the scale it is zero,
// and a zero-quantity lot is neither storable (CHECK quantity > 0) nor a thing
// that exists. It is dropped — but its COST is real money, so it is carried
// onto the next piece that survives. Nothing is invented and nothing
// evaporates: the basis that arrives is still every minor unit that left.
func TestTransferDropsAPieceTooSmallToStoreButKeepsItsCost(t *testing.T) {
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
	for _, op := range []operation.Operation{dust, split, buy} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("5.00000000004"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer including a dust lot: %v", err)
	}

	stored := transferInOf(t, f, f.account2ID).TransferLots
	if len(stored) != 1 {
		t.Fatalf("stored pieces = %+v, want one (the dust piece is not storable)", stored)
	}
	// 49 999 of the 50 000 lot is released (the last 4e-11 units stay behind
	// with 1 minor unit of cost), plus the dust lot's whole 400.
	const wantCost = int64(49_999 + 400)
	if pc := stored[0]; !pc.Quantity.Equal(decimal.RequireFromString("5")) || pc.CostMinor != wantCost {
		t.Errorf("stored piece = %s/%d, want 5/%d (the dropped piece's cost rides along)",
			pc.Quantity, pc.CostMinor, wantCost)
	}
	if in.AmountMinor != wantCost {
		t.Errorf("carried basis = %d, want %d — the operation's amount must be the sum of the pieces actually written",
			in.AmountMinor, wantCost)
	}
	if dest := positionsOf(t, f, f.account2ID)[f.sberID]; dest.CostMinor != wantCost {
		t.Errorf("received basis = %d, want %d", dest.CostMinor, wantCost)
	}
}
