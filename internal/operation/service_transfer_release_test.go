package operation_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/operation"
)

// TestTransferReleasesTheParcelItRecorded is issue #60 end to end, through the
// same service calls the API makes, and it is the family's books that are
// checked rather than one account's.
//
// A transfer freezes the parcel it moved — the pieces, their basis, the day
// each was bought — and hands it to the receiving account. The departing
// account used to ignore that record and work out a release of its own from
// its queue, which reproduced the frozen answer only while the queue's rule
// stayed put and the source's history stayed still. Neither holds: the rule
// moved once already (plan 7c ordered the queue by acquisition rather than by
// arrival), and history moves whenever a transfer that arrived is backdated
// ahead of one that departed — which is exactly what this test does, because it
// is the one way to reach the divergence through the public API alone.
//
// The sequence, all of it ordinary use:
//
//	Брокер 3  buys 10 on 02.07 for 1 000,00       — this parcel is elsewhere
//	Брокер    buys 10 on 20.07 for 3 000,00
//	Брокер  → Брокер 2, 10 units on 22.07         — records: 3 000,00, bought 20.07
//	Брокер 3 → Брокер,  10 units on 21.07         — backdated AHEAD of the move above
//
// Replayed by acquisition, Брокер's queue on 22.07 leads with the 02.07 parcel
// that has just arrived, so the departing leg gave THAT away — while Брокер 2
// went on holding the 20.07 parcel its own leg describes. One parcel on two
// accounts, the other gone, and the family's basis 600 000 minor units against
// the 400 000 it actually spent. The integrity check saw nothing: it only ever
// compared a leg against its own frozen numbers.
func TestTransferReleasesTheParcelItRecorded(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	elsewhere, err := f.accStore.Create(f.ctx, f.spaceID, nil, "Брокер 3", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("third account: %v", err)
	}

	for _, op := range []operation.Operation{{
		AccountID: elsewhere.ID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-20"), Quantity: dec("10"), Price: dec("30"),
		AmountMinor: -300_000, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy %s: %v", op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("10"),
		OccurredOn: date("2026-07-22"),
	})
	if err != nil {
		t.Fatalf("Брокер → Брокер 2: %v", err)
	}
	// The frozen record the rest of this test is about. Stated here so that a
	// later change to what CreateTransfer captures fails on this line, where it
	// is explicable, instead of on the family total further down.
	if len(in.TransferLots) != 1 || in.AmountMinor != 300_000 ||
		!sameAcquisition(in.TransferLots[0].AcquiredOn, datep("2026-07-20")) {
		t.Fatalf("the move recorded %d minor in %+v, want 300000 in one piece bought 2026-07-20",
			in.AmountMinor, in.TransferLots)
	}

	// Backdated ahead of the move that has already been written: the parcel
	// bought on 02.07 lands in Брокер on 21.07 and takes the head of its
	// queue, because it was bought before everything else the account holds.
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: elsewhere.ID, ToAccountID: f.accountID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("10"),
		OccurredOn: date("2026-07-21"),
	}); err != nil {
		t.Fatalf("Брокер 3 → Брокер: %v", err)
	}

	// What each account is left holding. The source keeps the parcel its
	// transfer did NOT name; the destination holds the one it did.
	checkLots(t, f, f.accountID, []lotSummary{{"10", 100_000, "2026-07-02"}})
	checkLots(t, f, f.account2ID, []lotSummary{{"10", 300_000, "2026-07-20"}})
	checkLots(t, f, elsewhere.ID, nil)

	// The books. 400 000 minor units left the family's cash for these twenty
	// shares and none of them has been sold, so 400 000 is what the family's
	// accounts may hold between them — no more, and not the same parcel twice.
	const spent = 400_000
	var held int64
	for _, accountID := range []uuid.UUID{f.accountID, f.account2ID, elsewhere.ID} {
		if pos := positionsOf(t, f, accountID)[f.sberID]; pos != nil {
			held += pos.CostMinor
		}
	}
	if held != spent {
		t.Errorf("the family's accounts hold %d of basis between them, want %d (%+d invented): "+
			"the departing leg gave away a parcel other than the one it recorded, so one parcel is on two accounts and another has vanished",
			held, spent, held-spent)
	}
}
