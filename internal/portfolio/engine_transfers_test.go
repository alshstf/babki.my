package portfolio_test

import (
	"errors"
	"strings"
	"testing"

	"babki.my/babki/internal/portfolio"
)

func TestSplitAdjustsQuantity(t *testing.T) {
	split := op(portfolio.TypeSplit, 5, &sber, "", "", 0, 0)
	split.SplitRatio = dp("10")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "3000", -3_000_000, 0),
		split,
		// after a 1:10 split, sell 50 of 100; cost released = 3000000/2
		op(portfolio.TypeSell, 6, &sber, "50", "310", 1_550_000, 0),
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
	outOps := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeTransferOut, 5, &sber, "4", "", 40_000, 0),
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
	inOps := []portfolio.Operation{
		op(portfolio.TypeTransferIn, 5, &sber, "4", "", 40_000, 0),
		op(portfolio.TypeSell, 6, &sber, "4", "120", 48_000, 0),
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
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "100", -30_000, 0),
		op(portfolio.TypeTransferOut, 2, &sber, "5", "", 0, 0),
	}
	if _, err := portfolio.Compute(ops); !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell", err)
	}
}

// piece builds one element of a transfer's stored FIFO breakdown.
func piece(qty string, cost int64, dayN int) portfolio.ReleasedLot {
	return portfolio.ReleasedLot{Quantity: d(qty), CostMinor: cost, AcquiredOn: dayp(dayN)}
}

// transferIn builds a transfer_in on day dayN carrying the given breakdown.
func transferIn(dayN int, qty string, amount int64, lots ...portfolio.ReleasedLot) portfolio.Operation {
	o := op(portfolio.TypeTransferIn, dayN, &sber, qty, "", amount, 0)
	o.TransferLots = lots
	return o
}

// TestTransferInRebuildsLotsFromBreakdown is the core of this change. The
// arriving leg carries the FIFO breakdown of what the source account released
// (see Operation.TransferLots), so the destination rebuilds exactly those
// lots — each with its own quantity, its own cost, and the day it was actually
// bought — instead of collapsing them into one lot dated on the transfer day.
// The dates are the point: every lot is later valued at the fx rate of the day
// it was acquired, so a collapsed lot misprices the whole arrived position.
func TestTransferInRebuildsLotsFromBreakdown(t *testing.T) {
	ops := []portfolio.Operation{
		transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("5", 55_005, 9)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	want := []portfolio.Lot{
		{Quantity: d("10"), CostMinor: 100_010, AcquiredOn: dayp(2)},
		{Quantity: d("5"), CostMinor: 55_005, AcquiredOn: dayp(9)},
	}
	if len(p.Lots) != len(want) {
		t.Fatalf("lots = %+v, want %d — one per piece of the breakdown", p.Lots, len(want))
	}
	for i, w := range want {
		got := p.Lots[i]
		if !got.Quantity.Equal(w.Quantity) || got.CostMinor != w.CostMinor || !sameAcquisition(got.AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = {qty %s cost %d on %s}, want {qty %s cost %d on %s}",
				i, got.Quantity, got.CostMinor, acquired(got.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
		if sameAcquisition(got.AcquiredOn, dayp(20)) {
			t.Errorf("lot %d is dated on the transfer day %s: the breakdown says it was bought on %s",
				i, day(20).Format("2006-01-02"), acquired(w.AcquiredOn))
		}
	}
	checkLotInvariants(t, p)
}

// TestTransferredLotsReleaseInFIFOOrder pins that the rebuilt lots are real
// lots and not just a display detail: a later sale in the destination account
// consumes them front-to-back, taking the oldest piece's cost first and
// leaving the younger piece — with its own date — behind.
func TestTransferredLotsReleaseInFIFOOrder(t *testing.T) {
	ops := []portfolio.Operation{
		transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("5", 55_005, 9)),
		op(portfolio.TypeSell, 25, &sber, "10", "120", 120_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	// The whole first piece is released, so realized P&L is measured against
	// that piece's cost alone — not against a share of one merged lot, which
	// for this fixture would be floor(155015*10/15) = 103_343 and give 16_657.
	if p.RealizedPnLMinor == 120_000-103_343 {
		t.Fatalf("realized = %d — that is a proportional share of ONE merged lot; the sale must consume the first piece of the breakdown whole",
			p.RealizedPnLMinor)
	}
	if p.RealizedPnLMinor != 120_000-100_010 {
		t.Errorf("realized = %d, want %d (120000 − the first piece's cost 100010)",
			p.RealizedPnLMinor, 120_000-100_010)
	}
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %+v, want 1 (the younger piece)", p.Lots)
	}
	if !sameAcquisition(p.Lots[0].AcquiredOn, dayp(9)) || p.Lots[0].CostMinor != 55_005 {
		t.Errorf("remaining lot = {cost %d on %s}, want {55005 on %s}",
			p.Lots[0].CostMinor, acquired(p.Lots[0].AcquiredOn), day(9).Format("2006-01-02"))
	}
	checkLotInvariants(t, p)
}

// TestTransferInBreakdownMismatchRejected pins the loud refusal. A breakdown
// that does not add up to the operation it rides on is a corrupted journal:
// the write path derives the operation's own totals by summing these very
// pieces, so the two can only disagree if the stored rows were damaged. Both
// readings are then unreliable, and quietly falling back to a single lot dated
// on the transfer day would replace corrupt data with a plausible-looking
// invention that nobody would ever notice.
func TestTransferInBreakdownMismatchRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		op   portfolio.Operation
		want []string
	}{
		"quantity sum too small": {
			op:   transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("4", 55_005, 9)),
			want: []string{"14", "15"},
		},
		"quantity sum too large": {
			op:   transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("6", 55_005, 9)),
			want: []string{"16", "15"},
		},
		"cost sum differs": {
			op:   transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("5", 55_000, 9)),
			want: []string{"155010", "155015"},
		},
		"piece with zero quantity": {
			op:   transferIn(20, "15", 155_015, piece("15", 100_010, 2), portfolio.ReleasedLot{Quantity: d("0"), CostMinor: 55_005, AcquiredOn: dayp(9)}),
			want: []string{"quantity 0"},
		},
		"pieces that cancel out": {
			op: transferIn(20, "15", 155_015,
				piece("20", 100_010, 2),
				portfolio.ReleasedLot{Quantity: d("-5"), CostMinor: 55_005, AcquiredOn: dayp(9)}),
			want: []string{"-5"},
		},
		"piece with negative cost": {
			op:   transferIn(20, "15", 155_015, piece("10", 200_020, 2), piece("5", -45_005, 9)),
			want: []string{"-45005"},
		},
		// The acquisition date is the only field the table itself does not
		// constrain, and it is the one the whole breakdown exists to carry: an
		// IMPOSSIBLE date turns into a lot revalued at a rate from a day it was
		// never held on. A date that is simply absent is a different thing
		// entirely and is accepted — see
		// TestTransferInAcceptsPieceWithoutAcquisitionDate.
		"piece acquired after the transfer": {
			op:   transferIn(20, "15", 155_015, piece("10", 100_010, 2), piece("5", 55_005, 25)),
			want: []string{"after the transfer"},
		},
	} {
		_, err := portfolio.Compute([]portfolio.Operation{tc.op})
		if !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want ErrBadOperation — a breakdown that does not add up must not be papered over", name, err)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q, so it cannot be acted on", name, err, want)
			}
		}
	}
}

// TestTransferInAcceptsPieceWithoutAcquisitionDate is the rule this task
// reverses. CheckTransferLots used to refuse a piece with no acquisition date
// outright, on the grounds that "the date is what a carried lot is for". That
// refusal quietly required every piece to name a day — and for shares whose
// purchase day is not knowable, the only day on hand is the transfer's own,
// which is precisely the invention the breakdown exists to prevent.
//
// Such pieces are real. They arise the moment a parcel that arrived without
// dates (a hand-entered basis, or a transfer written down before breakdowns
// were kept) is moved on again: the release yields pieces with nothing to
// date them by, mixed in with pieces that do know their day. The breakdown
// must carry that mixture faithfully, and the lots it rebuilds must too.
//
// The rebuilt lots are asserted in QUEUE order, and the queue is ordered by
// acquisition (see Position.Lots and addLot), so the undated lot stands at the
// head even though its piece is the second one in the breakdown. That is the
// only part of this test the acquisition-ordering change moved: it used to read
// the pieces off in the order they were listed, because the queue used to be
// arrival order. What the test is here to pin is unchanged and still asserted —
// a dateless piece is accepted rather than refused, it becomes a lot of its
// own, and no date is invented for it, least of all the transfer's own.
func TestTransferInAcceptsPieceWithoutAcquisitionDate(t *testing.T) {
	undated := portfolio.ReleasedLot{Quantity: d("5"), CostMinor: 55_005}
	ops := []portfolio.Operation{
		transferIn(20, "15", 155_015, piece("10", 100_010, 2), undated),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v — a piece with no acquisition date is a legitimate breakdown, not a corrupt one", err)
	}
	p := pos[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2 — one per piece, the undated one included", p.Lots)
	}
	if p.Lots[0].AcquiredOn != nil {
		t.Errorf("lot 0 acquired on %s, want unknown — the piece carried no date, the engine must not supply one, and a lot with no date leads the queue",
			acquired(p.Lots[0].AcquiredOn))
	}
	if sameAcquisition(p.Lots[0].AcquiredOn, dayp(20)) {
		t.Errorf("lot 0 was dated on the transfer day %s: the breakdown said nothing about when it was bought", acquired(dayp(20)))
	}
	if p.Lots[0].CostMinor != 55_005 {
		t.Errorf("lot 0 cost = %d, want 55005 (the undated piece)", p.Lots[0].CostMinor)
	}
	if !sameAcquisition(p.Lots[1].AcquiredOn, dayp(2)) || p.Lots[1].CostMinor != 100_010 {
		t.Errorf("lot 1 = {cost %d on %s}, want {100010 on %s} — the dated piece keeps its own day and queues behind the undated one",
			p.Lots[1].CostMinor, acquired(p.Lots[1].AcquiredOn), acquired(dayp(2)))
	}
	if p.CostMinor != 155_015 {
		t.Errorf("cost = %d, want 155015 — an unknown date changes no money", p.CostMinor)
	}
	checkLotInvariants(t, p)
}

// TestUndatedPieceStillCheckedForSums pins that accepting a dateless piece
// relaxes ONLY the date rule. Everything else CheckTransferLots guards — the
// pieces summing to the quantity that moved and to the basis that moved — is
// unchanged for such a piece, so a breakdown cannot smuggle a mismatch through
// by omitting a date.
func TestUndatedPieceStillCheckedForSums(t *testing.T) {
	ops := []portfolio.Operation{
		transferIn(20, "15", 155_015,
			piece("10", 100_010, 2),
			portfolio.ReleasedLot{Quantity: d("5"), CostMinor: 55_000}),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation: the costs sum to 155010, not the 155015 the operation carries", err)
	}
	if !strings.Contains(err.Error(), "155010") {
		t.Errorf("error %q does not name the sum it found", err)
	}
}

func TestReleasedCostHelper(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 5),
		op(portfolio.TypeBuy, 2, &sber, "10", "200", -200_000, 5),
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
