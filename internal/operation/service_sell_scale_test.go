package operation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// sellEverything sells exactly what the positions screen says is held. The
// quantity comes from the screen because the fault was that this number could
// not be written into the journal.
func sellEverything(t *testing.T, f fixture, svc *operation.Service, accountID uuid.UUID, on string) decimal.Decimal {
	t.Helper()
	held := positionsOf(t, f, accountID)[f.sberID].Quantity
	sell := operation.Operation{
		AccountID: accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date(on), Quantity: &held, AmountMinor: 1_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, sell); err != nil {
		t.Fatalf("selling the whole position of %s: %v", held, err)
	}
	return held
}

// storedSell returns the quantity the journal holds for the account's only
// sell.
func storedSell(t *testing.T, f fixture, accountID uuid.UUID) decimal.Decimal {
	t.Helper()
	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	sell := findByType(ops, operation.TypeSell)
	if sell == nil || sell.Quantity == nil {
		t.Fatalf("no sell recorded on account %s", accountID)
	}
	return *sell.Quantity
}

// Selling everything after a reverse split stays readable.
//
//	buy 0.35 SBER on 01.07 ; reverse split 1:3 on 02.07 ; sell everything on 03.07
//
// 0.35 × 0.3333333333 = 0.116666666655, but the journal keeps ten places. The
// sell was checked at that figure and stored rounded up to 0.1166666667, so every
// later read answered 422 "not enough quantity". Now the split's result is on the
// scale (Position.applySplit) and the request is normalized before the check
// (normalizeForStorage).
func TestSellingEverythingAfterASplitStaysReadable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.35"), Price: dec("100"),
		AmountMinor: -3_500, Currency: "RUB",
	}
	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("0.3333333333"),
		AmountMinor: 0, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("seed the purchase: %v", err)
	}
	seedSplit(t, f, svc, split)

	held := positionsOf(t, f, f.accountID)[f.sberID].Quantity
	if want := decimal.RequireFromString("0.1166666666"); !held.Equal(want) {
		// Reported, not fatal, so the rest still runs.
		t.Errorf("position after the split = %s, want %s (the journal cannot name 0.116666666655, so neither may a position)", held, want)
	}

	sold := sellEverything(t, f, svc, f.accountID, "2026-07-03")

	// What the row says is what the request said.
	if stored := storedSell(t, f, f.accountID); !stored.Equal(sold) {
		t.Errorf("the sell was accepted for %s and recorded as %s — every later read compares against the recorded one", sold, stored)
	}

	// The reproduction ended here, with the 422 above.
	after := positionsOf(t, f, f.accountID)[f.sberID]
	if after == nil {
		t.Fatalf("no position after selling everything")
	}
	if !after.Quantity.IsZero() {
		t.Errorf("quantity after selling everything = %s, want 0 — a position nobody can close is the other half of this bug", after.Quantity)
	}
	if after.CostMinor != 0 {
		t.Errorf("cost after selling everything = %d, want 0", after.CostMinor)
	}
}

// Selling what a transfer left behind stays readable.
//
//	buy 0.35 SBER ; reverse split 1:3 ; move 0.05 away ; sell the rest
//
// The remainder used to be 0.066666666655, unsellable dust; with the split on the
// scale every remainder is too, and the source closes at zero.
func TestSellingWhatATransferLeftBehindStaysReadable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.35"), Price: dec("100"),
		AmountMinor: -3_500, Currency: "RUB",
	}
	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("0.3333333333"),
		AmountMinor: 0, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("seed the purchase: %v", err)
	}
	seedSplit(t, f, svc, split)

	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("0.05"),
		OccurredOn: date("2026-07-03"),
	}); err != nil {
		t.Fatalf("moving part of the position away: %v", err)
	}

	left := positionsOf(t, f, f.accountID)[f.sberID].Quantity
	if want := decimal.RequireFromString("0.0666666666"); !left.Equal(want) {
		t.Errorf("remainder after the transfer = %s, want %s — what a transfer leaves must be as recordable as what it moved", left, want)
	}

	sold := sellEverything(t, f, svc, f.accountID, "2026-07-04")
	if stored := storedSell(t, f, f.accountID); !stored.Equal(sold) {
		t.Errorf("the sell was accepted for %s and recorded as %s", sold, stored)
	}

	// Both accounts' screens must read.
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.IsZero() {
		t.Errorf("source quantity after selling the remainder = %s, want 0", src.Quantity)
	}
	if dst := positionsOf(t, f, f.account2ID)[f.sberID]; !dst.Quantity.Equal(decimal.RequireFromString("0.05")) {
		t.Errorf("received quantity = %s, want 0.05", dst.Quantity)
	}
}

// split_ratio is normalized before the check too.
//
//	buy 10 SBER on 01.07 ; sell 5.0000000004 on 03.07
//	then a backdated 1:2 split on 02.07 typed as 0.500000000049
//
// As typed the post-split position is 5.00000000049 and the sell fits; stored the
// ratio is 0.5, the position 5, and the sell does not. The check now runs against
// the stored ratio, so the split is refused at entry instead of breaking every
// later read.
func TestSplitRatioIsRecordedAtTheScaleItIsCheckedAt(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	sell := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-03"), Quantity: dec("5.0000000004"), Price: dec("120"),
		AmountMinor: 60_000, Currency: "RUB",
	}
	for _, op := range []operation.Operation{buy, sell} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.Type, err)
		}
	}

	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("0.500000000049"),
		AmountMinor: 0, Currency: "RUB",
	}
	if err := trySplit(t, f, svc, split); !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("backdated split with a ratio finer than the journal: err = %v, want ErrInconsistent — accepting it records a ratio that makes the existing sell an oversell", err)
	}

	// Refused means the journal is unchanged and still reads.
	if p := positionsOf(t, f, f.accountID)[f.sberID]; !p.Quantity.Equal(decimal.RequireFromString("4.9999999996")) {
		t.Errorf("position = %s, want 4.9999999996 (10 − 5.0000000004, the split never happened)", p.Quantity)
	}
}

// A quantity entirely past the tenth decimal is refused as an input error,
// not stored as zero.
func TestQuantityFinerThanTheJournalIsRefusedRatherThanRoundedToNothing(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.00000000004"), Price: dec("100"),
		AmountMinor: -1, Currency: "RUB",
	}
	err := func() error {
		_, err := svc.Create(f.ctx, f.spaceID, buy)
		return err
	}()
	if !errors.Is(err, family.ErrValidation) || !strings.Contains(err.Error(), "finer than") {
		t.Errorf("err = %v, want a validation error saying the quantity is finer than the journal records", err)
	}
}
