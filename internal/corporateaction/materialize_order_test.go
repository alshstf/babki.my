package corporateaction_test

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// One share, split 10:1 in 2021, five sold in 2022, split 2:1 in 2024: ten
// held. The second event sees the trades between the two (#187).
func TestASecondEventSeesTheTradesBetweenItAndTheFirst(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2020-01-02", "1", -100_000)
	f.splitEvent(t, "2021-06-01", 1, 10)
	f.splitEvent(t, "2024-06-10", 1, 2)

	// The sale only fits once the first split is in the journal.
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.sell(t, f.accountID, "2022-01-03", "5", 60_000)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("10"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s — (1 x 10 - 5) x 2", got, want)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 2 {
		t.Errorf("got %d registry rows, want both splits", len(rows))
	}
}

// A journal broken on one account does not stop the others' rows, and the
// failure is reported.
func TestOneAccountThatDoesNotReplayDoesNotStopTheOthers(t *testing.T) {
	f := newFixture(t)
	// Broken behind the service's back: a sale of a paper never bought.
	if _, err := f.ops.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.amazonID, Type: operation.TypeSell,
		OccurredOn: date("2021-05-04"), Quantity: dec("3"), AmountMinor: 900_000, Currency: "USD",
	}, nil); err != nil {
		t.Fatalf("stage the broken journal: %v", err)
	}
	f.buy(t, f.otherID, "2021-05-04", "1", -323_000)
	f.splitEvent(t, "2022-06-06", 1, 20)

	_, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err == nil {
		t.Error("ForISIN reported nothing about the account whose journal does not replay")
	}
	if got, want := f.held(t, f.otherID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("the healthy account holds %s, want %s — another account's broken journal is not its problem", got, want)
	}
}

// Registry rows fold at the start of their day, so a spin-off divides the
// parcels held that morning (#189).
func TestASpinoffOnADayTheAccountAlsoBoughtIsStruckAgainstTheMorning(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "TECH2")
	f.buy(t, f.accountID, "2020-12-30", "100", -100_000)
	f.buy(t, f.accountID, "2023-12-22", "10", -90_000)
	f.spinoffEvent(t, "2023-12-22", 1, 1, "0.25")

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Refused != 0 {
		t.Fatalf("stats = %+v, want the pair written", stats)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil {
		t.Fatal("the carved-out paper is not held at all")
	}
	if got.CostMinor != 25_000 || got.Quantity.String() != "100" {
		t.Errorf("the carved-out paper is %s units costing %d, want 100 costing 25000 — the morning's holding, not the day's purchase",
			got.Quantity, got.CostMinor)
	}
	old := f.position(t, f.accountID, f.amazonID)
	if old == nil || old.CostMinor != 165_000 || old.Quantity.String() != "110" {
		t.Errorf("the original is %+v, want 110 units costing 165000", old)
	}
}

// The same day with a sale: the sale folds after the spin-off.
func TestASpinoffOnADayTheAccountAlsoSoldIsStruckAgainstTheMorning(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "TECH2")
	f.buy(t, f.accountID, "2020-12-30", "100", -100_000)
	f.sell(t, f.accountID, "2023-12-22", "40", 60_000)
	f.spinoffEvent(t, "2023-12-22", 1, 1, "0.25")

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Refused != 0 {
		t.Fatalf("stats = %+v, want the pair written", stats)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil || got.CostMinor != 25_000 || got.Quantity.String() != "100" {
		t.Errorf("the carved-out paper is %+v, want 100 units costing 25000", got)
	}
	// 75 000 of basis left on 100 units; 40 sold take 30 000 of it.
	old := f.position(t, f.accountID, f.amazonID)
	if old == nil || old.CostMinor != 45_000 || old.Quantity.String() != "60" {
		t.Errorf("the original is %+v, want 60 units costing 45000", old)
	}
}

// Р-16 through the registry: a share of 0 gives the new paper no cost.
func TestASpinoffWithNoShareGivesTheNewPaperNoCost(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "TECH2")
	f.buy(t, f.accountID, "2020-12-30", "100", -100_000)
	f.spinoffEvent(t, "2023-12-22", 1, 1, "0")

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Refused != 0 {
		t.Fatalf("stats = %+v, want the pair written", stats)
	}
	if got := f.position(t, f.accountID, produced); got == nil || got.CostMinor != 0 || got.Quantity.String() != "100" {
		t.Errorf("the carved-out paper is %+v, want 100 units at no cost", got)
	}
	if old := f.position(t, f.accountID, f.amazonID); old == nil || old.CostMinor != 100_000 {
		t.Errorf("the original is %+v, want its whole 100000", old)
	}
}
