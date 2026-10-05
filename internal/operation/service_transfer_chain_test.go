package operation_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// lotSummary is one lot flattened for comparison, so a test can state the
// expected position as a list and get one readable diff instead of three.
type lotSummary struct {
	quantity   string
	costMinor  int64
	acquiredOn string
}

// checkLots asserts an account's lots for the instrument, in queue order.
func checkLots(t *testing.T, f fixture, accountID uuid.UUID, want []lotSummary) {
	t.Helper()
	pos := positionsOf(t, f, accountID)[f.sberID]
	if pos == nil {
		if len(want) == 0 {
			return
		}
		t.Fatalf("no position on account %s, want %d lots", accountID, len(want))
	}
	if len(pos.Lots) != len(want) {
		t.Fatalf("account %s holds %d lots, want %d: %+v", accountID, len(pos.Lots), len(want), pos.Lots)
	}
	for i, w := range want {
		g := pos.Lots[i]
		if !g.Quantity.Equal(decimal.RequireFromString(w.quantity)) ||
			g.CostMinor != w.costMinor ||
			!sameAcquisition(g.AcquiredOn, datep(w.acquiredOn)) {
			t.Errorf("account %s lot %d = %s/%d/%s, want %s/%d/%s", accountID, i,
				g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
				w.quantity, w.costMinor, w.acquiredOn)
		}
	}
}

// twoLots seeds buy 10 @ 100.00 on 01.07 (100000) and buy 10 @ 900.00 on
// 03.07 (900000).
func twoLots(t *testing.T, f fixture, svc *operation.Service) {
	t.Helper()
	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000
	for _, op := range []operation.Operation{buy1, buy2} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy %s: %v", op.OccurredOn.Format("2006-01-02"), err)
		}
	}
}

// A -> B -> C: the second release reads dates off lots the first one
// rebuilt, so they must be indistinguishable from bought ones.
func TestTransferChainKeepsOriginalPurchaseDates(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	twoLots(t, f, svc)

	third, err := f.accStore.Create(f.ctx, f.spaceID, nil, "Брокер 3", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("third account: %v", err)
	}

	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("20"),
		OccurredOn: date("2026-07-05"),
	}); err != nil {
		t.Fatalf("A → B: %v", err)
	}
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.account2ID, ToAccountID: third.ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("20"),
		OccurredOn: date("2026-07-08"),
	}); err != nil {
		t.Fatalf("B → C: %v", err)
	}

	// Still bought on 01.07 and 03.07, not on either move's day.
	want := []lotSummary{
		{"10", 100_000, "2026-07-01"},
		{"10", 900_000, "2026-07-03"},
	}
	checkLots(t, f, third.ID, want)

	// The second hop's own breakdown says the same thing, since that is where
	// C's lots came from.
	pieces := transferInOf(t, f, third.ID).TransferLots
	if len(pieces) != len(want) {
		t.Fatalf("B → C breakdown = %+v, want %d pieces", pieces, len(want))
	}
	for i, w := range want {
		if !sameAcquisition(pieces[i].AcquiredOn, datep(w.acquiredOn)) {
			t.Errorf("B → C piece %d dated %s, want %s (B only ever knew this from A's pieces)",
				i, acquired(pieces[i].AcquiredOn), w.acquiredOn)
		}
	}

	// B is left holding nothing, and the source too.
	checkLots(t, f, f.account2ID, nil)
	checkLots(t, f, f.accountID, nil)
}

// Shares sent away and brought home keep their purchase days. The queue
// order is held by portfolio.TestTransferredLotBoughtEarlierIsSoldFirst; this
// account is emptied in between, so it cannot observe it.
func TestTransferBackToTheAccountItCameFrom(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	twoLots(t, f, svc)

	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("20"),
		OccurredOn: date("2026-07-05"),
	}); err != nil {
		t.Fatalf("A → B: %v", err)
	}
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.account2ID, ToAccountID: f.accountID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("20"),
		OccurredOn: date("2026-07-09"),
	}); err != nil {
		t.Fatalf("B → A: %v", err)
	}

	checkLots(t, f, f.accountID, []lotSummary{
		{"10", 100_000, "2026-07-01"},
		{"10", 900_000, "2026-07-03"},
	})
	checkLots(t, f, f.account2ID, nil)

	if pos := positionsOf(t, f, f.accountID)[f.sberID]; pos.CostMinor != 1_000_000 {
		t.Errorf("basis after the round trip = %d, want 1000000 (a return trip costs nothing)", pos.CostMinor)
	}
}

// What stays behind after a partial transfer: 5 units of the second lot with
// the 450000 the released piece did not take, still dated.
func TestPartialTransferLeavesTheRestOfTheLotBehind(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	twoLots(t, f, svc)

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("15"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if in.AmountMinor != 550_000 {
		t.Fatalf("moved basis = %d, want 550000 (100000 + half of 900000)", in.AmountMinor)
	}

	// The first lot left whole; the second was split in half and its remainder
	// keeps both the day it was bought and the cost that was not released.
	checkLots(t, f, f.accountID, []lotSummary{
		{"5", 450_000, "2026-07-03"},
	})
	if pos := positionsOf(t, f, f.accountID)[f.sberID]; pos.CostMinor != 450_000 {
		t.Errorf("source basis after the move = %d, want 450000 (1000000 − 550000: what left plus what stayed is what there was)",
			pos.CostMinor)
	}
	checkLots(t, f, f.account2ID, []lotSummary{
		{"10", 100_000, "2026-07-01"},
		{"5", 450_000, "2026-07-03"},
	})
}

// Amortization drains cost front to back, so a later transfer moves the
// reduced basis.
//
//	buy 10 bonds on 01.07 for 1 000,00 (100000)
//	amortization of 300,00 on 03.07 -> the lot holds 70000
//	transfer 5 on 05.07 -> 70000 × 5 / 10 = 35000, dated 01.07
func TestTransferAfterAmortizationCarriesTheReducedBasis(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// A bond, since amortization is a bond's event; the engine does not check
	// the instrument's type, but a test that says "bond" should use one.
	bond, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("bond: %v", err)
	}

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &bond.ID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	amortization := operation.Operation{
		AccountID: f.accountID, InstrumentID: &bond.ID, Type: operation.TypeAmortization,
		OccurredOn: date("2026-07-03"), AmountMinor: 30_000, Currency: "RUB",
	}
	for _, op := range []operation.Operation{buy, amortization} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.Type, err)
		}
	}

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: bond.ID, Quantity: decimal.RequireFromString("5"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer after amortization: %v", err)
	}
	if in.AmountMinor != 35_000 {
		t.Errorf("moved basis = %d, want 35000 (half of the amortized 70000, not half of the original 100000)", in.AmountMinor)
	}
	if len(in.TransferLots) != 1 {
		t.Fatalf("breakdown = %+v, want one piece", in.TransferLots)
	}
	if pc := in.TransferLots[0]; pc.CostMinor != 35_000 || !sameAcquisition(pc.AcquiredOn, datep("2026-07-01")) {
		t.Errorf("piece = %d/%s, want 35000/2026-07-01 (amortization drains cost, it does not re-date anything)",
			pc.CostMinor, acquired(pc.AcquiredOn))
	}

	dest := positionsOf(t, f, f.account2ID)[bond.ID]
	if dest.CostMinor != 35_000 || len(dest.Lots) != 1 || !sameAcquisition(dest.Lots[0].AcquiredOn, datep("2026-07-01")) {
		t.Errorf("received position = %d/%+v, want 35000 in one lot dated 2026-07-01", dest.CostMinor, dest.Lots)
	}
	if src := positionsOf(t, f, f.accountID)[bond.ID]; src.CostMinor != 35_000 {
		t.Errorf("source basis after the move = %d, want 35000 (the other half of the amortized basis)", src.CostMinor)
	}
}
