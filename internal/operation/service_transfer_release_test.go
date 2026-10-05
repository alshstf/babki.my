package operation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/operation"
)

// A transfer's departing leg releases the parcel it recorded (#60), checked on
// the family's books.
//
//	Брокер 3  buys 10 on 02.07 for 1 000,00
//	Брокер    buys 10 on 20.07 for 3 000,00
//	Брокер  -> Брокер 2, 10 on 22.07       records 3 000,00, bought 20.07
//	Брокер 3 -> Брокер,  10 on 21.07       backdated ahead of the move above
//
// Replayed by acquisition date, Брокер's queue on 22.07 leads with the 02.07
// parcel. A release computed afresh gave that one away while Брокер 2 held the
// 20.07 parcel: one parcel on two accounts and a family basis of 600 000 against
// 400 000 spent.
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
	// The recorded parcel, checked here so a capture change fails where it
	// is explicable.
	if len(in.TransferLots) != 1 || in.AmountMinor != 300_000 ||
		!sameAcquisition(in.TransferLots[0].AcquiredOn, datep("2026-07-20")) {
		t.Fatalf("the move recorded %d minor in %+v, want 300000 in one piece bought 2026-07-20",
			in.AmountMinor, in.TransferLots)
	}

	// Backdated ahead of the move: the 02.07 parcel heads Брокер's queue.
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: elsewhere.ID, ToAccountID: f.accountID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("10"),
		OccurredOn: date("2026-07-21"),
	}); err != nil {
		t.Fatalf("Брокер 3 → Брокер: %v", err)
	}

	// The source keeps the parcel its transfer did not name.
	checkLots(t, f, f.accountID, []lotSummary{{"10", 100_000, "2026-07-02"}})
	checkLots(t, f, f.account2ID, []lotSummary{{"10", 300_000, "2026-07-20"}})
	checkLots(t, f, elsewhere.ID, nil)

	// 400 000 left the family's cash and nothing was sold, so that is the
	// family's basis.
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

// A backdated sale that would consume a parcel a transfer recorded as departed
// is refused at entry, so the books stay right. Otherwise valid: ten held, ten
// sold. This is what the README describes, and it holds only while the write path
// replays the journal.
func TestBackdatedSellThatEatsARecordedParcelIsRefusedAtWrite(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	twoLots(t, f, svc) // 01.07: 10 for 100000, 03.07: 10 for 900000

	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("10"),
		OccurredOn: date("2026-07-10"),
	})
	if err != nil {
		t.Fatalf("Брокер → Брокер 2: %v", err)
	}
	if len(in.TransferLots) != 1 || !sameAcquisition(in.TransferLots[0].AcquiredOn, datep("2026-07-01")) {
		t.Fatalf("the move recorded %+v, want the 01.07 parcel — the rest of this test is about that parcel", in.TransferLots)
	}

	// Backdated ahead of the transfer, consuming the parcel it recorded.
	_, err = svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-05"), Quantity: dec("10"), Price: dec("200"),
		AmountMinor: 200_000, Currency: "RUB",
	})
	if !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("recording the sale answered %v, want ErrInconsistent: it takes away the parcel a transfer already recorded as gone, "+
			"and accepting it would leave that parcel on the receiving account and its basis counted twice", err)
	}
	if !strings.Contains(err.Error(), "record it again") {
		t.Errorf("refusal %q does not tell the owner how to record this sale (delete the transfer, record the sale, record the transfer again)", err)
	}

	// Nothing written; both accounts still replay.
	checkLots(t, f, f.accountID, []lotSummary{{"10", 900_000, "2026-07-03"}})
	checkLots(t, f, f.account2ID, []lotSummary{{"10", 100_000, "2026-07-01"}})
}
