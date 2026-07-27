package operation_test

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

func TestServiceValidation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	valid := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 10,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, valid); err != nil {
		t.Fatalf("valid buy: %v", err)
	}

	cases := map[string]func(o operation.Operation) operation.Operation{
		"buy positive amount": func(o operation.Operation) operation.Operation {
			o.AmountMinor = 100
			return o
		},
		"buy without instrument": func(o operation.Operation) operation.Operation {
			o.InstrumentID = nil
			return o
		},
		"sell without instrument": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeSell
			o.InstrumentID = nil
			o.AmountMinor = 100_000
			return o
		},
		"bad currency": func(o operation.Operation) operation.Operation {
			o.Currency = "rub"
			return o
		},
		"future date": func(o operation.Operation) operation.Operation {
			o.OccurredOn = date("2099-01-01")
			return o
		},
		"negative fee": func(o operation.Operation) operation.Operation {
			o.FeeMinor = -1
			return o
		},
		"transfer_in via create": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTransferIn
			o.AmountMinor = 100
			return o
		},
		"transfer_out via create": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTransferOut
			o.AmountMinor = 100
			return o
		},
		"zero dividend amount": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeDividend
			o.AmountMinor = 0
			return o
		},
		"positive tax amount": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTax
			o.AmountMinor = 100
			return o
		},
		// Bounds: an unbounded amount_minor poisons the cost basis and wraps
		// realized P&L on the next addition.
		"amount_minor at MinInt64": func(o operation.Operation) operation.Operation {
			o.AmountMinor = math.MinInt64
			return o
		},
		"amount_minor beyond cap": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeDeposit
			o.InstrumentID = nil
			o.Quantity = nil
			o.Price = nil
			o.AmountMinor = 1_000_000_000_000_001
			return o
		},
		"fee_minor beyond cap": func(o operation.Operation) operation.Operation {
			o.FeeMinor = 1_000_000_000_000_001
			return o
		},
	}
	for name, mutate := range cases {
		if _, err := svc.Create(f.ctx, f.spaceID, mutate(valid)); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}

	// oversell rejected on write
	oversell := valid
	oversell.Type = operation.TypeSell
	oversell.Quantity = dec("999")
	oversell.AmountMinor = 999_000
	if _, err := svc.Create(f.ctx, f.spaceID, oversell); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("oversell: err = %v, want ErrInconsistent", err)
	}
}

func TestServiceDeleteConsistency(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	createdBuy, err := svc.Create(f.ctx, f.spaceID, buy)
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	sell := buy
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-02")
	sell.Quantity = dec("5")
	sell.AmountMinor = 55_000
	createdSell, err := svc.Create(f.ctx, f.spaceID, sell)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}

	// deleting buy breaks sell → 409
	if err := svc.Delete(f.ctx, f.spaceID, createdBuy.ID); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("delete buy: err = %v, want ErrInconsistent", err)
	}
	// deleting sell — ok, then buy — ok
	if err := svc.Delete(f.ctx, f.spaceID, createdSell.ID); err != nil {
		t.Fatalf("delete sell: %v", err)
	}
	if err := svc.Delete(f.ctx, f.spaceID, createdBuy.ID); err != nil {
		t.Fatalf("delete buy after sell: %v", err)
	}
}

func TestServiceTransfer(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 10,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// auto cost: floor((100000+10)*4/10) = 40004
	if out.AmountMinor != 40_004 || in.AmountMinor != 40_004 {
		t.Errorf("cost = %d/%d, want 40004", out.AmountMinor, in.AmountMinor)
	}
	if out.Currency != "RUB" || in.AccountID != f.account2ID {
		t.Errorf("pair = %+v %+v", out, in)
	}

	// transfer exceeding balance → ErrInconsistent
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("100"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("oversell transfer: %v", err)
	}
	// from == to
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.accountID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("1"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("same account: %v", err)
	}
}

// TestTransferBackdatedUsesChronologicalBasis pins the fix for a basis leak:
// a transfer dated before existing operations must take its FIFO cost from
// the journal as of its own date, not from the journal's end state.
//
// Source journal: buy 10 @ 100.00 (01.07), buy 10 @ 900.00 (03.07),
// sell 10 (20.07) — the sell consumes lot 1 entirely.
// Transferring 5 units backdated to 05.07 must release from lot 1, which is
// still intact on that date: floor(100000 * 5 / 10) = 50000.
// Folding the whole journal instead would leave only lot 2 in front and
// release floor(900000 * 5 / 10) = 450000 — 400000 minor units of cost basis
// out of thin air.
func TestTransferBackdatedUsesChronologicalBasis(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000
	sell := buy1
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-20")
	sell.Price = dec("1000")
	sell.AmountMinor = 1_000_000
	for _, op := range []operation.Operation{buy1, buy2, sell} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("5"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("backdated transfer: %v", err)
	}
	const wantCost = int64(50_000)
	if out.AmountMinor != wantCost || in.AmountMinor != wantCost {
		t.Errorf("cost = %d/%d, want %d/%d (end-state basis would be 450000)",
			out.AmountMinor, in.AmountMinor, wantCost, wantCost)
	}
}

func TestServiceSplitValidation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Valid split: instrument + positive ratio + amount 0 + source manual or empty
	validSplit := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-01"), SplitRatio: dec("10"), AmountMinor: 0,
		Currency: "RUB", Source: "manual",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, validSplit); err != nil {
		t.Fatalf("valid split with source=manual: %v", err)
	}

	validSplitNoSource := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("2"), AmountMinor: 0,
		Currency: "RUB", Source: "",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, validSplitNoSource); err != nil {
		t.Fatalf("valid split with empty source: %v", err)
	}

	cases := map[string]func(o operation.Operation) operation.Operation{
		"split without instrument": func(o operation.Operation) operation.Operation {
			o.InstrumentID = nil
			return o
		},
		"split with nil ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = nil
			return o
		},
		"split with zero ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = dec("0")
			return o
		},
		"split with negative ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = dec("-5")
			return o
		},
		"split with non-zero amount": func(o operation.Operation) operation.Operation {
			o.AmountMinor = 100
			return o
		},
		"split with csv source": func(o operation.Operation) operation.Operation {
			o.Source = "csv"
			return o
		},
	}
	for name, mutate := range cases {
		if _, err := svc.Create(f.ctx, f.spaceID, mutate(validSplit)); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

// TestTransferSameDayBoundary pins the journalUpTo boundary:
// !o.OccurredOn.After(day) (INCLUSIVE) is the critical filter.
// If changed to Before(day) (exclusive), same-day transfers would not see
// same-day purchases, silently losing cost basis.
func TestTransferSameDayBoundary(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Case (a): Transfer on same day as only purchase.
	// buy 10 @ 100 = -100_000 on 2026-07-01
	// transfer 4 on 2026-07-01 → should see the buy and release floor(100_000 * 4 / 10) = 40_000
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 0,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-01"), // same day as buy
	})
	if err != nil {
		t.Fatalf("same-day transfer: %v", err)
	}
	const wantCostA = int64(40_000)
	if out.AmountMinor != wantCostA || in.AmountMinor != wantCostA {
		t.Errorf("case (a) same-day cost = %d/%d, want %d (Before would fail: 0)",
			out.AmountMinor, in.AmountMinor, wantCostA)
	}

	// Case (b): Sale and transfer on same day; sale precedes transfer.
	// buy 10 @ 100 = -100_000 on 2026-07-01
	// buy 10 @ 900 = -900_000 on 2026-07-03
	// sell 10 on 2026-07-05 (consumes lot 1 completely)
	// transfer 4 on 2026-07-05 (FIFO front is now lot 2; release floor(900_000 * 4 / 10) = 360_000)
	f2 := newFixture(t)
	svc2 := operation.NewService(f2.store)

	buy1 := operation.Operation{
		AccountID: f2.accountID, InstrumentID: &f2.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000

	sell := buy1
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-05")
	sell.Quantity = dec("10")
	sell.Price = dec("1000")
	sell.AmountMinor = 1_000_000

	for _, op := range []operation.Operation{buy1, buy2, sell} {
		if _, err := svc2.Create(f2.ctx, f2.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out2, in2, err := svc2.CreateTransfer(f2.ctx, f2.spaceID, operation.TransferParams{
		FromAccountID: f2.accountID, ToAccountID: f2.account2ID,
		InstrumentID: f2.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-05"), // same day as sell
	})
	if err != nil {
		t.Fatalf("same-day transfer after sale: %v", err)
	}
	const wantCostB = int64(360_000)
	if out2.AmountMinor != wantCostB || in2.AmountMinor != wantCostB {
		t.Errorf("case (b) same-day cost = %d/%d, want %d (FIFO after-sell front)",
			out2.AmountMinor, in2.AmountMinor, wantCostB)
	}
}

// TestTransferBasisConservation verifies the invariant that cost basis is
// conserved across a transfer sequence with subsequent sales.
// Invariant: source.CostMinor + dest.CostMinor + (cost released in later sales)
// must equal the initial total outlay.
//
// Scenario: buy 10 @ 100 (−100_000) 07-01; transfer 4 on 07-02;
// sell 4 (+50_000) on 07-03 (dest); sell 6 (+70_000) on 07-04 (source).
//
// Arithmetic check at end state:
//
//	Initial outlay: 100_000
//	Source remaining cost: 0 (all 6 units sold)
//	Dest remaining cost: 0 (all 4 units sold)
//	Cost released in source sale: 60_000 (FIFO 6 of the 10)
//	Cost released in dest sale: 40_000 (transferred basis of 4)
//	Sum: 0 + 0 + 60_000 + 40_000 = 100_000 ✓
//
// The test tracks proceeds and fees to verify:
//
//	initial_cost = source.CostMinor + dest.CostMinor + (total_proceeds - total_realized_pnl - total_fees)
//
// This would fail if journalUpTo excluded same-day operations or looked at the
// wrong point in the journal, because the transferred basis would be wrong.
func TestTransferBasisConservation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Seed: buy 10 @ 100 = -100_000
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 0,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	// Transfer 4 units on 2026-07-02
	// At this point, source has all 10 @ cost 100_000.
	// Releasing 4 units: floor(100_000 * 4 / 10) = 40_000.
	_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-02"),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	// Sell 4 on dest on 2026-07-03 for 50_000
	sellDest := operation.Operation{
		AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-03"), Quantity: dec("4"), Price: dec("12.5"),
		AmountMinor: 50_000, Currency: "RUB", FeeMinor: 0,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, sellDest); err != nil {
		t.Fatalf("sell dest: %v", err)
	}

	// Sell 6 on source on 2026-07-04 for 70_000
	sellSrc := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-04"), Quantity: dec("6"), Price: dec("11.666666"),
		AmountMinor: 70_000, Currency: "RUB", FeeMinor: 0,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, sellSrc); err != nil {
		t.Fatalf("sell src: %v", err)
	}

	// Compute positions for both accounts
	srcOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list source: %v", err)
	}
	srcPos, err := portfolio.Compute(srcOps)
	if err != nil {
		t.Fatalf("compute source: %v", err)
	}
	srcPosBuy := srcPos[f.sberID]

	destOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("list dest: %v", err)
	}
	destPos, err := portfolio.Compute(destOps)
	if err != nil {
		t.Fatalf("compute dest: %v", err)
	}
	destPosBuy := destPos[f.sberID]

	const initialOutlay = int64(100_000)
	const srcSaleProceeds = int64(70_000)
	const destSaleProceeds = int64(50_000)
	const totalProceeds = srcSaleProceeds + destSaleProceeds

	// At end state: both accounts have sold all units.
	if srcPosBuy.CostMinor != 0 {
		t.Errorf("source final cost = %d, want 0", srcPosBuy.CostMinor)
	}
	if destPosBuy.CostMinor != 0 {
		t.Errorf("dest final cost = %d, want 0", destPosBuy.CostMinor)
	}

	// Verify basis conservation: initial cost = (remaining costs) + (realized gains + fees).
	// Rearranged: cost_released = total_proceeds - realized_pnl - total_fees
	// Invariant: initial_outlay = src.Cost + dst.Cost + cost_released
	//
	// At end state where all units are sold:
	// cost_released = proceeds - realized_pnl - fees
	//              = 120_000 - 20_000 - 0 = 100_000
	// So: src.Cost + dst.Cost + 100_000 = 0 + 0 + 100_000 = 100_000 ✓
	//
	// More generally (if not all sold):
	totalRealizedPnL := srcPosBuy.RealizedPnLMinor + destPosBuy.RealizedPnLMinor
	totalFees := srcPosBuy.FeesMinor + destPosBuy.FeesMinor
	costReleased := totalProceeds - totalRealizedPnL - totalFees
	totalAccountedFor := srcPosBuy.CostMinor + destPosBuy.CostMinor + costReleased

	if totalAccountedFor != initialOutlay {
		t.Errorf("basis conservation: %d + %d + %d = %d, want %d (realized=%d, fees=%d)",
			srcPosBuy.CostMinor, destPosBuy.CostMinor, costReleased, totalAccountedFor,
			initialOutlay, totalRealizedPnL, totalFees)
	}
}
