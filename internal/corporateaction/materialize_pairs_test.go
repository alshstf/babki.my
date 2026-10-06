package corporateaction_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// producedISIN: TCS Group receipts became ТКС Холдинг shares under this ISIN
// on 2024-02-27, with no broker operation for it.
const producedISIN = "RU000A107UL4"

// catalogue adds the produced paper to the catalog and returns its id.
func (f fixture) catalogue(t *testing.T, isin, ticker string) uuid.UUID {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: ticker, Ticker: ticker, ISIN: isin, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("catalogue %s: %v", ticker, err)
	}
	return inst.ID
}

// conversionEvent records "N of this paper became M of that one".
func (f fixture) conversionEvent(t *testing.T, on string, from, to int64) corporateaction.Event {
	t.Helper()
	e, err := f.store.Create(f.ctx, corporateaction.Event{
		Kind: corporateaction.KindConversion, ISIN: amazonISIN, ResultISIN: producedISIN,
		EffectiveOn: date(on), RatioFrom: from, RatioTo: to,
		Source: corporateaction.SourceManual, SourceRef: "https://www.moex.com/n67851",
	})
	if err != nil {
		t.Fatalf("record the conversion: %v", err)
	}
	return e
}

// spinoffEvent records "this paper kept its units and gave up a share of its
// money to that one".
func (f fixture) spinoffEvent(t *testing.T, on string, from, to int64, share string) corporateaction.Event {
	t.Helper()
	basis := decimal.RequireFromString(share)
	e, err := f.store.Create(f.ctx, corporateaction.Event{
		Kind: corporateaction.KindSpinOff, ISIN: amazonISIN, ResultISIN: producedISIN,
		EffectiveOn: date(on), RatioFrom: from, RatioTo: to, BasisShare: &basis,
		Source: corporateaction.SourceManual, SourceRef: "https://www.tbank.ru/invest/help/urgent-funds/",
	})
	if err != nil {
		t.Fatalf("record the spin-off: %v", err)
	}
	return e
}

// position is what the engine says an account holds of one paper, folding its
// whole journal.
func (f fixture) position(t *testing.T, accountID, instrumentID uuid.UUID) *portfolio.Position {
	t.Helper()
	journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("fold the journal: %v", err)
	}
	return positions[instrumentID]
}

// The owner's case: four receipts bought on two days in 2021 become four
// shares; the receipts' cost and days carry over and nothing is realized (НК РФ
// ст. 214.1 п. 13 абз. 17).
func TestAConversionMovesTheWholeHoldingOntoTheNewPaper(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "2", -1_282_920)
	f.buy(t, f.accountID, "2021-07-05", "2", -1_296_940)

	f.conversionEvent(t, "2024-02-27", 1, 1)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 2 {
		t.Fatalf("added %d rows, want 2 — a conversion is a pair", stats.Added)
	}

	if old := f.position(t, f.accountID, f.amazonID); old != nil && old.Quantity.IsPositive() {
		t.Errorf("the old paper still holds %s, want nothing — the whole holding converted", old.Quantity)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil {
		t.Fatal("the new paper is not held at all")
	}
	if got.Quantity.String() != "4" {
		t.Errorf("the new paper holds %s, want 4 at one for one", got.Quantity)
	}
	// The money paid for the receipts, to the kopeck.
	if got.CostMinor != 2_579_860 {
		t.Errorf("the new paper cost %d, want 2579860 — the two purchases of the receipts", got.CostMinor)
	}
	if len(got.Lots) != 2 {
		t.Fatalf("the new paper has %d parcels, want 2 — one per purchase of the old", len(got.Lots))
	}
	for i, want := range []string{"2021-07-02", "2021-07-05"} {
		if got.Lots[i].AcquiredOn == nil || got.Lots[i].AcquiredOn.Format("2006-01-02") != want {
			t.Errorf("parcel %d acquired %v, want %s — the day the RECEIPT was bought", i, got.Lots[i].AcquiredOn, want)
		}
	}
}

// The original keeps every unit and gives up a share of its cost; the new
// paper is built from those parcels (НК РФ ст. 214.1 п. 13 абз. 8 -> ст. 277
// п. 7). The owner's case: Т-Капитал's blocked assets carved into closed funds one
// for one on 2023-12-22.
func TestASpinoffLeavesTheUnitsAndCarvesOutTheirMoney(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "TECH2")
	f.buy(t, f.accountID, "2020-12-30", "100", -100_000)

	f.spinoffEvent(t, "2023-12-22", 1, 1, "0.25")
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 2 {
		t.Fatalf("added %d rows, want 2 — a spin-off is a pair", stats.Added)
	}

	old := f.position(t, f.accountID, f.amazonID)
	if old == nil {
		t.Fatal("the original paper is gone; a spin-off leaves it standing")
	}
	if old.Quantity.String() != "100" {
		t.Errorf("the original holds %s units, want 100 — a spin-off moves money, not units", old.Quantity)
	}
	if old.CostMinor != 75_000 {
		t.Errorf("the original cost %d, want 75000 — a quarter of 100000 moved away", old.CostMinor)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil {
		t.Fatal("the carved-out paper is not held at all")
	}
	if got.Quantity.String() != "100" {
		t.Errorf("the carved-out paper holds %s, want 100 at one for one", got.Quantity)
	}
	if got.CostMinor != 25_000 {
		t.Errorf("the carved-out paper cost %d, want 25000", got.CostMinor)
	}
	// The invariant: a carve-out creates and loses no money.
	if old.CostMinor+got.CostMinor != 100_000 {
		t.Errorf("the basis after the spin-off is %d, want the 100000 that was paid",
			old.CostMinor+got.CostMinor)
	}
}

// A conversion takes the parcels held on its own day. A later buy and sale
// consume the parcel it took, so the end state would carry a different day and
// sum. Elsewhere in this file the two coincide.
func TestAConversionTakesTheParcelsOfItsOwnDayAndNotTheOnesLeftAtTheEnd(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "2", -600_000)
	f.buy(t, f.accountID, "2021-07-05", "2", -900_000)
	// At the end the oldest parcel is the 5th's; on the 3rd it was the 2nd's.
	f.sell(t, f.accountID, "2021-07-06", "2", 700_000)

	f.conversionEvent(t, "2021-07-03", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got := f.position(t, f.accountID, produced)
	if got == nil || got.Quantity.String() != "2" {
		t.Fatalf("the new paper holds %v, want the 2 held on the day of the conversion", got)
	}
	if got.CostMinor != 600_000 {
		t.Errorf("the new paper cost %d, want 600000 — what was paid on 2021-07-02, the parcel held on the day",
			got.CostMinor)
	}
	if len(got.Lots) != 1 || got.Lots[0].AcquiredOn == nil ||
		got.Lots[0].AcquiredOn.Format("2006-01-02") != "2021-07-02" {
		t.Errorf("the new paper's parcel is %v, want one acquired 2021-07-02", got.Lots)
	}
}

// A spin-off divides only the money paid by its day; a later purchase was
// never in the fund when its assets were split off.
func TestASpinoffCarvesFromTheMoneyHeldOnTheDayOnly(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "TECH2")
	f.buy(t, f.accountID, "2020-12-30", "100", -100_000)
	f.spinoffEvent(t, "2023-12-22", 1, 1, "0.25")
	// Bought after the carve-out.
	f.buy(t, f.accountID, "2024-03-01", "100", -900_000)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got := f.position(t, f.accountID, produced)
	if got == nil {
		t.Fatal("the carved-out paper is not held at all")
	}
	if got.CostMinor != 25_000 {
		t.Errorf("the carved-out paper cost %d, want 25000 — a quarter of the 100000 paid BEFORE the carve-out",
			got.CostMinor)
	}
	if got.Quantity.String() != "100" {
		t.Errorf("the carved-out paper holds %s, want 100 — the units held on the day, one for one", got.Quantity)
	}
	old := f.position(t, f.accountID, f.amazonID)
	if old == nil || old.CostMinor != 975_000 {
		t.Errorf("the original cost %v, want 975000 — everything paid less the 25000 carved out", old)
	}
}

// A second run over an unchanged world does nothing.
func TestMaterializingAPairTwiceWritesItOnce(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	f.conversionEvent(t, "2024-02-27", 1, 1)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats.Added != 0 || stats.Removed != 0 {
		t.Errorf("the second run added %d and removed %d, want 0 and 0", stats.Added, stats.Removed)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 2 {
		t.Errorf("the account carries %d registry rows, want the 2 of one pair", len(rows))
	}
}

// A corrected ratio rewrites both legs, though only one count changed: the
// journal refuses removing half a group.
func TestCorrectingAPairsRatioReplacesBothLegs(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	wrong := f.conversionEvent(t, "2024-02-27", 1, 1)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := f.position(t, f.accountID, produced); got == nil || got.Quantity.String() != "4" {
		t.Fatalf("the new paper holds %v, want 4 before the correction", got)
	}

	if _, err := f.store.Delete(f.ctx, wrong.ID); err != nil {
		t.Fatalf("remove the wrong event: %v", err)
	}
	f.conversionEvent(t, "2024-02-27", 1, 3)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("run after the correction: %v", err)
	}
	if stats.Removed != 2 || stats.Added != 2 {
		t.Errorf("the correction removed %d and added %d, want 2 and 2 — a pair is rewritten whole",
			stats.Removed, stats.Added)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil || got.Quantity.String() != "12" {
		t.Fatalf("the new paper holds %v, want 12 at three for one", got)
	}
	if got.CostMinor != 1_282_920 {
		t.Errorf("the new paper cost %d, want the 1282920 that was paid — a ratio changes counts, not money", got.CostMinor)
	}
}

// A deleted event's two legs both go.
func TestDeletingTheEventTakesBothLegsOut(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	e := f.conversionEvent(t, "2024-02-27", 1, 1)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := f.store.Delete(f.ctx, e.ID); err != nil {
		t.Fatalf("delete the event: %v", err)
	}
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("run after the deletion: %v", err)
	}
	if stats.Removed != 2 {
		t.Errorf("removed %d rows, want 2 — both legs of the pair", stats.Removed)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("%d registry rows survive an event nothing asks for", len(rows))
	}
	if got := f.position(t, f.accountID, produced); got != nil && got.Quantity.IsPositive() {
		t.Errorf("the produced paper still holds %s after its event was deleted", got.Quantity)
	}
	if held := f.held(t, f.accountID); held.String() != "4" {
		t.Errorf("the original holds %s, want the 4 it was bought with", held)
	}
}

// A purchase backdated under a conversion changes the parcels it names, so the
// pair is rewritten even with the same counts and money (see sameRow).
func TestAPurchaseBackdatedUnderAConversionRebuildsThePair(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-05", "2", -600_000)
	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// An earlier purchase entered afterwards; production runs the registry
	// from the hand-entry hook, here it is called directly.
	f.buy(t, f.accountID, "2021-07-02", "3", -900_000)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("run after the backdated purchase: %v", err)
	}
	if stats.Removed != 2 || stats.Added != 2 {
		t.Errorf("removed %d and added %d, want 2 and 2 — the pair names parcels that changed",
			stats.Removed, stats.Added)
	}
	got := f.position(t, f.accountID, produced)
	if got == nil || got.Quantity.String() != "5" {
		t.Fatalf("the new paper holds %v, want 5 — the conversion takes the whole holding", got)
	}
	if got.CostMinor != 1_500_000 {
		t.Errorf("the new paper cost %d, want 1500000 — both purchases of the old", got.CostMinor)
	}
	if len(got.Lots) != 2 {
		t.Fatalf("the new paper has %d parcels, want 2", len(got.Lots))
	}
	// FIFO order: the backdated purchase is the front on the new paper too.
	if got.Lots[0].AcquiredOn == nil || got.Lots[0].AcquiredOn.Format("2006-01-02") != "2021-07-02" {
		t.Errorf("the first parcel is %v, want the backdated 2021-07-02", got.Lots[0].AcquiredOn)
	}
}

// An account that held nothing on the day gets no pair.
func TestAPairIsNotWrittenForAnAccountThatHeldNothing(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	// The second account buys only after the conversion.
	f.buy(t, f.otherID, "2024-03-01", "5", -1_000_000)

	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if rows := f.registryRows(t, f.otherID); len(rows) != 0 {
		t.Errorf("the account that held nothing on the day got %d rows", len(rows))
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 2 {
		t.Errorf("the holder got %d rows, want the 2 of one pair", len(rows))
	}
}

// A run for the produced paper's ISIN sees the arriving leg on its own
// instrument and must leave it: the row's name points at the source paper.
func TestTheProducedPapersOwnRunLeavesThePairAlone(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize the conversion: %v", err)
	}

	stats, err := f.materializer.ForISIN(f.ctx, producedISIN)
	if err != nil {
		t.Fatalf("materialize the produced paper: %v", err)
	}
	if stats.Removed != 0 || stats.Added != 0 {
		t.Errorf("the produced paper's own run removed %d and added %d, want 0 and 0 — the pair is not its to touch",
			stats.Removed, stats.Added)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 2 {
		t.Errorf("%d registry rows survive, want the 2 of the pair", len(rows))
	}
}

// ForAccount, the hand-entry trigger, reaches pairs as ForISIN does.
func TestAnAccountSweepAlsoBringsPairsIntoLine(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	f.conversionEvent(t, "2024-02-27", 1, 1)

	stats, err := f.materializer.ForAccount(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("materialize for the account: %v", err)
	}
	if stats.Added != 2 {
		t.Fatalf("added %d rows, want 2", stats.Added)
	}
	if got := f.position(t, f.accountID, produced); got == nil || got.Quantity.String() != "4" {
		t.Errorf("the new paper holds %v, want 4", got)
	}
}

// Both legs share one group, which makes them one event downstream.
func TestBothLegsOfAMaterializedPairShareOneGroup(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	rows := f.registryRows(t, f.accountID)
	if len(rows) != 2 {
		t.Fatalf("%d registry rows, want 2", len(rows))
	}
	for _, o := range rows {
		if o.TransferGroupID == nil {
			t.Fatalf("%s carries no group, so nothing downstream can tell it is half of one event", o.Type)
		}
	}
	if *rows[0].TransferGroupID != *rows[1].TransferGroupID {
		t.Errorf("the two legs carry different groups, %s and %s", rows[0].TransferGroupID, rows[1].TransferGroupID)
	}
	// And the group survives a recomputation.
	was := *rows[0].TransferGroupID
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("second run: %v", err)
	}
	again := f.registryRows(t, f.accountID)
	if len(again) != 2 || *again[0].TransferGroupID != was {
		t.Errorf("the group changed on a recomputation, from %s to %v", was, again[0].TransferGroupID)
	}
}

// No catalog row for the produced paper: nothing is written, and the event
// says why.
func TestAConversionWithoutItsProducedPaperWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	f.conversionEvent(t, "2024-02-27", 1, 1)

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 0 {
		t.Errorf("added %d rows though the paper it produces is not in the catalog", stats.Added)
	}
	if held := f.held(t, f.accountID); held.String() != "4" {
		t.Errorf("the account holds %s, want the 4 it bought — nothing was converted", held)
	}
	// And the registry says so, so a reader is not left guessing.
	cataloged, err := f.store.CatalogedISINs(f.ctx, []string{producedISIN})
	if err != nil {
		t.Fatalf("look up the produced paper: %v", err)
	}
	events, err := f.store.ByISIN(f.ctx, amazonISIN)
	if err != nil || len(events) != 1 {
		t.Fatalf("read the event back: %v (%d events)", err, len(events))
	}
	if got := events[0].NotCountedReason(cataloged[producedISIN]); got != corporateaction.NotCountedResultMissing {
		t.Errorf("the event's reason is %q, want %q", got, corporateaction.NotCountedResultMissing)
	}
}

// A split in 2022 then a conversion in 2024 converts the multiplied
// count.
func TestASplitAndAConversionOfOnePaperApplyInOrder(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)

	f.splitEvent(t, "2022-06-06", 1, 20)
	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got := f.position(t, f.accountID, produced)
	if got == nil || got.Quantity.String() != "20" {
		t.Fatalf("the new paper holds %v, want 20 — the split ran first", got)
	}
	if got.CostMinor != 323_000 {
		t.Errorf("the new paper cost %d, want the 323000 that was paid", got.CostMinor)
	}
}

// The registry's departing legs name the lots they take, and a leg written
// before lots had numbers is rewritten with them on the next run.
func TestTheRegistrysLegsNameTheirLots(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "2", -1_282_920)
	f.buy(t, f.accountID, "2021-07-05", "2", -1_296_940)
	f.conversionEvent(t, "2024-02-27", 1, 1)

	numbers := func() (named, unnamed int) {
		journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, f.accountID)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range journal {
			if o.Type != operation.TypeExchangeOut {
				continue
			}
			for _, pc := range o.TransferLots {
				if pc.From.IsZero() {
					unnamed++
				} else {
					named++
				}
			}
		}
		return named, unnamed
	}
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatal(err)
	}
	if named, unnamed := numbers(); named != 2 || unnamed != 0 {
		t.Fatalf("the conversion's leg names %d lots and leaves %d unnamed, want both purchases named", named, unnamed)
	}

	if _, err := f.pool.Exec(f.ctx, `UPDATE operation_transfer_lots SET from_origin = NULL, from_seq = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatal(err)
	}
	if named, unnamed := numbers(); named != 2 || unnamed != 0 {
		t.Errorf("after the next run the leg names %d lots and leaves %d unnamed, want it rewritten with both", named, unnamed)
	}
}
