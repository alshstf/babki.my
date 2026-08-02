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

// transferOut builds the departing leg of the same parcel. Both legs of a pair
// carry one breakdown (see portfolio.Operation.TransferLots), which is why the
// tests below hand the very same pieces to both.
func transferOut(dayN int, qty string, amount int64, lots ...portfolio.ReleasedLot) portfolio.Operation {
	o := op(portfolio.TypeTransferOut, dayN, &sber, qty, "", amount, 0)
	o.TransferLots = lots
	return o
}

// TestTransferOutReleasesTheLotsItRecorded is issue #60, folded.
//
// A transfer freezes what it moved: the pieces, their basis, and the day each
// was bought. The departing leg used to ignore all of that and work out a
// release of its own from the queue — which reproduced the frozen answer only
// while the rule building the queue stayed put. Ordering the queue by
// acquisition instead of by arrival (plan 7c) moved it, and every transfer
// already in the journal began releasing lots other than the ones it had
// recorded, with nobody editing anything.
//
// The fixture is the smallest account where the two rules disagree: shares
// bought here on day 20, and shares bought on day 2 that arrived later. The
// transfer recorded the day-20 parcel as the one that left. Replayed by
// arrival, that is also what the queue's head held; replayed by acquisition,
// the head is the day-2 parcel, so the departing leg gave away a parcel the
// record does not mention while the destination went on holding the one it
// does — the SAME lot on both accounts, the other one gone, and 200 000 minor
// units of basis conjured out of nothing (50 % of what the family had spent).
//
// The test therefore asserts the family, not just an account: what the two
// accounts hold together must be what was actually paid.
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

// TestTransferOutTakesOnlyPartOfTheLotItRecorded is the partial case of the
// same rule: a breakdown that names half of a lot leaves the other half where
// it was, dated as it was, while a parcel standing AHEAD of it in the queue is
// not touched at all. Releasing by the queue instead would empty the head first
// and never reach the lot the record names.
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

// TestTransferOutWithoutBreakdownReleasesByTheQueue pins the other half of the
// rule, and it is not a leftover: a transfer whose basis was typed in by hand,
// or written down before breakdowns were kept, records NOTHING about which lots
// went. There is nothing to honour for it, so the queue decides — which is a
// legitimate answer for such a transfer and not a fallback to be closed off.
//
// The fixture is the one above with the record removed, so the two tests
// differ in exactly one thing: with a breakdown the day-20 parcel leaves,
// without one the head of the queue does.
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

// TestTransferOutRefusesAParcelTheAccountDoesNotHold is the loud half of the
// matching rule. A piece is matched to a lot by the DAY IT WAS ACQUIRED, and
// when the replayed journal holds no such shares at all, the record and the
// history contradict each other: the source was edited after the transfer, or
// the shares were released twice. Every quiet answer is worse than saying so —
// taking the quantity off some other day's lot re-dates shares that are still
// held and reprices them at a rate from a day they were never bought on, and
// taking nothing leaves the family holding one parcel's basis twice.
func TestTransferOutRefusesAParcelTheAccountDoesNotHold(t *testing.T) {
	for name, ops := range map[string][]portfolio.Operation{
		"no lot was ever acquired on that day": {
			op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
			transferOut(22, "10", 300_000, piece("10", 300_000, 5)),
		},
		// The account holds enough shares overall — so this is not an oversell
		// and no total would notice — but a sale has since eaten into the very
		// parcel the record says departed.
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

// checkNamesBothCausesAndTheWayOut pins what the owner is actually told when a
// recorded parcel cannot be found.
//
// The message used to state ONE cause as a fact — "its history was edited after
// the transfer was recorded" — and that is the cause it almost never is. Every
// write path replays the journal before storing anything (see
// operation.Service), so rows written by this build cannot reach this refusal
// through the API at all; what reaches it is a journal written under an earlier
// queue rule, where nobody edited anything. The owner got a blank positions
// screen, an account they could no longer write to, and an accusation about an
// edit that never happened, with no way out named.
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

// TestTransferOutRefusesAParcelAnEarlierQueueRuleRecorded is the refusal above
// on data nobody touched — the only way it is actually reachable.
//
// The journal is one an older build wrote: the queue was ordered by ARRIVAL
// then, so the sale on day 22 took the parcel bought on day 20 (which was on
// the account first) and left the day-2 parcel for the transfer to record.
// Today the queue is ordered by ACQUISITION, so the same sale takes the day-2
// parcel instead and the recorded one is not there to be given up. No edit, no
// second release — just a rule that moved under a frozen record, which is issue
// #60 seen from the other side.
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

// TestTransferOutRefusesToMoveMoreThanTheAccountHolds pins the oversell check
// that runs before any piece is matched. Without it the shortfall still gets
// caught — the pieces run out of lots to come from — but as "your record and
// your history disagree, delete the transfer and record it again", which is the
// wrong thing to tell someone whose account simply never held that many shares.
func TestTransferOutRefusesToMoveMoreThanTheAccountHolds(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "", -300_000, 0),
		transferOut(22, "15", 300_000, piece("15", 300_000, 20)),
	}
	if _, err := portfolio.Compute(ops); !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell: 15 units cannot leave an account holding 10", err)
	}
}

// TestTransferOutRefusesABreakdownCarryingBasisTheAccountDoesNotHold pins the
// other guard: the cost a breakdown could not take out of the lots of its own
// dates is drained from the front of the queue, and there has to be that much
// money there. Without the check the drain takes what it finds, the position's
// basis goes NEGATIVE, and nothing says a word — a positions screen showing
// less than nothing invested, from a journal that replays "successfully".
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

// TestTransferOutRefusesABreakdownThatDoesNotAddUp extends the arriving leg's
// guard (see TestTransferInBreakdownMismatchRejected) to the departing one.
// The two legs read ONE set of stored pieces, so pieces that no longer sum to
// the operation carrying them are damage on both sides; and now that the
// departing leg releases those pieces, letting them through would take a
// quantity or a basis out of the source that no operation claims.
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

// TestTransferOutTakesTheBasisOfAShareLessLotItsPieceCarries is the case that
// decides whether the matching rule may be strict about MONEY the way it is
// about shares. It may not — and it also decides which lot the difference
// comes out of.
//
// A reverse split deep enough to round a lot's whole holding away leaves it
// with no shares and its cost intact (see portfolio.Lot). A release consumes
// such a lot as a piece of nothing, and operation.quantizeLots — the table
// cannot store a piece with no quantity — folds that piece's cost into the next
// piece along. So a perfectly healthy breakdown, written by this program, can
// name a piece carrying MORE basis than the lot its day points at holds, with
// the remainder sitting in a SHARELESS LOT AHEAD OF IT.
//
// The fixture is that, with one lot more: the shareless lot bought on day 1,
// the lot the piece is dated by on day 2, and a THIRD lot bought on day 2 as
// well that the transfer never touched. Two ways of getting it wrong are ruled
// out at once. Refusing the piece because its own lot holds less money than it
// names would refuse a transfer CreateTransfer itself wrote. And taking the
// difference off the untouched day-2 lot — the nearest lot that shares the
// piece's date — would leave the departed shareless lot still sitting there
// holding 30000 the destination now holds too, while an innocent parcel quietly
// lost the same amount of its own basis.
func TestTransferOutTakesTheBasisOfAShareLessLotItsPieceCarries(t *testing.T) {
	// 3e-11 rounds the day-1 lot's 3 units away entirely and leaves each day-2
	// lot with 3e-10 — the running-total allocation applySplit uses.
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.00000000003")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -30_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -100_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -900_000, 0),
		split,
		// What CreateTransfer records for moving the first two lots: one
		// storable piece, dated by the first lot that still has shares,
		// carrying the shareless lot's 30000 as well.
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

// TestTransferOutTakesAShareLessLotsBasisWhenItsPieceTakesOnlyPartOfALot is the
// test above with one thing changed: the piece takes the lot it is dated by IN
// PART instead of whole. That difference is the whole point.
//
// A piece may carry more basis than the lot of its date holds, because
// operation.quantizeLots folded a shareless lot's money into it (see the test
// above). Taking "whatever the lot still holds" for such a piece answers
// correctly only while the piece empties that lot: the moment it takes a
// fraction, the lot's whole cost exceeds what the piece asks for, so the clamp
// never binds, nothing is carried, and the shareless lot's 30000 comes quietly
// out of the fraction's own parcel — which had nothing to do with the transfer —
// while the shareless lot stays on the account holding 30000 the destination
// holds too.
//
// Nothing sums wrong when that happens. The account gives up the 330000 its
// record names, the family holds the 930000 it paid, and both totals agree with
// themselves; only the parcels are wrong, and every figure struck at a lot's own
// date afterwards is wrong with them. The fixture is the reviewer's: 3 units on
// day 1 for 30000, 10 on day 2 for 900000, a reverse split that rounds the day-1
// lot's shares away, and a third of what is left departing.
func TestTransferOutTakesAShareLessLotsBasisWhenItsPieceTakesOnlyPartOfALot(t *testing.T) {
	// 3e-11 leaves the day-1 lot with no shares and the day-2 lot with 3e-10.
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.00000000003")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -30_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "", -900_000, 0),
		split,
		// What CreateTransfer records for moving a third of the position: the
		// shareless lot's whole 30000 plus a third of the day-2 lot's 900000,
		// in the single storable piece its date is taken from.
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

// TestTransferOutMatchesPiecesToLotsOfTheSameDay covers two lots bought on ONE
// day — the tie the queue breaks by journal order (see addLot). The breakdown
// then holds two pieces with the same date, and each must find its own lot
// rather than both draining the first: the account moved 15 of its 20 shares
// and must be left with 5 and the basis that goes with them.
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

// TestBackdatedSplitLeavesTheSourceWithSharesAndNoBasis pins the one skew
// honouring the record cannot undo, so that the README's description of it and
// the engine's behaviour cannot drift apart.
//
// A split entered AFTER a transfer but dated BEFORE it doubles the lot the
// breakdown points at. The recorded basis is still honoured in full — that is
// the point of reading the release off the record — so it comes off twice the
// shares it was struck against, and the source keeps shares carrying none of it.
// The family holds exactly what it paid and the money is where the record says
// it went, which is why this is accepted rather than refused; it is simply all
// on one side. Deleting the transfer, recording the split and recording the
// transfer again is the way to even it out.
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
