package portfolio_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/portfolio"
)

// spinoffLegs builds the pair the registry would write when a share of one
// paper's basis is carved out onto another: the departing leg names the lots
// and the money each gives up and carries NO quantity, the arriving leg is an
// ordinary parcel of `to` units of the new paper.
func spinoffLegs(dayN int, a, b *uuid.UUID, to string,
	outLots, inLots []portfolio.ReleasedLot,
) (portfolio.Operation, portfolio.Operation) {
	cost := portfolio.LotsCost(outLots)
	out := op(portfolio.TypeSpinoffOut, dayN, a, "", "", cost, 0)
	out.TransferLots = outLots
	in := op(portfolio.TypeSpinoffIn, dayN, b, to, "", cost, 0)
	in.TransferLots = inLots
	return out, in
}

// TestSpinoffLeavesTheUnitsAndMovesAShareOfTheMoney is the load-bearing test of
// the pair, on the owner's own shape: units bought on two days stay exactly
// where they were, a share of what was paid for them appears on the carved-out
// paper, and the days behind that money are the days it was really spent.
func TestSpinoffLeavesTheUnitsAndMovesAShareOfTheMoney(t *testing.T) {
	// 5 400 units at 0.0957 and 9 668 at 0.1028, in kopecks: 51 678 and 99 387.
	buy1 := op(portfolio.TypeBuy, 2, &lkoh, "5400", "0.0957", -51_678, 0)
	buy2 := op(portfolio.TypeBuy, 3, &lkoh, "9668", "0.1028", -99_387, 0)

	// A third of the basis moves: floor((51678+99387) x 0.3333333333) = 50 354,
	// allocated 17 225 / 33 129 — the exact largest-remainder split of that
	// figure between the two parcels.
	outLots := []portfolio.ReleasedLot{lot("5400", 17_225, 2), lot("9668", 33_129, 3)}
	inLots := []portfolio.ReleasedLot{lot("5400", 17_225, 2), lot("9668", 33_129, 3)}
	out, in := spinoffLegs(5, &lkoh, &sber, "15068", outLots, inLots)

	pos, err := portfolio.Compute([]portfolio.Operation{buy1, buy2, out, in})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	old := pos[lkoh]
	if !old.Quantity.Equal(d("15068")) {
		t.Errorf("the original paper holds %s units, want 15068 — a spin-off moves no units at all", old.Quantity)
	}
	if old.CostMinor != 151_065-50_354 {
		t.Errorf("the original paper keeps %d minor of basis, want %d", old.CostMinor, 151_065-50_354)
	}
	if realizedOf(t, old) != 0 {
		t.Errorf("the spin-off realized %d, want 0 — nothing was sold (НК РФ ст. 277 п. 7)", realizedOf(t, old))
	}

	carved := pos[sber]
	if !carved.Quantity.Equal(d("15068")) {
		t.Errorf("the carved-out paper holds %s units, want 15068", carved.Quantity)
	}
	if carved.CostMinor != 50_354 {
		t.Errorf("the carved-out paper carries %d minor of basis, want 50354", carved.CostMinor)
	}
	if len(carved.Lots) != 2 {
		t.Fatalf("the carved-out paper has %d parcels, want 2 — one per parcel of the original", len(carved.Lots))
	}
	// THE DATES ARE THE ORIGINAL PURCHASES', not the day the new paper
	// appeared. This is the assertion the tax rule turns on: the holding period
	// and the ruble basis are both struck at the day the money was really spent.
	if !sameAcquisition(carved.Lots[0].AcquiredOn, dayp(2)) {
		t.Errorf("first carved parcel acquired %s, want 2026-07-02", acquired(carved.Lots[0].AcquiredOn))
	}
	if !sameAcquisition(carved.Lots[1].AcquiredOn, dayp(3)) {
		t.Errorf("second carved parcel acquired %s, want 2026-07-03", acquired(carved.Lots[1].AcquiredOn))
	}

	// The family holds exactly what it paid, split across two papers.
	if total := old.CostMinor + carved.CostMinor; total != 151_065 {
		t.Errorf("the two papers hold %d minor between them, want the 151065 that was paid", total)
	}
}

// TestSpinoffPiecesAllocateTheWholeAndNothingMore is the allocation on its own,
// at the scale where a per-lot rounding would show: eleven equal parcels of a
// basis that does not divide, where flooring each lot on its own loses minor
// units and rounding each up invents them.
func TestSpinoffPiecesAllocateTheWholeAndNothingMore(t *testing.T) {
	lots := make([]portfolio.Lot, 11)
	for i := range lots {
		acquired := day(i + 1)
		lots[i] = portfolio.Lot{Quantity: d("1"), CostMinor: 100, AcquiredOn: &acquired}
	}
	// 1100 x 0.115 = 126.5, floored to 126. Each lot's ideal share is
	// 11.4545…, so flooring per lot would place 11 x 11 = 121 and lose five.
	pieces := portfolio.SpinoffPieces(lots, d("0.115"))

	if len(pieces) != 11 {
		t.Fatalf("got %d pieces, want one per parcel", len(pieces))
	}
	if got := portfolio.LotsCost(pieces); got != 126 {
		t.Errorf("the pieces move %d minor, want exactly 126 — floor(1100 x 0.115), placed whole", got)
	}
	// Largest remainders, earliest parcel first among equals: every remainder
	// here is identical, so the five extra units go to the first five parcels.
	for i, pc := range pieces {
		want := int64(11)
		if i < 5 {
			want = 12
		}
		if pc.CostMinor != want {
			t.Errorf("parcel %d gives up %d, want %d", i, pc.CostMinor, want)
		}
		if !pc.Quantity.Equal(d("1")) {
			t.Errorf("parcel %d names %s units, want its own 1 — the piece carries the lot's identity", i, pc.Quantity)
		}
	}
}

// TestSpinoffPiecesNameEveryParcelIncludingTheEmptyOnes: the record is a
// photograph of the lot list, so a parcel that gives up nothing is still in it.
// Without that, applySpinoffOut could not tell a journal that has grown a
// parcel from one that had a penniless parcel all along.
func TestSpinoffPiecesNameEveryParcelIncludingTheEmptyOnes(t *testing.T) {
	free := day(1)
	paid := day(2)
	lots := []portfolio.Lot{
		{Quantity: d("10"), CostMinor: 0, AcquiredOn: &free},
		{Quantity: d("10"), CostMinor: 1000, AcquiredOn: &paid},
	}
	pieces := portfolio.SpinoffPieces(lots, d("0.5"))

	if len(pieces) != 2 {
		t.Fatalf("got %d pieces, want one per parcel including the one bought for nothing", len(pieces))
	}
	if pieces[0].CostMinor != 0 {
		t.Errorf("the parcel bought for nothing gives up %d, want 0", pieces[0].CostMinor)
	}
	if pieces[1].CostMinor != 500 {
		t.Errorf("the paid parcel gives up %d, want 500", pieces[1].CostMinor)
	}
}

// TestSpinoffRefusesWhenTheJournalGrewAParcelUnderneathIt: the record names the
// whole position, so a purchase inserted before the spin-off's own date makes
// the allocation describe a position that no longer exists. Re-allocating
// quietly would take money out of a parcel the record never touched.
func TestSpinoffRefusesWhenTheJournalGrewAParcelUnderneathIt(t *testing.T) {
	buy1 := op(portfolio.TypeBuy, 2, &lkoh, "100", "10", -100_000, 0)
	backdated := op(portfolio.TypeBuy, 3, &lkoh, "50", "10", -50_000, 0)
	outLots := []portfolio.ReleasedLot{lot("100", 40_000, 2)}
	out, in := spinoffLegs(5, &lkoh, &sber, "100", outLots, outLots)

	_, err := portfolio.Compute([]portfolio.Operation{buy1, backdated, out, in})
	if err == nil {
		t.Fatal("a spin-off struck against one parcel was folded over a position that now holds two")
	}
	if !strings.Contains(err.Error(), "struck against 1 parcels") {
		t.Errorf("error = %v, want it to name how many parcels the record and the replay disagree about", err)
	}
	if !strings.Contains(err.Error(), "delete the transfer and record it again") {
		t.Errorf("error = %v, want it to name the way out", err)
	}
}

// TestSpinoffRefusesWhenAParcelChangedUnderneathIt: same list, same days, but a
// parcel is a different size than the one the allocation was struck against —
// a split folded before it, say. The proportion the record carries is no longer
// the proportion of anything.
func TestSpinoffRefusesWhenAParcelChangedUnderneathIt(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "100", "10", -100_000, 0)
	split := op(portfolio.TypeSplit, 3, &lkoh, "", "", 0, 0)
	split.SplitRatio = dp("10")
	outLots := []portfolio.ReleasedLot{lot("100", 40_000, 2)}
	out, in := spinoffLegs(5, &lkoh, &sber, "100", outLots, outLots)

	_, err := portfolio.Compute([]portfolio.Operation{buy, split, out, in})
	if err == nil {
		t.Fatal("a spin-off struck against 100 units was folded over a parcel of 1000")
	}
	if !strings.Contains(err.Error(), "names 100 units acquired") {
		t.Errorf("error = %v, want it to name the units the record claims and the units replaying leaves", err)
	}
}

// TestSpinoffRefusesMovingMoreThanAParcelHolds guards the direction a bug would
// take money in: a piece bigger than its parcel would drive a lot's basis
// negative and the position's with it.
func TestSpinoffRefusesMovingMoreThanAParcelHolds(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "100", "10", -100_000, 0)
	outLots := []portfolio.ReleasedLot{lot("100", 150_000, 2)}
	out, in := spinoffLegs(5, &lkoh, &sber, "100", outLots, outLots)

	_, err := portfolio.Compute([]portfolio.Operation{buy, out, in})
	if err == nil {
		t.Fatal("a spin-off moved more basis than the parcel it names holds")
	}
	if !strings.Contains(err.Error(), "moves 150000 minor of basis out of a parcel") {
		t.Errorf("error = %v, want it to name both figures", err)
	}
}

// TestSpinoffOutRefusesAQuantity: the field is empty because the event moves no
// units, and a count in it would be rendered as units leaving on every screen
// that draws a journal.
func TestSpinoffOutRefusesAQuantity(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "100", "10", -100_000, 0)
	outLots := []portfolio.ReleasedLot{lot("100", 40_000, 2)}
	out, in := spinoffLegs(5, &lkoh, &sber, "100", outLots, outLots)
	out.Quantity = dp("100")

	_, err := portfolio.Compute([]portfolio.Operation{buy, out, in})
	if err == nil {
		t.Fatal("a spin-off carrying a quantity was folded")
	}
	if !strings.Contains(err.Error(), "moves no units") {
		t.Errorf("error = %v, want it to say the event moves no units", err)
	}
}

// TestSpinoffBreakdownMustSumToTheBasisItCarries: the two figures on the row
// are one fact written twice, and a row where they disagree is damage.
func TestSpinoffBreakdownMustSumToTheBasisItCarries(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "100", "10", -100_000, 0)
	outLots := []portfolio.ReleasedLot{lot("100", 40_000, 2)}
	out, in := spinoffLegs(5, &lkoh, &sber, "100", outLots, outLots)
	out.AmountMinor = 39_000

	_, err := portfolio.Compute([]portfolio.Operation{buy, out, in})
	if err == nil {
		t.Fatal("a spin-off whose pieces do not add up to its own basis was folded")
	}
	if !strings.Contains(err.Error(), "sum to cost 40000, but the operation moves 39000") {
		t.Errorf("error = %v, want both figures named", err)
	}
}

// TestSpinoffOutOfAParcelWhoseSharesASplitRoundedAway: a reverse split can
// leave a parcel with no units and real money in it (see applySplit). That
// money is as much a part of the paper's cost as any other, so a share of it
// moves — and the piece that names the parcel carries its true count of zero.
func TestSpinoffOutOfAParcelWhoseSharesASplitRoundedAway(t *testing.T) {
	// Two parcels; a reverse split deep enough to round the first away entirely
	// — one unit multiplied by 1e-11 is finer than the ten decimal places the
	// journal keeps — while the second keeps a hundredth of a unit.
	buy1 := op(portfolio.TypeBuy, 2, &lkoh, "1", "1000", -100_000, 0)
	buy2 := op(portfolio.TypeBuy, 3, &lkoh, "1000000000", "0.1", -100_000_000, 0)
	split := op(portfolio.TypeSplit, 4, &lkoh, "", "", 0, 0)
	split.SplitRatio = dp("0.00000000001")

	before, err := portfolio.Compute([]portfolio.Operation{buy1, buy2, split})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	held := before[lkoh]
	if len(held.Lots) != 2 || !held.Lots[0].Quantity.IsZero() || held.Lots[0].CostMinor == 0 {
		t.Fatalf("the fixture did not produce a shareless parcel with money in it: %+v", held.Lots)
	}

	pieces := portfolio.SpinoffPieces(held.Lots, d("0.5"))
	if len(pieces) != 2 {
		t.Fatalf("got %d pieces, want one per parcel including the shareless one", len(pieces))
	}
	if !pieces[0].Quantity.IsZero() {
		t.Errorf("the shareless parcel's piece names %s units, want its true 0", pieces[0].Quantity)
	}
	if pieces[0].CostMinor != 50_000 {
		t.Errorf("the shareless parcel gives up %d, want 50000 — its money is money like any other", pieces[0].CostMinor)
	}
}

// TestSpinoffConservesBasisOverRandomJournals is the invariant, asserted on
// VALUES rather than on two computations agreeing: whatever the journal, the
// money the original paper loses is exactly the money the carved-out paper
// gains, and the two together are exactly what the position held before.
//
// The alternative — comparing the engine's answer against a second
// implementation of the same allocation — would pass just as happily with both
// of them wrong, which is the failure mode this codebase has already met (see
// the package doc on differential tests).
func TestSpinoffConservesBasisOverRandomJournals(t *testing.T) {
	rng := rand.New(rand.NewSource(20260824))
	shares := []string{"0.5", "0.3333333333", "0.115", "0.01", "0.9999999999", "0.73"}

	for run := 0; run < 300; run++ {
		a, b := uuid.New(), uuid.New()
		var journal []portfolio.Operation
		lots := rng.Intn(6) + 1
		for i := 0; i < lots; i++ {
			qty := decimal.NewFromInt(int64(rng.Intn(1000) + 1))
			cost := int64(rng.Intn(1_000_000))
			buy := op(portfolio.TypeBuy, i+1, &a, qty.String(), "1", -cost, 0)
			journal = append(journal, buy)
		}
		positions, err := portfolio.Compute(journal)
		if err != nil {
			t.Fatalf("run %d: Compute: %v", run, err)
		}
		held := positions[a]
		basisBefore := held.CostMinor

		share := d(shares[rng.Intn(len(shares))])
		pieces := portfolio.SpinoffPieces(held.Lots, share)
		moved := portfolio.LotsCost(pieces)

		to := held.Quantity // one for one, the owner's own case
		out := op(portfolio.TypeSpinoffOut, lots+1, &a, "", "", moved, 0)
		out.TransferLots = pieces
		in := op(portfolio.TypeSpinoffIn, lots+1, &b, to.String(), "", moved, 0)
		in.TransferLots = pieces

		after, err := portfolio.Compute(append(journal, out, in))
		if err != nil {
			t.Fatalf("run %d: Compute after the spin-off: %v", run, err)
		}

		left, carved := after[a].CostMinor, after[b].CostMinor
		if left+carved != basisBefore {
			t.Fatalf("run %d: %d minor before, %d left plus %d carved out = %d after",
				run, basisBefore, left, carved, left+carved)
		}
		if carved != moved {
			t.Fatalf("run %d: the pieces move %d and the carved paper carries %d", run, moved, carved)
		}
		if !after[a].Quantity.Equal(held.Quantity) {
			t.Fatalf("run %d: the original paper holds %s after the spin-off, want the %s it held before",
				run, after[a].Quantity, held.Quantity)
		}
		// Never more than the share asks for: the total is floored, so it can
		// fall a minor unit short of the exact proportion but never above it.
		ceiling := decimal.NewFromInt(basisBefore).Mul(share)
		if decimal.NewFromInt(moved).GreaterThan(ceiling) {
			t.Fatalf("run %d: moved %d minor, more than %s of the %d that was paid",
				run, moved, share, basisBefore)
		}
	}
}
