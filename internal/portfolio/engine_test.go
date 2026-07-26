package portfolio_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/portfolio"
)

var (
	sber = uuid.New()
	lkoh = uuid.New()
	ofz  = uuid.New()
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }
func dp(s string) *decimal.Decimal {
	v := d(s)
	return &v
}

func day(n int) time.Time {
	return time.Date(2026, 7, n, 0, 0, 0, 0, time.UTC)
}

func op(typ portfolio.Type, dayN int, inst *uuid.UUID, qty, price string, amount, fee int64) portfolio.Operation {
	o := portfolio.Operation{
		Type: typ, OccurredOn: day(dayN), AmountMinor: amount,
		Currency: "RUB", FeeMinor: fee, InstrumentID: inst,
	}
	if qty != "" {
		o.Quantity = dp(qty)
	}
	if price != "" {
		o.Price = dp(price)
	}
	return o
}

func TestBuySellFIFO(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeDeposit, 1, nil, "", "", 1_000_000, 0),
		// 10 × 100.00 + fee 10
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		// 10 × 110.00 + fee 11
		op(portfolio.TypeBuy, 3, &sber, "10", "110", -110_000, 11),
		// sell 15 × 120.00, fee 18: released = lot1 fully (100010) + 5/10 of lot2 (floor(110011*0.5)=55005)
		op(portfolio.TypeSell, 4, &sber, "15", "120", 180_000, 18),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if p == nil {
		t.Fatal("no SBER position")
	}
	if !p.Quantity.Equal(d("5")) {
		t.Errorf("qty = %s, want 5", p.Quantity)
	}
	wantReleased := int64(100_010 + 55_005)
	wantRealized := 180_000 - wantReleased - 18
	if p.RealizedPnLMinor != wantRealized {
		t.Errorf("realized = %d, want %d", p.RealizedPnLMinor, wantRealized)
	}
	// remaining cost = full cost of both lots − released (not a cent of drift)
	if p.CostMinor != (100_010+110_011)-wantReleased {
		t.Errorf("cost = %d", p.CostMinor)
	}
	if p.FeesMinor != 10+11+18 {
		t.Errorf("fees = %d", p.FeesMinor)
	}
}

func TestLotDrainNoRoundingDrift(t *testing.T) {
	// Lot of 3 shares at 100.00 (cost 30000): sells of 1+1+1 — released sums to exactly 30000.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "100", -30_000, 0),
	}
	// lot cost = 30000; equal thirds of 10000 — no drift, remainder 0
	for i := 0; i < 3; i++ {
		ops = append(ops, op(portfolio.TypeSell, 2+i, &sber, "1", "100", 10_000, 0))
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if !p.Quantity.IsZero() || p.CostMinor != 0 {
		t.Errorf("qty=%s cost=%d, want 0/0", p.Quantity, p.CostMinor)
	}
	if p.RealizedPnLMinor != 0 {
		t.Errorf("realized = %d, want 0", p.RealizedPnLMinor)
	}
}

func TestDriftRemainderGoesToLastPiece(t *testing.T) {
	// Lot of 3 at 100.01 (cost 10001; fee 0) and three sells of 1 each.
	// Step by step: floor(10001*1/3)=3333 (lot: cost 6668, qty 2);
	// floor(6668*1/2)=3334 (lot: cost 3334, qty 1); the last piece
	// takes the lot's remaining cost 3334. Sum 3333+3334+3334 = 10001 — exact.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -10_001, 0),
		op(portfolio.TypeSell, 2, &sber, "1", "", 4_000, 0),
		op(portfolio.TypeSell, 3, &sber, "1", "", 4_000, 0),
		op(portfolio.TypeSell, 4, &sber, "1", "", 4_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if p.CostMinor != 0 {
		t.Errorf("cost = %d, want 0", p.CostMinor)
	}
	if p.RealizedPnLMinor != 12_000-10_001 {
		t.Errorf("realized = %d, want %d", p.RealizedPnLMinor, 12_000-10_001)
	}
}

func TestOversellRejected(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeSell, 2, &sber, "11", "100", 110_000, 0),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell", err)
	}
	// Verify instrument ID is included in error message
	if !strings.Contains(err.Error(), sber.String()) {
		t.Errorf("error message missing instrument ID: %v", err)
	}
}

func TestConversionWithInstrumentNoGhost(t *testing.T) {
	// A conversion operation with instrument_id should not create a ghost position
	ops := []portfolio.Operation{
		// Create a conversion op with instrument — it's cash-level and should be ignored,
		// not creating an empty position in the result map
		op(portfolio.TypeConversion, 1, &sber, "", "", 0, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(pos) != 0 {
		t.Errorf("positions = %d, want 0 (no ghost position)", len(pos))
	}
}

func TestIncomeAndTaxes(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeDividend, 5, &sber, "", "", 3_480, 0),
		op(portfolio.TypeTax, 5, &sber, "", "", -452, 0),
		// dividend/tax without instrument — cash-level, ignored
		op(portfolio.TypeInterest, 6, nil, "", "", 1_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[sber].IncomeMinor != 3_480-452 {
		t.Errorf("income = %d", pos[sber].IncomeMinor)
	}
	if len(pos) != 1 {
		t.Errorf("positions = %d, want 1", len(pos))
	}
}

func TestAmortizationReducesCost(t *testing.T) {
	// Bond: 10 units at 950.00 (cost 950000). Amortization 250 per unit → 2500.00 total.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 250_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[ofz].CostMinor != 700_000 {
		t.Errorf("cost = %d, want 700000", pos[ofz].CostMinor)
	}
	// amortization beyond remaining cost basis goes to Realized
	ops = append(ops, op(portfolio.TypeAmortization, 11, &ofz, "", "", 800_000, 0))
	pos, err = portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute 2: %v", err)
	}
	if pos[ofz].CostMinor != 0 || pos[ofz].RealizedPnLMinor != 100_000 {
		t.Errorf("cost=%d realized=%d, want 0/100000", pos[ofz].CostMinor, pos[ofz].RealizedPnLMinor)
	}
}

func TestClosedPositionKeptInResult(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &lkoh, "5", "7000", -3_500_000, 0),
		op(portfolio.TypeSell, 2, &lkoh, "5", "7500", 3_750_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[lkoh]
	if p == nil || !p.Quantity.IsZero() || p.RealizedPnLMinor != 250_000 {
		t.Fatalf("closed position = %+v", p)
	}
}

func TestBadOperations(t *testing.T) {
	for name, bad := range map[string]portfolio.Operation{
		"buy without qty":      op(portfolio.TypeBuy, 1, &sber, "", "100", -1000, 0),
		"buy negative qty":     op(portfolio.TypeBuy, 1, &sber, "-1", "100", -1000, 0),
		"sell without inst":    op(portfolio.TypeSell, 1, nil, "1", "100", 1000, 0),
		"buy positive amount":  op(portfolio.TypeBuy, 1, &sber, "1", "100", 1000, 0),
		"sell negative amount": op(portfolio.TypeSell, 1, &sber, "1", "100", -1000, 0),
	} {
		if _, err := portfolio.Compute([]portfolio.Operation{bad}); !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want ErrBadOperation", name, err)
		}
	}
}
