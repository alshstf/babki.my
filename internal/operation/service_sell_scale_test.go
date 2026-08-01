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

// sellEverything sells exactly what the account's positions screen says is
// held — the one request a user is guaranteed to make sooner or later, and the
// one that used to be impossible to record faithfully. The quantity is taken
// from the screen rather than hard-coded on purpose: the bug this reproduces is
// precisely that the number the screen shows could not be written into the
// journal, so a test that types in a hand-picked, already-storable quantity
// would sail past it.
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

// storedSell returns the quantity the journal actually holds for the account's
// only sell — what every later read of that account compares against, as
// opposed to what the service had in hand when it accepted the request.
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

// TestSellingEverythingAfterASplitStaysReadable is the reviewer's reproduction
// of the same fault the transfer path had, on the path nobody had looked at:
// an ordinary sell, with no transfer anywhere in the journal.
//
//	buy 0.35 SBER on 01.07 for 35,00
//	reverse split 1:3 on 02.07 (ratio 0.3333333333)
//	sell everything on 03.07
//
// 0.35 × 0.3333333333 is 0.116666666655, and the journal keeps ten decimal
// places. Selling "everything" was checked against the exact figure in memory
// and recorded as 0.1166666667 — Postgres rounds to NEAREST, which is UP here —
// so from that moment every read of the account compared a recorded 0.1166666667
// against a position of 0.116666666655 and answered
//
//	422: not enough quantity: have 0.116666666655, need 0.1166666667
//
// forever, for every instrument on the account. Accepted with a 201, broken on
// every later read, by data this program wrote itself.
//
// Both halves are closed now. The split no longer produces a quantity the
// journal cannot name (portfolio.Position.applySplit), so "everything" is a
// number a sell row can hold; and the request is brought onto that scale before
// it is checked (operation.normalizeForStorage), so what was validated is what
// was written. The position closes exactly, with no unsellable dust behind it.
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
	for _, op := range []operation.Operation{buy, split} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.Type, err)
		}
	}

	held := positionsOf(t, f, f.accountID)[f.sberID].Quantity
	if want := decimal.RequireFromString("0.1166666666"); !held.Equal(want) {
		// Reported, not fatal: the rest of this test is the reproduction
		// proper, and it is worth running against a position like this to see
		// where it ends up.
		t.Errorf("position after the split = %s, want %s (the journal cannot name 0.116666666655, so neither may a position)", held, want)
	}

	sold := sellEverything(t, f, svc, f.accountID, "2026-07-03")

	// What the row says must be what the request said. Rounded up, this is the
	// 0.1166666667 that broke the account.
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

// TestSellingWhatATransferLeftBehindStaysReadable covers the reviewer's second
// route to the same 422, the one the previous wave's own fix opened: a transfer
// truncates the quantity it moves DOWN to the journal's scale, so whatever it
// leaves on the source is a remainder — and a remainder of an unrecordable
// position was itself unrecordable.
//
//	buy 0.35 SBER on 01.07 ; reverse split 1:3 on 02.07 ; move 0.05 away on 03.07
//	  → the source keeps 0.066666666655 : twenty-digit dust nobody can sell
//	sell the rest on 04.07 → accepted (201), recorded as 0.0666666667
//	  → the source account's positions screen: 422, forever
//
// With the split's own result on the scale, every remainder is on the scale
// too: a transfer subtracts a recordable quantity from a recordable position.
// The rest sells in one entry and the source closes at exactly zero.
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
	for _, op := range []operation.Operation{buy, split} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.Type, err)
		}
	}

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

	// Both screens must read: the source's, which the oversell used to take
	// down, and the destination's, which the previous wave was about.
	if src := positionsOf(t, f, f.accountID)[f.sberID]; !src.Quantity.IsZero() {
		t.Errorf("source quantity after selling the remainder = %s, want 0", src.Quantity)
	}
	if dst := positionsOf(t, f, f.account2ID)[f.sberID]; !dst.Quantity.Equal(decimal.RequireFromString("0.05")) {
		t.Errorf("received quantity = %s, want 0.05", dst.Quantity)
	}
}

// TestSplitRatioIsRecordedAtTheScaleItIsCheckedAt is the same divergence on the
// other field the engine replays. split_ratio is NUMERIC(20,10) like every
// quantity, and a ratio is a multiplier: rounding it on the way into the table
// moves every later quantity in the position.
//
//	buy 10 SBER on 01.07 ; sell 5.0000000004 on 03.07
//	then, backdated, a 1:2 split on 02.07 recorded as 0.500000000049
//
// Checked against the ratio as typed, the position after the split is
// 5.00000000049 and the existing sell of 5.0000000004 fits inside it, so the
// split was accepted. Stored, the ratio is 0.5000000000 — the twelfth digit is
// not kept — the position after the split is 5, the sell no longer fits, and
// the account's positions screen answers 422 from then on. Nothing the user did
// was wrong; the program checked one journal and wrote another.
//
// The ratio is now brought onto the scale BEFORE the check, so the check is
// made against the journal that will exist. This request is genuinely
// inconsistent with that journal and is refused as one, at the moment it is
// made, with an explanation — instead of being accepted and breaking every
// later read by somebody else.
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
	if _, err := svc.Create(f.ctx, f.spaceID, split); !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("backdated split with a ratio finer than the journal: err = %v, want ErrInconsistent — accepting it records a ratio that makes the existing sell an oversell", err)
	}

	// Refused means refused: the journal is exactly as it was, and the screen
	// this used to break still reads.
	if p := positionsOf(t, f, f.accountID)[f.sberID]; !p.Quantity.Equal(decimal.RequireFromString("4.9999999996")) {
		t.Errorf("position = %s, want 4.9999999996 (10 − 5.0000000004, the split never happened)", p.Quantity)
	}
}

// TestQuantityFinerThanTheJournalIsRefusedRatherThanRoundedToNothing is the
// counterpart of the transfer endpoint's own rule: a request whose entire
// quantity lives past the tenth decimal place cannot be recorded at all, and
// says so as the input error it is instead of quietly becoming a zero-quantity
// buy the engine would then reject with a confusing complaint of its own.
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
