package tinvest

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// carvedFundRows is the owner's October 2025 as the broker sent it, at the
// live figures: 60 795 units of «Технологии Америки» bought, 44 380,35 of them
// withdrawn "to another depositary" on 15.10.2025 — which the projection reads
// as a transfer, believing it — and 2 559,80 ₽ paid a fortnight later under the
// bond-redemption type, which #174 leaves unparsed.
//
// IT DIFFERS FROM techFundRows IN THE ONE FIGURE THAT MATTERS: the withdrawal
// takes MOST of the position rather than a third of it, leaving 16 414,65. That
// is what makes the owner's own answer — one redemption of the 44 380,35 units
// that were actually redeemed — impossible to write while the withdrawal still
// stands, and it is the shape the feature met the first time it was used.
func carvedFundRows(t *testing.T, f *rebuildFixture) (buy, out, payout OperationItem) {
	t.Helper()
	f.src.instruments[uidTechFund] = InstrumentBrief{
		UID: uidTechFund, FIGI: "TCS20A101X68", ISIN: "RU000A101X68",
		Ticker: "TECH", Name: "Технологии Америки", Currency: "RUB", InstrumentType: "etf",
	}

	buy = loadOperationItem(t, "buy.json")
	buy.ID = "op-carve-buy"
	buy.InstrumentUID, buy.FIGI, buy.InstrumentType = uidTechFund, "TCS20A101X68", "etf"
	buy.Date = time.Date(2021, 3, 1, 8, 0, 0, 0, time.UTC)
	buy.Quantity, buy.QuantityDone = 60795, 60795
	buy.Price = MoneyValue{Currency: "rub", Units: 1, Nano: 0}
	buy.Payment = MoneyValue{Currency: "rub", Units: -60795, Nano: 0}
	buy.Commission = MoneyValue{Currency: "rub", Units: 0, Nano: 0}
	buy.Description = "Покупка 60795 лотов фонда Технологии Америки"

	out = loadOperationItem(t, "output_securities.json")
	out.ID = "op-carve-out"
	out.InstrumentUID, out.FIGI, out.InstrumentType = uidTechFund, "TCS20A101X68", "etf"
	out.Quantity, out.QuantityDone = 44380, 44380
	out.Description = "Вывод 44380.35 лотов фонда Технологии Америки в другой депозитарий"
	// The fixture's own date is in the future and the journal refuses one; the
	// live date is 15.10.2025.
	out.Date = time.Date(2025, 10, 15, 8, 43, 21, 0, time.UTC)

	payout = loadOperationItem(t, "bond_repayment_full_no_quantity.json")
	payout.ID = "op-carve-payout"
	payout.InstrumentUID, payout.FIGI, payout.InstrumentType = uidTechFund, "TCS97A101X68", "etf"
	payout.Payment = MoneyValue{Currency: "rub", Units: 2559, Nano: 800000000}
	payout.Date = time.Date(2025, 10, 29, 14, 17, 22, 0, time.UTC)
	payout.Description = "Погашение Технологии Америки"
	return buy, out, payout
}

// explainService builds the service the way every test here needs it: a real
// journal on both sides, since what is under test is what the journal does.
func explainService(f *rebuildFixture) *Service {
	return NewService(f.store, nil, operation.NewService(f.ops), f.ops, nil, nil, &fakeInserter{}, slog.Default())
}

// TestExplainRowsReplacesTheReadingItCorrects is the case this feature exists
// for, at the figures it was first used at, and until the replacement existed
// it was the case the feature could not do.
//
// The broker's two rows are one partial redemption: 44 380,35 units of the fund
// were retired and 2 559,80 ₽ paid for them. This program reads the first row
// as a transfer to another broker — wrongly, but successfully — so the units it
// names are gone from the position before the owner's own operation is checked,
// and a redemption of exactly those units met «not enough quantity: have
// 16414.65, need 44380.35». Naming the entries for replacement is what makes
// the question the right one: not "does this fit beside the old reading" but
// "does the journal replay once the old reading is gone".
func TestExplainRowsReplacesTheReadingItCorrects(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := carvedFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)

	journal := f.journalOf(t, f.accountID)
	instrumentID := *byExternalID(t, journal,
		externalIDFor(f.mirrorRow(t, f.link, "op-carve-buy"), 1)).InstrumentID
	// The withdrawal really is in the journal, and really does hold the units:
	// without this the test could pass on a fixture where there was nothing to
	// replace.
	if _, ok := findByExternalID(journal, externalIDFor(f.mirrorRow(t, f.link, "op-carve-out"), 1)); !ok {
		t.Fatal("the withdrawal produced no journal entry, so this test would prove nothing about replacing one")
	}

	svc := explainService(f)
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	qty := decimal.RequireFromString("44380.35")
	explained, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID,
		[]string{
			f.mirrorRow(t, f.link, "op-carve-out").ContentKey,
			f.mirrorRow(t, f.link, "op-carve-payout").ContentKey,
		},
		operation.Operation{
			InstrumentID: &instrumentID, Type: operation.TypeRedemption,
			OccurredOn: time.Date(2025, 10, 29, 0, 0, 0, 0, time.UTC),
			Quantity:   &qty, AmountMinor: 255980, Currency: "RUB",
			Note: "частичное погашение паёв Т-Капиталом",
		})
	if err != nil {
		t.Fatalf("ExplainRows: %v — the operation was checked against a journal still holding the reading it replaces", err)
	}

	after := f.journalOf(t, f.accountID)
	for _, id := range []string{"op-carve-out", "op-carve-payout"} {
		row := f.mirrorRow(t, f.link, id)
		for leg := 1; leg <= 2; leg++ {
			if op, ok := findByExternalID(after, externalIDFor(row, leg)); ok {
				t.Errorf("%s still has journal entry %s (%s): the old reading of the event is what an explanation removes",
					id, op.ID, op.Type)
			}
		}
	}

	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	// The broker's own figure for what is left, which is the whole point of
	// getting this right: 60 795 bought less the 44 380,35 redeemed.
	if got := positions[instrumentID].Quantity.String(); got != "16414.65" {
		t.Errorf("the fund position is %s units, want 16414.65 — what the broker still shows as held", got)
	}
	var redemption operation.Operation
	// Read through the engine's own listing and not through journalOf, which
	// lists what this IMPORTER wrote: the redemption is the owner's, entered by
	// hand, and would be missing from that half of the journal by design.
	for _, o := range mustListForEngine(t, f, f.accountID) {
		if o.ID == explained.OperationID {
			redemption = o
		}
	}
	if redemption.ID == uuid.Nil {
		t.Fatal("the manual redemption is not in the journal")
	}
	if redemption.Source != "manual" || redemption.AmountMinor != 255980 {
		t.Errorf("the redemption is %s for %d minor units, want a manual one for 255980",
			redemption.Source, redemption.AmountMinor)
	}
}

// TestARebuildAfterAnExplanationLeavesTheJournalAlone is the other half of the
// same promise: the rebuild that follows must not put back what the
// explanation took out. It is already the projection's rule — an explained row
// produces nothing — but the removal is new, and a rule stated in one place and
// relied on in another is exactly the pair worth pinning.
func TestARebuildAfterAnExplanationLeavesTheJournalAlone(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := carvedFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-carve-buy"), 1)).InstrumentID

	svc := explainService(f)
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	qty := decimal.RequireFromString("44380.35")
	if _, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID,
		[]string{
			f.mirrorRow(t, f.link, "op-carve-out").ContentKey,
			f.mirrorRow(t, f.link, "op-carve-payout").ContentKey,
		},
		operation.Operation{
			InstrumentID: &instrumentID, Type: operation.TypeRedemption,
			OccurredOn: time.Date(2025, 10, 29, 0, 0, 0, 0, time.UTC),
			Quantity:   &qty, AmountMinor: 255980, Currency: "RUB",
		}); err != nil {
		t.Fatalf("ExplainRows: %v", err)
	}

	before := idsOf(f.journalOf(t, f.accountID))
	stats := f.rebuild(t)
	if stats.Added != 0 || stats.Removed != 0 {
		t.Errorf("the rebuild added %d and removed %d operations, want none of either — "+
			"the explained rows produce nothing and the entries they used to produce are already gone",
			stats.Added, stats.Removed)
	}
	if got := idsOf(f.journalOf(t, f.accountID)); !sameIDs(before, got) {
		t.Errorf("the journal changed across a rebuild: %v became %v", before, got)
	}
}

// TestExplainRowsTakesEVERYEntryOfARowItReplaces: one broker row can produce
// several journal entries, and a replacement that took only the first would
// leave the other standing — money the owner's own operation is now also
// accounting for, counted twice with nothing on any screen to say so.
//
// A dividend paid to a card is the shape: the broker pays it and it leaves the
// brokerage account in the same breath, so the projection writes two entries
// (see projectDividendToCard).
func TestExplainRowsTakesEVERYEntryOfARowItReplaces(t *testing.T) {
	f := newRebuildFixture(t)
	divExt := loadOperationItem(t, "div_ext.json")
	divExt.ID = "op-divext"
	f.sync(t, f.link, divExt)
	f.rebuild(t)

	row := f.mirrorRow(t, f.link, "op-divext")
	journal := f.journalOf(t, f.accountID)
	for leg := 1; leg <= 2; leg++ {
		if _, ok := findByExternalID(journal, externalIDFor(row, leg)); !ok {
			t.Fatalf("the dividend-to-card row produced no entry %d, so this test would prove nothing about taking both", leg)
		}
	}

	svc := explainService(f)
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	if _, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID, []string{row.ContentKey},
		operation.Operation{
			Type: operation.TypeDividend, OccurredOn: divExt.Date,
			AmountMinor: 98000, Currency: "RUB",
			Note: "дивиденд на карту, учтён одной записью",
		}); err != nil {
		t.Fatalf("ExplainRows: %v", err)
	}

	after := f.journalOf(t, f.accountID)
	for leg := 1; leg <= 2; leg++ {
		if op, ok := findByExternalID(after, externalIDFor(row, leg)); ok {
			t.Errorf("entry %d of the explained row survived as %s (%s): one row's entries go together or the event is counted twice",
				leg, op.ID, op.Type)
		}
	}
}

// TestAJournalRefusalIsAnsweredAsTheJournalAnswersIt pins defect 2. The engine
// refusing an operation is news about the owner's history, not about this
// program: the journal screen answers it with 409 and the engine's own
// sentence, and until this branch existed the same refusal came back from this
// endpoint as «internal error» with a 500 — the program saying it had broken.
//
// The refusal is taken FROM THE SERVICE rather than invented, so what is
// rendered here is the error the path really produces.
func TestAJournalRefusalIsAnsweredAsTheJournalAnswersIt(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := carvedFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-carve-buy"), 1)).InstrumentID

	svc := explainService(f)
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	// More units than the account holds even after the withdrawal is replaced:
	// 60 795 were bought and this redeems 70 000, so no ordering of the rows
	// makes it fit and the engine is right to refuse.
	qty := decimal.RequireFromString("70000")
	_, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID,
		[]string{f.mirrorRow(t, f.link, "op-carve-payout").ContentKey},
		operation.Operation{
			InstrumentID: &instrumentID, Type: operation.TypeRedemption,
			OccurredOn: time.Date(2025, 10, 29, 0, 0, 0, 0, time.UTC),
			Quantity:   &qty, AmountMinor: 255980, Currency: "RUB",
		})
	if err == nil {
		t.Fatal("redeeming more units than were ever bought was accepted")
	}
	if !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("err = %v, want the journal's own ErrInconsistent", err)
	}

	rec := httptest.NewRecorder()
	writeError(rec, err)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 — the same code the journal screen gives this refusal, "+
			"and not the 500 that told the owner the program had broken", rec.Code)
	}
	// The engine's own sentence, not a stand-in: it names the quantity held and
	// the quantity asked for, which is the whole of what the owner needs.
	if !strings.Contains(rec.Body.String(), "not enough quantity") {
		t.Errorf("body = %s, want the engine's own explanation in it", rec.Body.String())
	}
}

// idsOf and sameIDs compare journals by identity rather than by content: what
// the rebuild must not do is delete a row and write an equal one, which every
// comparison of amounts would call unchanged.
func idsOf(ops []operation.Operation) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.ID)
	}
	return out
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uuid.UUID]int, len(a))
	for _, id := range a {
		seen[id]++
	}
	for _, id := range b {
		seen[id]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
