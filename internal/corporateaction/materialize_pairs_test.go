package corporateaction_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/portfolio"
)

// producedISIN is the paper the conversions and spin-offs below produce. It is
// the owner's own case: the receipts of TCS Group became shares of ТКС Холдинг
// under this ISIN on 2024-02-27, and the broker reported no operation for it.
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

// TestAConversionMovesTheWholeHoldingOntoTheNewPaper is the owner's own case:
// four depositary receipts bought on two days in 2021 become four shares of the
// company that redomiciled, and what was paid for the RECEIPTS is what the
// shares cost (НК РФ ст. 214.1 п. 13 абз. 17). Nothing is realized, and the days
// the receipts were bought are the days the shares carry.
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
	// The money that was paid for the receipts, to the kopeck, and not a
	// valuation of anything.
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

// TestASpinoffLeavesTheUnitsAndCarvesOutTheirMoney: the original paper keeps
// every unit and gives up a share of what was paid for it, and the carved-out
// paper is built from those very parcels (НК РФ ст. 214.1 п. 13 абз. 8 → ст. 277
// п. 7). The owner's own case is the blocked assets of the Т-Капитал funds,
// carved into closed funds one unit for one on 2023-12-22.
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
	// NOT A COINCIDENCE BUT THE INVARIANT: no minor unit is created or lost by a
	// carve-out, because nothing was bought or sold.
	if old.CostMinor+got.CostMinor != 100_000 {
		t.Errorf("the basis after the spin-off is %d, want the 100000 that was paid",
			old.CostMinor+got.CostMinor)
	}
}

// TestMaterializingAPairTwiceWritesItOnce. Every run recomputes the rows the
// registry asks for and diffs them, so a second run over an unchanged world must
// find nothing to do — not "write it again and let the unique index refuse".
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

// TestCorrectingAPairsRatioReplacesBothLegs. A ratio fixed after the fact must
// reach the journal — and BOTH legs must be rewritten even though only one of
// them carries a count that changed, because the journal refuses a delta that
// removes one leg of a group and leaves the other.
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

// TestDeletingTheEventTakesBothLegsOut. The registry's rows are a projection: an
// event that no longer exists asks for nothing, and both halves of what it wrote
// go.
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

// TestAPurchaseBackdatedUnderAConversionRebuildsThePair. The pair names the very
// parcels it converted, so a purchase that turns up underneath it — a broker
// history imported after the fact, an operation entered late — changes what
// those parcels are. The counts and the money may be identical and the pair
// still has to be rewritten, which is why the breakdown is part of what "the
// same row" means (see sameRow).
func TestAPurchaseBackdatedUnderAConversionRebuildsThePair(t *testing.T) {
	f := newFixture(t)
	produced := f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-05", "2", -600_000)
	f.conversionEvent(t, "2024-02-27", 1, 1)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// An earlier purchase, entered afterwards. The hand-entry door runs the
	// registry itself in production; here it is called directly so the test says
	// what it is testing.
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
	// FIFO order, so the backdated purchase is the FRONT of the queue on the new
	// paper as it would have been on the old. A pair left standing would have had
	// one parcel of 2 units here.
	if got.Lots[0].AcquiredOn == nil || got.Lots[0].AcquiredOn.Format("2006-01-02") != "2021-07-02" {
		t.Errorf("the first parcel is %v, want the backdated 2021-07-02", got.Lots[0].AcquiredOn)
	}
}

// TestAPairIsNotWrittenForAnAccountThatHeldNothing. The event is true about the
// paper; whether it produces a row is a question about each account.
func TestAPairIsNotWrittenForAnAccountThatHeldNothing(t *testing.T) {
	f := newFixture(t)
	f.catalogue(t, producedISIN, "T")
	f.buy(t, f.accountID, "2021-07-02", "4", -1_282_920)
	// The second account buys only AFTER the conversion, so it held nothing on
	// the day and there is nothing to convert.
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

// TestTheProducedPapersOwnRunLeavesThePairAlone is the ownership rule proved
// from the other side. The arriving leg sits on the PRODUCED paper, so a run for
// that paper's own ISIN sees a registry row on an instrument it owns — and must
// not take it for a row of its own that nothing asks for any more. It reads the
// row's name, which points at the paper the event came FROM.
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

// TestAnAccountSweepAlsoBringsPairsIntoLine: ForAccount is the trigger behind a
// hand-entered operation, and it must reach a pair exactly as ForISIN does.
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

// TestBothLegsOfAMaterializedPairShareOneGroup. The group is what makes the two
// rows one event to everything downstream — the journal refuses to remove half
// of one, and a screen that shows a conversion shows a pair.
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
	// And the group SURVIVES a recomputation: derived from the event and the
	// holding, never drawn fresh, or every run would rewrite the pair.
	was := *rows[0].TransferGroupID
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("second run: %v", err)
	}
	again := f.registryRows(t, f.accountID)
	if len(again) != 2 || *again[0].TransferGroupID != was {
		t.Errorf("the group changed on a recomputation, from %s to %v", was, again[0].TransferGroupID)
	}
}

// TestAConversionWithoutItsProducedPaperWritesNothing. A journal row cannot
// point at a paper the catalog has no row for, and inventing one would be this
// program deciding what the owner holds.
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

// TestASplitAndAConversionOfOnePaperApplyInOrder. Events act on the holding the
// ones before them left, and that is what lets a paper that split in 2022 be
// converted in 2024 with the multiplied count.
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
