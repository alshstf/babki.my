package portfolio_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/portfolio"
)

// exchangeLegs builds the pair the service would write for converting `from`
// units of instrument a into `to` units of instrument b, using the pieces the
// caller names. It exists so a test states the BREAKDOWN it is exercising and
// nothing else; the service's own construction of that breakdown is tested in
// package operation.
func exchangeLegs(dayN int, a, b *uuid.UUID, from, to string,
	outLots, inLots []portfolio.ReleasedLot,
) (portfolio.Operation, portfolio.Operation) {
	cost := portfolio.LotsCost(outLots)
	out := op(portfolio.TypeExchangeOut, dayN, a, from, "", cost, 0)
	out.TransferLots = outLots
	in := op(portfolio.TypeExchangeIn, dayN, b, to, "", cost, 0)
	in.TransferLots = inLots
	return out, in
}

func lot(qty string, cost int64, dayN int) portfolio.ReleasedLot {
	l := portfolio.ReleasedLot{Quantity: d(qty), CostMinor: cost}
	if dayN > 0 {
		acquired := day(dayN)
		l.AcquiredOn = &acquired
	}
	return l
}

// TestExchangeCarriesBasisAndDatesOntoTheNewPaper is the load-bearing test of
// the pair: a depositary receipt bought on two days becomes the share it
// represented, one for one, and the position that results must be
// indistinguishable — in money and in dates — from the one that was held before,
// except for the paper's identity.
func TestExchangeCarriesBasisAndDatesOntoTheNewPaper(t *testing.T) {
	// 2 receipts at 6414.60 (fee 6.41 capitalized) and 1 at 6567.20.
	buy1 := op(portfolio.TypeBuy, 2, &lkoh, "2", "6414.60", -1_282_920, 641)
	buy2 := op(portfolio.TypeBuy, 3, &lkoh, "1", "6567.20", -656_720, 0)
	pieces := []portfolio.ReleasedLot{lot("2", 1_283_561, 2), lot("1", 656_720, 3)}
	out, in := exchangeLegs(5, &lkoh, &sber, "3", "3", pieces, pieces)

	pos, err := portfolio.Compute([]portfolio.Operation{buy1, buy2, out, in})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	old := pos[lkoh]
	if !old.Quantity.IsZero() || old.CostMinor != 0 {
		t.Errorf("the converted paper still holds qty=%s cost=%d, want nothing left", old.Quantity, old.CostMinor)
	}
	if realizedOf(t, old) != 0 {
		t.Errorf("the conversion realized %d, want 0: nothing was sold", realizedOf(t, old))
	}

	got := pos[sber]
	if got == nil {
		t.Fatal("no position in the paper received")
	}
	if !got.Quantity.Equal(d("3")) {
		t.Errorf("qty = %s, want 3", got.Quantity)
	}
	// 1282920 + 641 + 656720, to the minor unit: a conversion is not a purchase
	// and not a sale, so the money is exactly what was paid for the receipts.
	if got.CostMinor != 1_940_281 {
		t.Errorf("cost = %d, want 1940281", got.CostMinor)
	}
	if realizedOf(t, got) != 0 {
		t.Errorf("realized = %d, want 0", realizedOf(t, got))
	}
	if len(got.Lots) != 2 {
		t.Fatalf("lots = %d, want 2 — one per purchase", len(got.Lots))
	}
	// THE DATES ARE THE POINT. НК РФ ст. 219.1 counts the holding period from
	// the day the receipt was bought, and every ruble figure downstream is
	// struck at the rate of a lot's own acquisition day.
	if got.Lots[0].AcquiredOn == nil || !got.Lots[0].AcquiredOn.Equal(day(2)) {
		t.Errorf("first lot acquired %v, want %v", got.Lots[0].AcquiredOn, day(2))
	}
	if got.Lots[1].AcquiredOn == nil || !got.Lots[1].AcquiredOn.Equal(day(3)) {
		t.Errorf("second lot acquired %v, want %v", got.Lots[1].AcquiredOn, day(3))
	}
	if got.Lots[0].CostMinor != 1_283_561 || got.Lots[1].CostMinor != 656_720 {
		t.Errorf("lot costs = %d/%d, want 1283561/656720", got.Lots[0].CostMinor, got.Lots[1].CostMinor)
	}
}

// TestExchangeRestatesQuantityWithoutRestatingCost is the case a wrong
// implementation is likeliest to produce: scaling the money along with the
// units. A 1-for-10 conversion that did so would hand the owner a nine-tenths
// loss the law says did not happen.
func TestExchangeRestatesQuantityWithoutRestatingCost(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "10", "500", -5_000_000, 0)
	outLots := []portfolio.ReleasedLot{lot("10", 5_000_000, 2)}
	inLots := []portfolio.ReleasedLot{lot("1", 5_000_000, 2)}
	out, in := exchangeLegs(5, &lkoh, &sber, "10", "1", outLots, inLots)

	pos, err := portfolio.Compute([]portfolio.Operation{buy, out, in})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	got := pos[sber]
	if !got.Quantity.Equal(d("1")) {
		t.Errorf("qty = %s, want 1", got.Quantity)
	}
	if got.CostMinor != 5_000_000 {
		t.Errorf("cost = %d, want 5000000 — a conversion moves no money", got.CostMinor)
	}
	// And the profit on selling it afterwards is measured against the ORIGINAL
	// outlay, which is the figure a tax return needs.
	sell := op(portfolio.TypeSell, 6, &sber, "1", "6000", 6_000_000, 0)
	pos, err = portfolio.Compute([]portfolio.Operation{buy, out, in, sell})
	if err != nil {
		t.Fatalf("Compute with sale: %v", err)
	}
	if realizedOf(t, pos[sber]) != 1_000_000 {
		t.Errorf("realized = %d, want 1000000", realizedOf(t, pos[sber]))
	}
}

// TestExchangeKeepsAnUndatedParcelUndated: shares that reached the account by a
// transfer carrying no dates cannot gain one by being converted, and the
// position that results must still refuse to be valued in another currency.
func TestExchangeKeepsAnUndatedParcelUndated(t *testing.T) {
	arrive := op(portfolio.TypeTransferIn, 2, &lkoh, "5", "", 500_000, 0)
	pieces := []portfolio.ReleasedLot{lot("5", 500_000, 0)}
	inPieces := []portfolio.ReleasedLot{lot("50", 500_000, 0)}
	out, in := exchangeLegs(5, &lkoh, &sber, "5", "50", pieces, inPieces)

	pos, err := portfolio.Compute([]portfolio.Operation{arrive, out, in})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	got := pos[sber]
	if len(got.Lots) != 1 {
		t.Fatalf("lots = %d, want 1", len(got.Lots))
	}
	if got.Lots[0].AcquiredOn != nil {
		t.Errorf("acquired on %v, want unknown — nothing recorded the purchase day",
			got.Lots[0].AcquiredOn)
	}
	if got.CostMinor != 500_000 || !got.Quantity.Equal(d("50")) {
		t.Errorf("pos = %s units / %d minor, want 50 / 500000", got.Quantity, got.CostMinor)
	}
}

// TestExchangeOutReleasesTheRecordedLotsNotAFreshSlice: the departing leg must
// give up the parcel its breakdown names even when the queue, replayed today,
// would pick another. This is the property issue #60 established for transfers,
// asked of the new pair.
func TestExchangeOutReleasesTheRecordedLotsNotAFreshSlice(t *testing.T) {
	// Two lots: day 2 (cheap) and day 3 (dear). The breakdown names the DAY 3
	// one, which is not what a fresh FIFO release would take.
	buy1 := op(portfolio.TypeBuy, 2, &lkoh, "1", "100", -100_000, 0)
	buy2 := op(portfolio.TypeBuy, 3, &lkoh, "1", "900", -900_000, 0)
	pieces := []portfolio.ReleasedLot{lot("1", 900_000, 3)}
	out, in := exchangeLegs(5, &lkoh, &sber, "1", "1", pieces, pieces)

	pos, err := portfolio.Compute([]portfolio.Operation{buy1, buy2, out, in})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	left := pos[lkoh]
	if left.CostMinor != 100_000 {
		t.Errorf("the old paper kept %d, want 100000 — the dear lot is the one that left", left.CostMinor)
	}
	if len(left.Lots) != 1 || left.Lots[0].AcquiredOn == nil || !left.Lots[0].AcquiredOn.Equal(day(2)) {
		t.Errorf("remaining lot = %+v, want the day-2 one", left.Lots)
	}
	if pos[sber].CostMinor != 900_000 {
		t.Errorf("the new paper arrived with %d, want 900000", pos[sber].CostMinor)
	}
}

// TestExchangeLegWithoutABreakdownIsRefused: a transfer may legitimately carry
// a hand-given basis and no pieces; a conversion may not, because its arriving
// leg is built from the departing one's pieces and nothing else can supply
// them.
func TestExchangeLegWithoutABreakdownIsRefused(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "10", "100", -1_000_000, 0)
	for _, tc := range []struct {
		name string
		ops  []portfolio.Operation
	}{
		{"departing leg", []portfolio.Operation{
			buy,
			op(portfolio.TypeExchangeOut, 5, &lkoh, "10", "", 1_000_000, 0),
		}},
		{"arriving leg", []portfolio.Operation{
			op(portfolio.TypeExchangeIn, 5, &sber, "10", "", 1_000_000, 0),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := portfolio.Compute(tc.ops)
			if err == nil {
				t.Fatal("Compute accepted a conversion leg with no breakdown")
			}
			if !strings.Contains(err.Error(), "must carry the breakdown") {
				t.Errorf("err = %v, want it to name the missing breakdown", err)
			}
		})
	}
}

// TestExchangeBreakdownMustSumToItsOwnLeg is what makes the in-leg quantity a
// checked figure rather than a claim: the two legs carry DIFFERENT pieces, each
// summing to the quantity of the row it rides on, and a disagreement is refused
// on every read.
func TestExchangeBreakdownMustSumToItsOwnLeg(t *testing.T) {
	buy := op(portfolio.TypeBuy, 2, &lkoh, "10", "100", -1_000_000, 0)
	outLots := []portfolio.ReleasedLot{lot("10", 1_000_000, 2)}
	// The arriving leg claims 100 units while its pieces sum to 99.
	inLots := []portfolio.ReleasedLot{lot("99", 1_000_000, 2)}
	out, in := exchangeLegs(5, &lkoh, &sber, "10", "100", outLots, inLots)

	_, err := portfolio.Compute([]portfolio.Operation{buy, out, in})
	if err == nil {
		t.Fatal("Compute accepted an arriving leg whose pieces do not sum to its quantity")
	}
	if !strings.Contains(err.Error(), "sum to quantity") {
		t.Errorf("err = %v, want the sum mismatch named", err)
	}
}

// TestExchangeConservesBasisOverRandomJournals asserts the VALUE the pair is
// built to preserve — the money the family has spent — rather than the
// agreement of two code paths, which is the shape this package has watched go
// green while both paths were wrong together.
func TestExchangeConservesBasisOverRandomJournals(t *testing.T) {
	rng := rand.New(rand.NewSource(20260823))
	for i := 0; i < 300; i++ {
		var ops []portfolio.Operation
		var spent int64
		lots := []portfolio.ReleasedLot{}
		held := decimal.Zero
		nBuys := 1 + rng.Intn(4)
		for b := 0; b < nBuys; b++ {
			qty := decimal.NewFromInt(int64(1 + rng.Intn(50)))
			amount := int64(1+rng.Intn(500_000)) * -1
			day := 2 + b
			ops = append(ops, op(portfolio.TypeBuy, day, &lkoh,
				qty.String(), "", amount, 0))
			spent += -amount
			lots = append(lots, lot(qty.String(), -amount, day))
			held = held.Add(qty)
		}
		// Convert the whole holding at a ratio the corporate action names.
		from, to := held, held.Mul(decimal.NewFromInt(int64(1+rng.Intn(20))))
		inLots := make([]portfolio.ReleasedLot, 0, len(lots))
		for _, pc := range lots {
			inLots = append(inLots, portfolio.ReleasedLot{
				Quantity:   pc.Quantity.Mul(to).Div(from),
				CostMinor:  pc.CostMinor,
				AcquiredOn: pc.AcquiredOn,
			})
		}
		out, in := exchangeLegs(2+nBuys, &lkoh, &sber, from.String(), to.String(), lots, inLots)
		pos, err := portfolio.Compute(append(ops, out, in))
		if err != nil {
			t.Fatalf("case %d: Compute: %v", i, err)
		}
		if got := pos[sber].CostMinor + pos[lkoh].CostMinor; got != spent {
			t.Fatalf("case %d: basis after conversion = %d, want the %d actually spent", i, got, spent)
		}
		if !pos[sber].Quantity.Equal(to) {
			t.Fatalf("case %d: qty = %s, want %s", i, pos[sber].Quantity, to)
		}
		if realizedOf(t, pos[sber]) != 0 || realizedOf(t, pos[lkoh]) != 0 {
			t.Fatalf("case %d: a conversion realized a result", i)
		}
	}
}

// TestExchangeIsNotCash pins the other half of what these types mean: their
// amount is a cost basis, and a reader that sums the journal as money must skip
// them — both of them, on the SAME account, which is the shape no transfer has.
func TestExchangeIsNotCash(t *testing.T) {
	for _, typ := range []portfolio.Type{portfolio.TypeExchangeOut, portfolio.TypeExchangeIn} {
		if portfolio.MovesCash(portfolio.Operation{Type: typ}) {
			t.Errorf("%s counts as cash", typ)
		}
	}
	if !portfolio.MovesCash(portfolio.Operation{Type: portfolio.TypeDividend}) {
		t.Error("a dividend does not count as cash")
	}
	buy := op(portfolio.TypeBuy, 2, &lkoh, "10", "100", -1_000_000, 0)
	deposit := op(portfolio.TypeDeposit, 1, nil, "", "", 1_000_000, 0)
	pieces := []portfolio.ReleasedLot{lot("10", 1_000_000, 2)}
	out, in := exchangeLegs(5, &lkoh, &sber, "10", "10", pieces, pieces)
	cash, err := portfolio.Cash([]portfolio.Operation{deposit, buy, out, in})
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	// 1 000 000 in, 1 000 000 spent: the conversion moved nothing either way.
	if got := cash["RUB"]; got != nil && got.Minor != 0 {
		t.Errorf("balance = %d, want 0", got.Minor)
	}
}
