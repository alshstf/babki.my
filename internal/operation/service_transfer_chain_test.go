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
			!g.AcquiredOn.Equal(date(w.acquiredOn)) {
			t.Errorf("account %s lot %d = %s/%d/%s, want %s/%d/%s", accountID, i,
				g.Quantity, g.CostMinor, g.AcquiredOn.Format("2006-01-02"),
				w.quantity, w.costMinor, w.acquiredOn)
		}
	}
}

// twoLots seeds the standard source history these tests share:
// buy 10 @ 100.00 on 01.07 (lot cost 100000) and buy 10 @ 900.00 on 03.07
// (lot cost 900000).
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

// TestTransferChainKeepsOriginalPurchaseDates covers the only case where a
// breakdown is built out of lots that a breakdown itself restored: A → B → C.
// B never saw the purchases; everything it knows about them arrived in the
// pieces A sent. When B passes the position on to C, the dates C receives can
// only be right if the second release read them off the lots the first one
// rebuilt — the round trip works once by construction, twice only if the
// restored lots are indistinguishable from bought ones.
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

	// Neither move is a purchase, so after two of them the shares still cost
	// what they cost and were still bought on the days they were bought —
	// 01.07 and 03.07, not 05.07 and not 08.07.
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
		if !pieces[i].AcquiredOn.Equal(date(w.acquiredOn)) {
			t.Errorf("B → C piece %d dated %s, want %s (B only ever knew this from A's pieces)",
				i, pieces[i].AcquiredOn.Format("2006-01-02"), w.acquiredOn)
		}
	}

	// B is left holding nothing, and the source too.
	checkLots(t, f, f.account2ID, nil)
	checkLots(t, f, f.accountID, nil)
}

// TestTransferBackToTheAccountItCameFrom covers the round trip: shares sent to
// another broker and then brought home. They must come back with the days they
// were bought, not the day either move happened — a position that has been
// away and returned is not a position bought this morning.
//
// They also come back at the END of the queue rather than at their old place
// in it. Nothing here was sold in between, so the FIFO order is not observable
// in this test; it is the owner's separate decision (restored lots queue where
// they arrive) and this test deliberately does not pin it either way.
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

// TestPartialTransferLeavesTheRestOfTheLotBehind looks at the half nobody was
// looking at: TestTransferCarriesSourceLotDates checks the 15 units that
// LEAVE, and stops there. What stays must be the other half of the same split
// — 5 units of the second lot, holding the 450000 the released piece did not
// take, still dated on the day that lot was bought. A source lot that lost its
// date or its remaining cost on the way out would misprice everything computed
// from it afterwards, and nothing was watching for it.
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

// TestTransferAfterAmortizationCarriesTheReducedBasis covers a bond whose
// principal has been partly returned. Amortization does not touch quantities;
// it drains cost out of the lots front to back, so what a later transfer moves
// is the SHRUNKEN basis. Carrying the original cost instead would hand the
// receiving account a bond that cost more than the owner still has in it, and
// pay out the same principal twice on paper.
//
//	buy 10 bonds on 01.07 for 1 000,00 (lot cost 100000)
//	amortization of 300,00 on 03.07 → the lot now holds 70000
//	transfer 5 on 05.07 → floor(70000 × 5 / 10) = 35000, still dated 01.07
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
	if pc := in.TransferLots[0]; pc.CostMinor != 35_000 || !pc.AcquiredOn.Equal(date("2026-07-01")) {
		t.Errorf("piece = %d/%s, want 35000/2026-07-01 (amortization drains cost, it does not re-date anything)",
			pc.CostMinor, pc.AcquiredOn.Format("2006-01-02"))
	}

	dest := positionsOf(t, f, f.account2ID)[bond.ID]
	if dest.CostMinor != 35_000 || len(dest.Lots) != 1 || !dest.Lots[0].AcquiredOn.Equal(date("2026-07-01")) {
		t.Errorf("received position = %d/%+v, want 35000 in one lot dated 2026-07-01", dest.CostMinor, dest.Lots)
	}
	if src := positionsOf(t, f, f.accountID)[bond.ID]; src.CostMinor != 35_000 {
		t.Errorf("source basis after the move = %d, want 35000 (the other half of the amortized basis)", src.CostMinor)
	}
}
