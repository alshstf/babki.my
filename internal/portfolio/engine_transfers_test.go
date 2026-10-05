package portfolio_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/portfolio/portfoliotest"
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
	if portfoliotest.Realized(t, p) != 1_550_000-1_500_000 {
		t.Errorf("realized = %d", portfoliotest.Realized(t, p))
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
	if !p.Quantity.Equal(d("6")) || p.CostMinor != 60_000 || portfoliotest.Realized(t, p) != 0 {
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
	if portfoliotest.Realized(t, pos[sber]) != 8_000 {
		t.Errorf("dest realized = %d, want 8000", portfoliotest.Realized(t, pos[sber]))
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

// transferOut builds the departing leg; both legs carry one breakdown.
func transferOut(dayN int, qty string, amount int64, lots ...portfolio.ReleasedLot) portfolio.Operation {
	o := op(portfolio.TypeTransferOut, dayN, &sber, qty, "", amount, 0)
	o.TransferLots = lots
	return o
}

// The departing leg releases the lots its record names (#60). Shares bought
// here on day 20 and shares bought on day 2 arriving later: the record names
// the day-20 parcel, while the acquisition-ordered queue's head is the day-2
// one. Releasing by the queue put one lot on both accounts and lost the other
// (200 000 of invented basis). The family's total is asserted.
func TestTransferOutReleasesTheLotsItRecorded(t *testing.T) {
	moved := piece("10", 300_000, 20)
	source := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferIn(21, "10", 100_000, piece("10", 100_000, 2)),
		transferOut(22, "10", 300_000, moved),
	}
	destination := []portfolio.Operation{transferIn(22, "10", 300_000, moved)}

	src, err := portfolio.Compute(source)
	if err != nil {
		t.Fatalf("Compute source: %v", err)
	}
	dst, err := portfolio.Compute(destination)
	if err != nil {
		t.Fatalf("Compute destination: %v", err)
	}
	from, to := src[sber], dst[sber]

	if len(from.Lots) != 1 {
		t.Fatalf("source holds %+v, want the single day-%s parcel the transfer did NOT move", from.Lots, day(2).Format("02"))
	}
	if !sameAcquisition(from.Lots[0].AcquiredOn, dayp(2)) || from.Lots[0].CostMinor != 100_000 {
		t.Errorf("source kept {cost %d on %s}, want {100000 on %s}: the breakdown says the day-%s parcel left, so this is the one that stays",
			from.Lots[0].CostMinor, acquired(from.Lots[0].AcquiredOn), acquired(dayp(2)), day(20).Format("02"))
	}
	if len(to.Lots) != 1 || !sameAcquisition(to.Lots[0].AcquiredOn, dayp(20)) {
		t.Fatalf("destination holds %+v, want the day-%s parcel", to.Lots, day(20).Format("02"))
	}
	if sameAcquisition(from.Lots[0].AcquiredOn, to.Lots[0].AcquiredOn) {
		t.Errorf("both accounts hold a parcel acquired %s — one parcel cannot be in two places, and the one it displaced has vanished",
			acquired(to.Lots[0].AcquiredOn))
	}

	const spent = 400_000 // 300000 bought here + 100000 the arriving parcel cost
	if held := from.CostMinor + to.CostMinor; held != spent {
		t.Errorf("the two accounts hold %d of basis between them, want %d — the family cannot hold more than it paid (%+d invented)",
			held, spent, held-spent)
	}
	checkLotInvariants(t, from)
	checkLotInvariants(t, to)
}

// A breakdown naming half a lot leaves the other half, and a parcel ahead in
// the queue untouched.
func TestTransferOutTakesOnlyPartOfTheLotItRecorded(t *testing.T) {
	moved := piece("5", 150_000, 20)
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferIn(21, "20", 200_000, piece("20", 200_000, 2)),
		transferOut(22, "5", 150_000, moved),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	want := []portfolio.Lot{
		{Quantity: d("20"), CostMinor: 200_000, AcquiredOn: dayp(2)},
		{Quantity: d("5"), CostMinor: 150_000, AcquiredOn: dayp(20)},
	}
	if len(p.Lots) != len(want) {
		t.Fatalf("lots = %+v, want %d: the untouched day-%s parcel and the half of the day-%s one that stayed",
			p.Lots, len(want), day(2).Format("02"), day(20).Format("02"))
	}
	for i, w := range want {
		got := p.Lots[i]
		if !got.Quantity.Equal(w.Quantity) || got.CostMinor != w.CostMinor || !sameAcquisition(got.AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = {qty %s cost %d on %s}, want {qty %s cost %d on %s}",
				i, got.Quantity, got.CostMinor, acquired(got.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	checkLotInvariants(t, p)
}

// Without a breakdown (a hand-given basis, or pre-breakdown) the queue
// decides — legitimately. Same fixture as above, record removed.
func TestTransferOutWithoutBreakdownReleasesByTheQueue(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferIn(21, "10", 100_000, piece("10", 100_000, 2)),
		op(portfolio.TypeTransferOut, 22, &sber, "10", "", 100_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v — a transfer with no breakdown is legitimate, not corrupt", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 || !sameAcquisition(p.Lots[0].AcquiredOn, dayp(20)) || p.CostMinor != 300_000 {
		t.Errorf("source holds %+v (cost %d), want the day-%s parcel alone (300000): with nothing recorded, the head of the acquisition queue is what leaves",
			p.Lots, p.CostMinor, day(20).Format("02"))
	}
	checkLotInvariants(t, p)
}

// A piece whose day has no shares left is refused: taking another day's lot
// would re-date held shares, taking nothing would double the basis.
func TestTransferOutRefusesAParcelTheAccountDoesNotHold(t *testing.T) {
	for name, ops := range map[string][]portfolio.Operation{
		"no lot was ever acquired on that day": {
			op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
			transferOut(22, "10", 300_000, piece("10", 300_000, 5)),
		},
		// Enough shares overall, but a sale has eaten into the recorded parcel.
		"the day is right but too little of it is left": {
			op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
			op(portfolio.TypeBuy, 21, &sber, "10", "", -100_000, 0),
			op(portfolio.TypeSell, 22, &sber, "4", "", 150_000, 0),
			transferOut(23, "10", 300_000, piece("10", 300_000, 20)),
		},
		"the piece knows no day and every lot does": {
			op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
			transferOut(22, "10", 300_000, portfolio.ReleasedLot{Quantity: d("10"), CostMinor: 300_000}),
		},
	} {
		_, err := portfolio.Compute(ops)
		if !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want ErrBadOperation — a record the journal contradicts must not be quietly replaced by a fresh guess", name, err)
			continue
		}
		checkNamesBothCausesAndTheWayOut(t, name, err)
	}
}

// checkNamesBothCausesAndTheWayOut pins the message: both causes (an edit, or
// an earlier build's queue rule — the usual one) and the way out.
func checkNamesBothCausesAndTheWayOut(t *testing.T, name string, err error) {
	t.Helper()
	for _, want := range []string{
		"edited after the transfer was recorded", // the cause that may be true
		"a different rule",                       // the cause that usually is
		"record it again",                        // the way out, the same either way
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not mention %q — it must name both possible causes, neither as a fact, and say what to do",
				name, err, want)
		}
	}
}

// The refusal on untouched data: an older build's arrival-ordered queue let a
// day-22 sale take the day-20 parcel and the transfer record the day-2 one;
// today's queue takes the day-2 parcel instead (#60 from the other side).
func TestTransferOutRefusesAParcelAnEarlierQueueRuleRecorded(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferIn(21, "10", 100_000, piece("10", 100_000, 2)),
		op(portfolio.TypeSell, 22, &sber, "10", "", 150_000, 0),
		transferOut(23, "10", 100_000, piece("10", 100_000, 2)),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation: the day-%s parcel this transfer recorded was consumed by the sale under today's queue rule",
			err, day(2).Format("02"))
	}
	checkNamesBothCausesAndTheWayOut(t, "recorded under the arrival-order rule", err)
}

// Moving more than is held is an oversell, refused before matching with the
// right message.
func TestTransferOutRefusesToMoveMoreThanTheAccountHolds(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferOut(22, "15", 300_000, piece("15", 300_000, 20)),
	}
	if _, err := portfolio.Compute(ops); !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell: 15 units cannot leave an account holding 10", err)
	}
}

// Basis a breakdown carries beyond its lots is drained from the queue's head,
// and there must be that much: otherwise the basis would go negative
// silently.
func TestTransferOutRefusesABreakdownCarryingBasisTheAccountDoesNotHold(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "", -100_000, 0),
		transferOut(5, "10", 300_000, piece("10", 300_000, 2)),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation: the breakdown moves 300000 out of an account that only ever held 100000", err)
	}
	if !strings.Contains(err.Error(), "200000") {
		t.Errorf("error %q does not name the 200000 minor units that are nowhere on the account", err)
	}
	checkNamesBothCausesAndTheWayOut(t, "more basis than the account holds", err)
}

// The departing leg checks the breakdown sums too: both legs read one set of
// pieces.
func TestTransferOutRefusesABreakdownThatDoesNotAddUp(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferOut(22, "10", 300_000, piece("10", 299_999, 20)),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation: the pieces sum to 299999, not the 300000 the operation carries", err)
	}
	if !strings.Contains(err.Error(), "299999") {
		t.Errorf("error %q does not name the sum it found", err)
	}
}

// A piece may carry more basis than its own lot holds — a shareless lot's
// money folded in by operation.quantizeLots — and the excess must come from
// that shareless lot, not from an untouched lot of the same day.
//
//	day 1: 3 units, 30 000 (rounded to no shares by the split)
//	day 2: two lots; the piece takes one of them and carries the 30 000
func TestTransferOutTakesTheBasisOfAShareLessLotItsPieceCarries(t *testing.T) {
	// 3e-11 rounds the day-1 lot away and leaves each day-2 lot with 3e-10.
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.00000000003")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -30_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -100_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -900_000, 0),
		split,
		// What CreateTransfer records for the first two lots: one piece dated by the
		// first lot with shares, carrying the shareless lot's 30 000.
		transferOut(4, "0.0000000003", 130_000, piece("0.0000000003", 130_000, 2)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v — this breakdown is one CreateTransfer itself writes; refusing it refuses healthy data", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("source holds %+v, want one lot: the day-%s parcel the transfer never named. A shareless lot still holding money it gave away is that money counted twice",
			p.Lots, day(2).Format("02"))
	}
	if !p.Lots[0].Quantity.Equal(d("0.0000000003")) || p.Lots[0].CostMinor != 900_000 {
		t.Errorf("remaining lot = {qty %s cost %d}, want {0.0000000003 900000} — the carried 30000 belongs to the shareless lot that departed, not to this one",
			p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	if p.CostMinor != 1_030_000-130_000 {
		t.Errorf("source basis = %d, want %d (1030000 spent − 130000 moved)", p.CostMinor, 1_030_000-130_000)
	}
	checkLotInvariants(t, p)
}

// The same with the piece taking its lot only in part: clamping by the lot's
// whole cost would take the 30 000 from the fraction's own parcel and leave the
// shareless lot holding money the destination holds too. Totals would still
// balance; only the parcels would be wrong.
//
//	day 1: 3 for 30 000; day 2: 10 for 900 000; reverse split; a third departs
func TestTransferOutTakesAShareLessLotsBasisWhenItsPieceTakesOnlyPartOfALot(t *testing.T) {
	// 3e-11 leaves the day-1 lot with no shares and the day-2 lot with 3e-10.
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.00000000003")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -30_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -900_000, 0),
		split,
		// The record: the shareless 30 000 plus a third of 900 000, in one piece.
		transferOut(4, "0.0000000001", 330_000, piece("0.0000000001", 330_000, 2)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v — this breakdown is one CreateTransfer itself writes", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("source holds %+v, want one lot: the two thirds of the day-%s parcel that stayed. A shareless lot still holding the money it gave away is that money counted twice",
			p.Lots, day(2).Format("02"))
	}
	if !p.Lots[0].Quantity.Equal(d("0.0000000002")) || p.Lots[0].CostMinor != 600_000 {
		t.Errorf("remaining lot = {qty %s cost %d}, want {0.0000000002 600000} — two thirds of the shares keep two thirds of the money; the carried 30000 belongs to the shareless lot that departed",
			p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	if p.CostMinor != 930_000-330_000 {
		t.Errorf("source basis = %d, want %d (930000 spent − 330000 moved)", p.CostMinor, 930_000-330_000)
	}
	checkLotInvariants(t, p)
}

// Two same-day lots: each of two same-dated pieces finds its own lot; 15 of 20
// move and 5 stay with their basis.
func TestTransferOutMatchesPiecesToLotsOfTheSameDay(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "", -100_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -900_000, 0),
		transferOut(5, "15", 550_000, piece("10", 100_000, 2), piece("5", 450_000, 2)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %+v, want 1: the half of the second buy that stayed", p.Lots)
	}
	if !p.Lots[0].Quantity.Equal(d("5")) || p.Lots[0].CostMinor != 450_000 {
		t.Errorf("remaining lot = {qty %s cost %d}, want {5 450000}", p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	checkLotInvariants(t, p)
}

// A split entered after a transfer but dated before it doubles the recorded
// lot; the record is honoured, so the source keeps shares with no basis. The
// family total is right; re-entering the transfer evens it.
func TestBackdatedSplitLeavesTheSourceWithSharesAndNoBasis(t *testing.T) {
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("2")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "", -300_000, 0),
		split, // entered later, dated before the transfer below
		transferOut(5, "10", 300_000, piece("10", 300_000, 1)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v — a backdated split still replays; the record it moves under is honoured, not refused", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 || !p.Lots[0].Quantity.Equal(d("10")) || p.Lots[0].CostMinor != 0 {
		t.Fatalf("source holds %+v, want one lot of 10 shares with no basis: the 300000 the record moved came off twenty shares, not the ten it was struck against",
			p.Lots)
	}
	checkLotInvariants(t, p)
}

// The arriving leg rebuilds the breakdown's lots, each with its quantity,
// cost and purchase day, instead of one lot dated on the transfer.
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

// Rebuilt lots are real: a later sale consumes them oldest first.
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
	// The first piece is released whole; a merged lot would give 16 657.
	if portfoliotest.Realized(t, p) == 120_000-103_343 {
		t.Fatalf("realized = %d — that is a proportional share of ONE merged lot; the sale must consume the first piece of the breakdown whole",
			portfoliotest.Realized(t, p))
	}
	if portfoliotest.Realized(t, p) != 120_000-100_010 {
		t.Errorf("realized = %d, want %d (120000 − the first piece's cost 100010)",
			portfoliotest.Realized(t, p), 120_000-100_010)
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

// A breakdown that does not add up to its operation is refused: the write path
// sums these very pieces, so a mismatch is damage.
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
		// No units with money is a shareless parcel; no units and no money is
		// nothing.
		"piece with neither units nor cost": {
			op:   transferIn(20, "15", 100_010, piece("15", 100_010, 2), portfolio.ReleasedLot{Quantity: d("0"), CostMinor: 0, AcquiredOn: dayp(9)}),
			want: []string{"neither units nor cost"},
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
		// An impossible acquisition date (after the transfer) is refused; an absent one
		// is accepted.
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

// A piece without an acquisition date is accepted and becomes an undated lot
// of its own, at the head of the queue; no date is invented.
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

// An undated piece is still checked for sums.
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

// Two same-day parcels at different prices are not interchangeable: when a
// backdated sale took the cheap one, the record's five units are refused
// rather than taken from the dear one at the cheap price (#197).
func TestARecordedPieceIsNotTakenFromADearerParcelOfTheSameDay(t *testing.T) {
	out := op(portfolio.TypeTransferOut, 5, &sber, "5", "", 50_000, 0)
	out.TransferLots = []portfolio.ReleasedLot{piece("5", 50_000, 2)}
	buys := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "900", -900_000, 0),
	}

	// As recorded, the journal replays: the piece is half of the cheap parcel.
	if _, err := portfolio.Compute(append(slices.Clone(buys), out)); err != nil {
		t.Fatalf("the journal the transfer was recorded against: %v", err)
	}

	// With the backdated sale underneath it, it must not.
	sale := op(portfolio.TypeSell, 3, &sber, "10", "150", 150_000, 0)
	_, err := portfolio.Compute(append(slices.Clone(buys), sale, out))
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation — the parcel the record names is gone", err)
	}
	if !strings.Contains(err.Error(), "50000") || !strings.Contains(err.Error(), "450000") {
		t.Errorf("error %q does not name both figures, so it cannot be acted on", err)
	}
}
