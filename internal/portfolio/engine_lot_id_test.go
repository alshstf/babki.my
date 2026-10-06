package portfolio_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/portfolio"
)

// Two parcels bought on one day, ten for 1 000 ₽ and ten for 9 000 ₽.
func twoParcelsOfOneDay(paper *uuid.UUID) (cheap, dear portfolio.Operation) {
	cheap = opIn(portfolio.TypeBuy, 1, paper, "RUB", "10", -100_000)
	cheap.ID = uuid.New()
	dear = opIn(portfolio.TypeBuy, 1, paper, "RUB", "10", -900_000)
	dear.ID = uuid.New()
	return cheap, dear
}

func lotOf(o portfolio.Operation) portfolio.LotID {
	return portfolio.LotID{Origin: "op/" + o.ID.String(), Seq: 0}
}

// A move takes the very parcel it recorded, not the first of its day: five of
// the dear parcel leave with 4 500 ₽, and the cheap one stays whole (#197).
func TestAMoveTakesTheParcelItNamesNotAnotherOfItsDay(t *testing.T) {
	paper := uuid.New()
	cheap, dear := twoParcelsOfOneDay(&paper)
	move := opIn(portfolio.TypeTransferOut, 5, &paper, "RUB", "5", 450_000)
	move.TransferLots = []portfolio.ReleasedLot{
		{From: lotOf(dear), Quantity: decimal.NewFromInt(5), CostMinor: 450_000, AcquiredOn: dayp(1)},
	}

	pos, err := portfolio.Compute([]portfolio.Operation{cheap, dear, move})
	if err != nil {
		t.Fatal(err)
	}
	p := pos[paper]
	if len(p.Lots) != 2 || p.Lots[0].CostMinor != 100_000 || !p.Lots[0].Quantity.Equal(decimal.NewFromInt(10)) ||
		p.Lots[1].CostMinor != 450_000 || !p.Lots[1].Quantity.Equal(decimal.NewFromInt(5)) {
		t.Errorf("lots = %+v, want the cheap ten whole and five of the dear left with 4 500 ₽", p.Lots)
	}
}

// A move whose parcel the history no longer holds, or whose parcel is now
// dated otherwise, is refused rather than taken from another.
func TestAMoveWhoseParcelChangedIsRefused(t *testing.T) {
	paper := uuid.New()
	cheap, dear := twoParcelsOfOneDay(&paper)
	for name, piece := range map[string]portfolio.ReleasedLot{
		"gone":         {From: portfolio.LotID{Origin: "op/" + uuid.NewString()}, Quantity: decimal.NewFromInt(5), CostMinor: 450_000, AcquiredOn: dayp(1)},
		"another day":  {From: lotOf(dear), Quantity: decimal.NewFromInt(5), CostMinor: 450_000, AcquiredOn: dayp(2)},
		"less money":   {From: lotOf(dear), Quantity: decimal.NewFromInt(5), CostMinor: 50_000, AcquiredOn: dayp(1)},
		"more than it": {From: lotOf(cheap), Quantity: decimal.NewFromInt(11), CostMinor: 100_000, AcquiredOn: dayp(1)},
	} {
		move := opIn(portfolio.TypeTransferOut, 5, &paper, "RUB", piece.Quantity.String(), piece.CostMinor)
		move.TransferLots = []portfolio.ReleasedLot{piece}
		if _, err := portfolio.Compute([]portfolio.Operation{cheap, dear, move}); !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

// A release reports the parcel each piece came from, so a move written from it
// names its lots.
func TestAReleaseNamesItsParcels(t *testing.T) {
	paper := uuid.New()
	cheap, dear := twoParcelsOfOneDay(&paper)
	pieces, err := portfolio.ReleasedLots([]portfolio.Operation{cheap, dear}, paper, decimal.NewFromInt(15))
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 2 || pieces[0].From != lotOf(cheap) || pieces[1].From != lotOf(dear) {
		t.Errorf("pieces = %+v, want the cheap parcel's then the dear one's", pieces)
	}
	imported := cheap
	ext := "rec-1"
	imported.Source, imported.ExternalID = "tinvest", &ext
	pieces, err = portfolio.ReleasedLots([]portfolio.Operation{imported}, paper, decimal.NewFromInt(1))
	if err != nil || len(pieces) != 1 || pieces[0].From != (portfolio.LotID{Origin: "tinvest/rec-1"}) {
		t.Errorf("an imported parcel's number = %+v (%v), want it named by its record", pieces, err)
	}
}

// A spin-off takes each piece's basis from the parcel it names; a parcel the
// record does not name keeps its basis.
func TestASpinoffTakesBasisFromTheParcelsItNames(t *testing.T) {
	paper := uuid.New()
	cheap, dear := twoParcelsOfOneDay(&paper)
	spin := opIn(portfolio.TypeSpinoffOut, 3, &paper, "RUB", "", 90_000)
	spin.Quantity = nil
	spin.TransferLots = []portfolio.ReleasedLot{
		{From: lotOf(dear), Quantity: decimal.NewFromInt(10), CostMinor: 90_000, AcquiredOn: dayp(1)},
	}
	pos, err := portfolio.Compute([]portfolio.Operation{cheap, dear, spin})
	if err != nil {
		t.Fatal(err)
	}
	if p := pos[paper]; p.Lots[0].CostMinor != 100_000 || p.Lots[1].CostMinor != 810_000 {
		t.Errorf("lots = %+v, want the cheap parcel untouched and 900 ₽ out of the dear one", p.Lots)
	}
}
