package portfolio_test

import (
	"errors"
	"testing"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

func TestSplitAdjustsQuantity(t *testing.T) {
	split := op(operation.TypeSplit, 5, &sber, "", "", 0, 0)
	split.SplitRatio = dp("10")
	ops := []operation.Operation{
		op(operation.TypeBuy, 1, &sber, "10", "3000", -3_000_000, 0),
		split,
		// after a 1:10 split, sell 50 of 100; cost released = 3000000/2
		op(operation.TypeSell, 6, &sber, "50", "310", 1_550_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if !p.Quantity.Equal(d("50")) {
		t.Errorf("qty = %s, want 50", p.Quantity)
	}
	if p.CostMinor != 1_500_000 {
		t.Errorf("cost = %d, want 1500000", p.CostMinor)
	}
	if p.RealizedPnLMinor != 1_550_000-1_500_000 {
		t.Errorf("realized = %d", p.RealizedPnLMinor)
	}
}

func TestTransferOutInCarryover(t *testing.T) {
	// Source: 10 x 100.00 (cost 100000). Transfer out 4: released = 40000.
	outOps := []operation.Operation{
		op(operation.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(operation.TypeTransferOut, 5, &sber, "4", "", 40_000, 0),
	}
	pos, err := portfolio.Compute(outOps)
	if err != nil {
		t.Fatalf("Compute out: %v", err)
	}
	p := pos[sber]
	if !p.Quantity.Equal(d("6")) || p.CostMinor != 60_000 || p.RealizedPnLMinor != 0 {
		t.Fatalf("source pos = %+v", p)
	}

	// Destination: transfer_in with the carried cost basis, then a profitable sell.
	inOps := []operation.Operation{
		op(operation.TypeTransferIn, 5, &sber, "4", "", 40_000, 0),
		op(operation.TypeSell, 6, &sber, "4", "120", 48_000, 0),
	}
	pos, err = portfolio.Compute(inOps)
	if err != nil {
		t.Fatalf("Compute in: %v", err)
	}
	if pos[sber].RealizedPnLMinor != 8_000 {
		t.Errorf("dest realized = %d, want 8000", pos[sber].RealizedPnLMinor)
	}
}

func TestTransferOutOversell(t *testing.T) {
	ops := []operation.Operation{
		op(operation.TypeBuy, 1, &sber, "3", "100", -30_000, 0),
		op(operation.TypeTransferOut, 2, &sber, "5", "", 0, 0),
	}
	if _, err := portfolio.Compute(ops); !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell", err)
	}
}

func TestReleasedCostHelper(t *testing.T) {
	ops := []operation.Operation{
		op(operation.TypeBuy, 1, &sber, "10", "100", -100_000, 5),
		op(operation.TypeBuy, 2, &sber, "10", "200", -200_000, 5),
	}
	// 15 units: all of lot 1 (100005) + half of lot 2 (floor(200005/2)=100002)
	cost, err := portfolio.ReleasedCost(ops, sber, d("15"))
	if err != nil {
		t.Fatalf("ReleasedCost: %v", err)
	}
	if cost != 100_005+100_002 {
		t.Errorf("cost = %d", cost)
	}
	if _, err := portfolio.ReleasedCost(ops, sber, d("25")); !errors.Is(err, portfolio.ErrOversell) {
		t.Errorf("oversell err = %v", err)
	}
}
