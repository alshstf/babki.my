package operation_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/portfolio/portfoliotest"
)

// imported dresses an operation as an importer hands it over: a non-manual
// source and the broker record's id.
func imported(op operation.Operation, externalID string) operation.Operation {
	op.Source = "tinvest"
	op.ExternalID = &externalID
	return op
}

// journalOf reads the account's journal back through the engine's own listing
// and folds it, so assertions see what later reads see, not what
// ApplyImportDelta returned.
func journalOf(t *testing.T, f fixture, accountID uuid.UUID) ([]operation.Operation, map[uuid.UUID]*portfolio.Position) {
	t.Helper()
	ops, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	positions, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("the journal that was written does not replay when read back: %v", err)
	}
	return ops, positions
}

func TestApplyImportDeltaEmptyDeltaWritesNothing(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{})
	if err != nil {
		t.Fatalf("empty delta: %v", err)
	}
	if len(applied) != 0 || len(refused) != 0 {
		t.Errorf("empty delta applied %d and refused %d, want none of either", len(applied), len(refused))
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 0 {
		t.Errorf("journal holds %d operations after an empty delta, want 0", len(ops))
	}
}

// A broker operation this program cannot record is refused on its own, with
// its real reason, while the rest of the history loads.
func TestApplyImportDeltaIsolatesTheCandidateThatCannotApply(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	oversell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-02"), Quantity: dec("999"), Price: dec("100"),
		AmountMinor: 9_990_000, Currency: "RUB",
	}, "op-oversell")
	dividend := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeDividend,
		OccurredOn: date("2026-07-03"), AmountMinor: 5_000, Currency: "RUB",
	}, "op-dividend")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, oversell, dividend},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied %d operations, want 2 — one bad candidate must not take the history with it", len(applied))
	}
	if len(refused) != 1 {
		t.Fatalf("refused %d candidates, want 1: %+v", len(refused), refused)
	}
	if refused[0].ExternalID != "op-oversell" {
		t.Errorf("refusal names %q, want the candidate that could not apply", refused[0].ExternalID)
	}
	if !errors.Is(refused[0].Err, operation.ErrInconsistent) {
		t.Errorf("refusal reason = %v, want ErrInconsistent — the real reason, not a stand-in", refused[0].Err)
	}

	ops, positions := journalOf(t, f, f.accountID)
	if len(ops) != 2 {
		t.Fatalf("journal holds %d operations, want 2", len(ops))
	}
	if q := positions[f.sberID].Quantity; !q.Equal(*dec("10")) {
		t.Errorf("position quantity = %s, want 10", q)
	}
}

// A replacement may only take out an importer's rows; a hand-entered row is
// the owner's, deleted on the journal screen.
func TestCreateReplacingWillNotReplaceARowEnteredByHand(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	byHand, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 100_000, Currency: "RUB",
		Note: "пополнение, введённое владельцем",
	})
	if err != nil {
		t.Fatalf("the hand-entered operation: %v", err)
	}

	_, err = svc.CreateReplacing(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-02"), AmountMinor: 200_000, Currency: "RUB",
	}, []uuid.UUID{byHand.ID})
	if err == nil {
		t.Fatal("a replacement took out a row the owner had entered by hand")
	}
	if !errors.Is(err, family.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation — the caller named a row it may not replace", err)
	}
	const reason = "entered by hand"
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("refusal = %v, want it to name %q rather than whatever the journal would have said next", err, reason)
	}

	// And the row is still there.
	if _, err := f.store.ByID(f.ctx, f.spaceID, byHand.ID); err != nil {
		t.Errorf("the hand-entered operation is gone after a refused replacement: %v", err)
	}
}

// A split is refused by validateImported itself. The candidate has no
// instrument, which validateByType would also refuse, so the message is checked
// for the import path's own wording.
func TestApplyImportDeltaRefusesASplitFromTheImporter(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	split := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-01"), SplitRatio: dec("2"), AmountMinor: 0,
		Currency: "RUB",
	}, "op-split")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{split},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("applied %d operations, want none — an import cannot know about a split", len(applied))
	}
	if len(refused) != 1 || !errors.Is(refused[0].Err, family.ErrValidation) {
		t.Fatalf("refused = %+v, want one ErrValidation refusal", refused)
	}
	const reason = "does not record splits"
	if !strings.Contains(refused[0].Err.Error(), reason) {
		t.Errorf("refusal reason = %v, want it to name %q — the import path's own unconditional reason, "+
			"not whatever validateByType would have said about this particular candidate (it carries no "+
			"instrument, which validateByType refuses first, for an unrelated reason)", refused[0].Err, reason)
	}
}

// Only the registry may send a pre-computed breakdown. A transfer between the
// owner's accounts must not: its parcel is released from the journal, and a
// supplied one would be an invented cost basis. (An arrival from another broker
// carries the owner's stated purchases; see the next test.)
func TestApplyImportDeltaRefusesABreakdownFromAnyoneButTheRegistry(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	on := date("2021-01-08")
	group := uuid.New()
	// One leg of a move between the owner's accounts, carrying a parcel.
	transfer := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: 100_000,
		Currency: "RUB", TransferGroupID: &group,
		TransferLots: []operation.ReleasedLot{
			{Quantity: *dec("10"), CostMinor: 100_000, AcquiredOn: &on},
		},
	}, "op-transfer-with-its-own-parcel")

	applied, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{transfer},
	})
	// The whole delta fails: an unearned parcel is a broken contract, not a
	// row the journal cannot take.
	if !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("err = %v, want ErrImportContract — a supplied parcel is a cost basis this path did not work out", err)
	}
	const reason = "not supplied"
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("refusal reason = %v, want it to name %q", err, reason)
	}
	if len(applied) != 0 {
		t.Fatalf("applied %d operations, want none", len(applied))
	}
}

// Shares from a broker outside this program carry the purchases the owner
// stated, and come back as dated, priced lots.
func TestApplyImportDeltaTakesTheStatedPurchasesOfAnArrivalFromOutside(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	early, late := date("2021-01-08"), date("2023-05-02")
	arrival := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: 250_000,
		Currency: "RUB",
		TransferLots: []operation.ReleasedLot{
			{Quantity: *dec("4"), CostMinor: 100_000, AcquiredOn: &early},
			{Quantity: *dec("6"), CostMinor: 150_000, AcquiredOn: &late},
		},
	}, "op-arrival-from-outside")

	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{arrival},
	}); err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	_, positions := journalOf(t, f, f.accountID)
	p := positions[f.sberID]
	if p == nil || len(p.Lots) != 2 {
		t.Fatalf("position = %+v, want two lots", p)
	}
	if p.CostMinor != 250_000 || !p.Lots[0].AcquiredOn.Equal(early) || !p.Lots[1].AcquiredOn.Equal(late) {
		t.Errorf("lots = %+v, cost %d — want the two stated purchases, 250000 in all", p.Lots, p.CostMinor)
	}
}

// A broker's tax correction arrives positive (seven of nine on the owner's
// account) and is taken; a zero is still refused.
func TestApplyImportDeltaTakesATaxThatGaveMoneyBack(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	refund := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeTax,
		OccurredOn: date("2026-07-01"), AmountMinor: 32_000, Currency: "RUB",
	}, "op-tax-refund")
	nothing := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeTax,
		OccurredOn: date("2026-07-02"), AmountMinor: 0, Currency: "RUB",
	}, "op-tax-zero")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{refund, nothing},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(applied) != 1 || applied[0].AmountMinor != 32_000 {
		t.Fatalf("applied = %+v, want the refund alone, with its sign untouched", applied)
	}
	if len(refused) != 1 || !errors.Is(refused[0].Err, family.ErrValidation) {
		t.Fatalf("refused = %+v, want the zero refused with ErrValidation", refused)
	}
	if refused[0].ExternalID != "op-tax-zero" {
		t.Errorf("refused %q, want the zero — the refund is the one that must go in", refused[0].ExternalID)
	}
}

// The hand-entry path still refuses a positive tax: there it is most likely a
// typo.
func TestCreateStillRefusesATaxThatIsNotACharge(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	_, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeTax,
		OccurredOn: date("2026-07-01"), AmountMinor: 32_000, Currency: "RUB",
	})
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("Create of a positive tax = %v, want ErrValidation: by hand, that sign is a typo", err)
	}
}

// pairOfLegs builds the two legs of one transfer sharing a caller-computed
// group id.
func pairOfLegs(f fixture, group uuid.UUID, quantity string, on string) (out, in operation.Operation) {
	out = imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date(on), Quantity: dec(quantity), Currency: "RUB",
		TransferGroupID: &group,
	}, "op-transfer-out")
	in = imported(operation.Operation{
		AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date(on), Quantity: dec(quantity), Currency: "RUB",
		TransferGroupID: &group,
	}, "op-transfer-in")
	return out, in
}

// A transfer is one event: if one leg is refused, so is the other.
func TestApplyImportDeltaRefusesBothLegsWhenOneIsRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	// The source account holds ten; the pair claims to move a thousand.
	out, in := pairOfLegs(f, uuid.New(), "1000", "2026-07-05")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, out, in},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied %d operations, want 1 (the buy alone)", len(applied))
	}
	if len(refused) != 2 {
		t.Fatalf("refused %d candidates, want both legs: %+v", len(refused), refused)
	}
	named := map[string]bool{}
	for _, r := range refused {
		named[r.ExternalID] = true
		if !errors.Is(r.Err, operation.ErrInconsistent) {
			t.Errorf("refusal of %s = %v, want ErrInconsistent", r.ExternalID, r.Err)
		}
	}
	if !named["op-transfer-out"] || !named["op-transfer-in"] {
		t.Errorf("refusals name %v, want both legs", named)
	}
	if ops, _ := journalOf(t, f, f.account2ID); len(ops) != 0 {
		t.Errorf("destination journal holds %d operations, want 0 — the arriving leg must not survive its sibling", len(ops))
	}
}

// On the import path the arriving account keeps the purchase dates, and the
// breakdown is computed from the journal rather than taken from the caller.
func TestApplyImportDeltaPairCarriesTheDatesItMoved(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2020-01-05"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	out, in := pairOfLegs(f, uuid.New(), "10", "2026-07-10")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, out, in},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused %+v, want none", refused)
	}
	if len(applied) != 3 {
		t.Fatalf("applied %d operations, want 3", len(applied))
	}

	var storedOut, storedIn operation.Operation
	for _, o := range applied {
		switch o.Type {
		case operation.TypeTransferOut:
			storedOut = o
		case operation.TypeTransferIn:
			storedIn = o
		}
	}
	// The basis is the journal's: 100 000 for the lot bought, on both legs.
	if storedOut.AmountMinor != 100_000 || storedIn.AmountMinor != 100_000 {
		t.Errorf("legs carry %d / %d, want the released basis 100000 on both",
			storedOut.AmountMinor, storedIn.AmountMinor)
	}
	if len(storedOut.TransferLots) != 1 || len(storedIn.TransferLots) != 1 {
		t.Fatalf("legs carry %d / %d pieces, want one on each",
			len(storedOut.TransferLots), len(storedIn.TransferLots))
	}
	// Pieces are stored next to the arriving leg only, as CreatePair does.
	if n := f.lotRows(t, storedIn.ID); n != 1 {
		t.Errorf("lot rows on the arriving leg = %d, want 1", n)
	}
	if n := f.lotRows(t, storedOut.ID); n != 0 {
		t.Errorf("lot rows on the departing leg = %d, want 0 — the pair stores one breakdown, not two", n)
	}

	_, destination := journalOf(t, f, f.account2ID)
	lots := destination[f.sberID].Lots
	if len(lots) != 1 {
		t.Fatalf("destination holds %d lots, want 1", len(lots))
	}
	if !sameAcquisition(lots[0].AcquiredOn, datep("2020-01-05")) {
		t.Errorf("destination lot acquired %s, want 2020-01-05 — the day it was bought", acquired(lots[0].AcquiredOn))
	}
	if lots[0].CostMinor != 100_000 {
		t.Errorf("destination lot cost = %d, want 100000", lots[0].CostMinor)
	}
	_, source := journalOf(t, f, f.accountID)
	if q := source[f.sberID].Quantity; !q.IsZero() {
		t.Errorf("source position quantity = %s, want 0", q)
	}
}

// A lone transfer_in (shares from another broker) is accepted, undated.
func TestApplyImportDeltaLoneTransferInArrivesUndated(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	in := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-01"), Quantity: dec("5"), AmountMinor: 100_000,
		Currency: "RUB",
	}, "op-transfer-in")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{in},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want one applied leg", len(applied), refused)
	}
	if n := f.lotRows(t, applied[0].ID); n != 0 {
		t.Errorf("lot rows = %d, want 0 — there is no breakdown to invent", n)
	}

	_, positions := journalOf(t, f, f.accountID)
	lots := positions[f.sberID].Lots
	if len(lots) != 1 {
		t.Fatalf("position holds %d lots, want 1", len(lots))
	}
	if lots[0].AcquiredOn != nil {
		t.Errorf("lot acquired %s, want unknown — nobody recorded when these shares were bought", acquired(lots[0].AcquiredOn))
	}
	if lots[0].CostMinor != 100_000 {
		t.Errorf("lot cost = %d, want the declared basis 100000", lots[0].CostMinor)
	}
}

// A lone undated transfer_in, then an import pair moving part of that lot on to
// a third account. Only the import path produces this, and an undated lot is
// first out of the FIFO queue, so a mistake here would misorder every later sale
// of shares mirrored in from another broker.
func TestApplyImportDeltaTransfersPartOfAnUndatedLot(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	arrival := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: 100_000,
		Currency: "RUB",
	}, "op-arrival")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{arrival},
	})
	if err != nil || len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("seed the undated arrival: applied %d, refused %+v, err %v", len(applied), refused, err)
	}

	out, in := pairOfLegs(f, uuid.New(), "4", "2026-07-15")
	applied, refused, err = svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{out, in},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("applied %d, refused %+v, want both legs of the pair applied", len(applied), refused)
	}

	var storedIn operation.Operation
	for _, o := range applied {
		if o.Type == operation.TypeTransferIn {
			storedIn = o
		}
	}
	if len(storedIn.TransferLots) != 1 {
		t.Fatalf("arriving leg carries %d pieces, want 1", len(storedIn.TransferLots))
	}
	piece := storedIn.TransferLots[0]
	if piece.AcquiredOn != nil {
		t.Errorf("piece acquired %s, want unknown — the lot it was released from never knew either",
			acquired(piece.AcquiredOn))
	}
	if !piece.Quantity.Equal(*dec("4")) || piece.CostMinor != 40_000 {
		t.Errorf("piece = %s/%d, want 4/40000 — a quarter of the arrival's 10 shares and 100000 basis", piece.Quantity, piece.CostMinor)
	}

	_, source := journalOf(t, f, f.accountID)
	sourcePos := source[f.sberID]
	if !sourcePos.Quantity.Equal(*dec("6")) {
		t.Errorf("source quantity = %s, want 6", sourcePos.Quantity)
	}
	if sourcePos.CostMinor != 60_000 {
		t.Errorf("source cost = %d, want 60000 — the basis left after 40000 of it moved on", sourcePos.CostMinor)
	}
	if len(sourcePos.Lots) != 1 || sourcePos.Lots[0].AcquiredOn != nil {
		t.Errorf("source lot = %+v, want one lot still undated", sourcePos.Lots)
	}

	_, destination := journalOf(t, f, f.account2ID)
	destPos := destination[f.sberID]
	if !destPos.Quantity.Equal(*dec("4")) {
		t.Errorf("destination quantity = %s, want 4", destPos.Quantity)
	}
	if destPos.CostMinor != 40_000 {
		t.Errorf("destination cost = %d, want 40000", destPos.CostMinor)
	}
	if len(destPos.Lots) != 1 || destPos.Lots[0].AcquiredOn != nil {
		t.Errorf("destination lot = %+v, want one lot still undated — the second broker never learns a date the first one never gave", destPos.Lots)
	}
}

// A departing leg with no sibling still records which lots went.
func TestApplyImportDeltaLoneTransferOutFreezesWhatLeft(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	first := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2020-01-05"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy-1")
	second := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-02-08"), Quantity: dec("10"),
		AmountMinor: -200_000, Currency: "RUB",
	}, "op-buy-2")
	out := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-10"), Quantity: dec("10"), Currency: "RUB",
	}, "op-transfer-out")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{first, second, out},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 3 {
		t.Fatalf("applied %d, refused %+v, want three applied", len(applied), refused)
	}

	var storedOut operation.Operation
	for _, o := range applied {
		if o.Type == operation.TypeTransferOut {
			storedOut = o
		}
	}
	if storedOut.AmountMinor != 100_000 {
		t.Errorf("departing leg carries %d, want the oldest lot's basis 100000", storedOut.AmountMinor)
	}
	if len(storedOut.TransferLots) != 1 {
		t.Fatalf("departing leg carries %d pieces, want 1", len(storedOut.TransferLots))
	}
	if !sameAcquisition(storedOut.TransferLots[0].AcquiredOn, datep("2020-01-05")) {
		t.Errorf("piece acquired %s, want 2020-01-05", acquired(storedOut.TransferLots[0].AcquiredOn))
	}
	// With no sibling, the pieces are stored next to the departing leg.
	if n := f.lotRows(t, storedOut.ID); n != 1 {
		t.Errorf("lot rows on the lone departing leg = %d, want 1", n)
	}

	_, positions := journalOf(t, f, f.accountID)
	if c := positions[f.sberID].CostMinor; c != 200_000 {
		t.Errorf("remaining basis = %d, want 200000 — the newer lot is what is left", c)
	}
}

// A delta naming one broker record twice is the caller's mistake and fails
// whole.
func TestApplyImportDeltaRefusesADeltaThatRepeatsAnExternalID(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	deposit := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-deposit")

	_, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{deposit, deposit},
	})
	if !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("twice in one delta: err = %v, want ErrImportContract", err)
	}
	if len(refused) != 0 {
		t.Errorf("refused %+v, want none — this is the caller's mistake, not a candidate's", refused)
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 0 {
		t.Fatalf("journal holds %d operations, want 0", len(ops))
	}

	// Duplicates fail the delta even when the engine would refuse the rows
	// anyway.
	oversell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-oversell")
	if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{oversell, oversell},
	}); !errors.Is(err, operation.ErrImportContract) || len(refused) != 0 {
		t.Fatalf("a record repeated among refusable candidates: err = %v, refused %+v, want ErrImportContract and no refusals", err, refused)
	}

	// And again against a record already in the journal.
	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{deposit},
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{deposit},
	}); !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("second write of the same record: err = %v, want ErrImportContract", err)
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 1 {
		t.Errorf("journal holds %d operations, want 1", len(ops))
	}
}

func TestApplyImportDeltaRefusesACandidateWithoutIdentity(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	base := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-deposit")

	cases := map[string]func(o operation.Operation) operation.Operation{
		"no source": func(o operation.Operation) operation.Operation {
			o.Source = ""
			return o
		},
		"source manual": func(o operation.Operation) operation.Operation {
			o.Source = "manual"
			return o
		},
		"no external id": func(o operation.Operation) operation.Operation {
			o.ExternalID = nil
			return o
		},
		"empty external id": func(o operation.Operation) operation.Operation {
			empty := ""
			o.ExternalID = &empty
			return o
		},
	}
	for name, mutate := range cases {
		applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
			Add: []operation.Operation{mutate(base)},
		})
		if !errors.Is(err, operation.ErrImportContract) {
			t.Errorf("%s: err = %v, want ErrImportContract", name, err)
		}
		if len(applied) != 0 || len(refused) != 0 {
			t.Errorf("%s: applied %d and refused %d, want neither", name, len(applied), len(refused))
		}
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 0 {
		t.Errorf("journal holds %d operations, want 0", len(ops))
	}
}

// A corrected record replaces the one it corrects in a single delta: both
// carry the same external id, so removals go first.
func TestApplyImportDeltaRemovesBeforeItAdds(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	deposit := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-deposit")
	applied, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{deposit},
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	corrected := deposit
	corrected.AmountMinor = 2_000
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{applied[0].ID},
		Add:    []operation.Operation{corrected},
	})
	if err != nil {
		t.Fatalf("replacing a record: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want one applied", len(applied), refused)
	}
	ops, _ := journalOf(t, f, f.accountID)
	if len(ops) != 1 {
		t.Fatalf("journal holds %d operations, want 1", len(ops))
	}
	if ops[0].AmountMinor != 2_000 {
		t.Errorf("journal holds %d, want the corrected 2000", ops[0].AmountMinor)
	}
}

// A removal that breaks the journal is blamed on the removal, not on
// candidates that happen to share the delta.
func TestApplyImportDeltaBlamesTheRemovalThatBreaksTheJournal(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-sell")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, sell},
	})
	if err != nil || len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("setup: applied %d, refused %+v, err %v", len(applied), refused, err)
	}
	var buyID uuid.UUID
	for _, o := range applied {
		if o.Type == operation.TypeBuy {
			buyID = o.ID
		}
	}

	dividend := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeDividend,
		OccurredOn: date("2026-07-03"), AmountMinor: 5_000, Currency: "RUB",
	}, "op-dividend")
	applied, refused, err = svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{buyID},
		Add:    []operation.Operation{dividend},
	})
	if !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("removing the buy the sell needs: err = %v, want ErrInconsistent", err)
	}
	if len(refused) != 0 {
		t.Errorf("refused %+v, want none — the dividend did nothing wrong", refused)
	}
	if len(applied) != 0 {
		t.Errorf("applied %d, want none", len(applied))
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 2 {
		t.Errorf("journal holds %d operations, want the original 2", len(ops))
	}
}

// A broker correction (remove the old row, add the corrected one) is judged
// by the journal it leaves. Judging the removal alone leaves a sale with no
// purchase, and that once wedged the import for good: every rebuild computed the
// same difference, and the owner could not delete an imported row.
func TestApplyImportDeltaTakesACorrectionOfTheRowItRemoves(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-01-15"), Quantity: dec("100"),
		AmountMinor: -3_000_000, Currency: "RUB",
	}, "op-buy")
	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-03-10"), Quantity: dec("100"),
		AmountMinor: 3_200_000, Currency: "RUB",
	}, "op-sell")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, sell},
	})
	if err != nil || len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("setup: applied %d, refused %+v, err %v", len(applied), refused, err)
	}
	var buyID uuid.UUID
	for _, o := range applied {
		if o.Type == operation.TypeBuy {
			buyID = o.ID
		}
	}

	// The broker restates the purchase's commission; same external id.
	corrected := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-01-15"), Quantity: dec("100"),
		AmountMinor: -3_000_000, FeeMinor: 12_500, Currency: "RUB",
	}, "op-buy")
	applied, refused, err = svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{buyID},
		Add:    []operation.Operation{corrected},
	})
	if err != nil {
		t.Fatalf("correcting the purchase the sale rests on: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want the correction applied", len(applied), refused)
	}
	ops, _ := journalOf(t, f, f.accountID)
	if len(ops) != 2 {
		t.Fatalf("journal holds %d operations, want the corrected purchase and the sale", len(ops))
	}
	if ops[0].Type != operation.TypeBuy || ops[0].FeeMinor != 12_500 {
		t.Errorf("first row is %s with fee %d, want the buy with the corrected 12500",
			ops[0].Type, ops[0].FeeMinor)
	}
}

// Two purchases cover one sale and the broker restates both. One at a time,
// each replacement covers only part of the sale and both would be refused; the
// final journal is fine, so neither is.
func TestApplyImportDeltaJudgesEveryCandidateBeforeBlamingOne(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	first := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-01-15"), Quantity: dec("60"),
		AmountMinor: -1_800_000, Currency: "RUB",
	}, "op-buy-1")
	second := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-02-15"), Quantity: dec("40"),
		AmountMinor: -1_320_000, Currency: "RUB",
	}, "op-buy-2")
	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-03-10"), Quantity: dec("100"),
		AmountMinor: 3_200_000, Currency: "RUB",
	}, "op-sell")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{first, second, sell},
	})
	if err != nil || len(refused) != 0 || len(applied) != 3 {
		t.Fatalf("setup: applied %d, refused %+v, err %v", len(applied), refused, err)
	}
	var buyIDs []uuid.UUID
	for _, o := range applied {
		if o.Type == operation.TypeBuy {
			buyIDs = append(buyIDs, o.ID)
		}
	}
	if len(buyIDs) != 2 {
		t.Fatalf("seeded %d purchases, want 2", len(buyIDs))
	}

	firstAgain := first
	firstAgain.Quantity = dec("60")
	firstAgain.Note = "Покупка 60 шт."
	secondAgain := second
	secondAgain.Quantity = dec("40")
	secondAgain.Note = "Покупка 40 шт."
	applied, refused, err = svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: buyIDs,
		Add:    []operation.Operation{firstAgain, secondAgain},
	})
	if err != nil {
		t.Fatalf("restating both purchases at once: %v", err)
	}
	if len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("applied %d, refused %+v, want both restatements applied", len(applied), refused)
	}
	ops, _ := journalOf(t, f, f.accountID)
	if len(ops) != 3 {
		t.Fatalf("journal holds %d operations, want 3", len(ops))
	}
	if ops[0].Note != "Покупка 60 шт." || ops[1].Note != "Покупка 40 шт." {
		t.Errorf("journal notes are %q and %q, want the broker's restated wording on both",
			ops[0].Note, ops[1].Note)
	}
}

func TestApplyImportDeltaWillNotRemoveAManualOperation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	byHand, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("manual create: %v", err)
	}

	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{byHand.ID},
	}); !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("removing a manual operation: err = %v, want ErrImportContract", err)
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 1 {
		t.Errorf("journal holds %d operations, want the manual one still there", len(ops))
	}

	// An id that is not in the space at all is the same class of mistake.
	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{uuid.New()},
	}); !errors.Is(err, operation.ErrImportContract) {
		t.Errorf("removing an unknown id: err = %v, want ErrImportContract", err)
	}
}

// A delta's rows go out in one batch; same-day rows sharing a created_at
// would fold in either order, and in one the sell is an oversell. The stored
// order must be the checked one.
func TestApplyImportDeltaOrdersWhatItWroteTheWayItCheckedIt(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-sell")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, sell},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("applied %d, refused %+v, want both applied", len(applied), refused)
	}

	ops, positions := journalOf(t, f, f.accountID)
	if len(ops) != 2 {
		t.Fatalf("journal holds %d operations, want 2", len(ops))
	}
	if ops[0].Type != operation.TypeBuy || ops[1].Type != operation.TypeSell {
		t.Errorf("journal reads back as %s then %s, want buy then sell", ops[0].Type, ops[1].Type)
	}
	if ops[0].CreatedAt.Equal(ops[1].CreatedAt) {
		t.Errorf("both rows were created at %s — two rows the read path orders by created_at cannot share one",
			ops[0].CreatedAt)
	}
	if q := positions[f.sberID].Quantity; !q.IsZero() {
		t.Errorf("position quantity = %s, want 0", q)
	}
}

// A broker pages newest first, so a sell arrives before its buy; candidates
// are judged in fold order.
func TestApplyImportDeltaJudgesCandidatesInTheOrderTheyHappened(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-sell")
	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")

	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{sell, buy},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 2 {
		t.Fatalf("applied %d, refused %+v, want both applied", len(applied), refused)
	}
	ops, positions := journalOf(t, f, f.accountID)
	if len(ops) != 2 || ops[0].Type != operation.TypeBuy {
		t.Errorf("journal reads back as %+v, want the buy first", ops)
	}
	if pnl := portfoliotest.Realized(t, positions[f.sberID]); pnl != 20_000 {
		t.Errorf("realized = %d, want 20000", pnl)
	}
}

// Removing half of a transfer is refused: a lone arriving leg replays fine,
// so the engine cannot see the break.
func TestApplyImportDeltaWillNotRemoveHalfATransfer(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	out, in := pairOfLegs(f, uuid.New(), "10", "2026-07-05")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{buy, out, in},
	})
	if err != nil || len(refused) != 0 || len(applied) != 3 {
		t.Fatalf("setup: applied %d, refused %+v, err %v", len(applied), refused, err)
	}

	var legs []uuid.UUID
	for _, o := range applied {
		if o.Type == operation.TypeTransferIn || o.Type == operation.TypeTransferOut {
			legs = append(legs, o.ID)
		}
	}
	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: legs[:1],
	}); !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("removing one leg of a pair: err = %v, want ErrImportContract", err)
	}
	if ops, _ := journalOf(t, f, f.account2ID); len(ops) != 1 {
		t.Errorf("destination journal holds %d operations, want the arriving leg untouched", len(ops))
	}

	// Both legs together is what a rebuild would ask for, and it is allowed.
	if _, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: legs,
	}); err != nil {
		t.Fatalf("removing the whole pair: %v", err)
	}
	if ops, _ := journalOf(t, f, f.account2ID); len(ops) != 0 {
		t.Errorf("destination journal holds %d operations, want none", len(ops))
	}
}

// A new row folds after everything its date already holds. The seeded buy is
// stamped an hour ahead, as a previous long sync leaves rows; ordering by the
// clock alone would put the sell before it.
func TestApplyImportDeltaFoldsACandidateAfterWhatItsDateAlreadyHolds(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy")
	buy.CreatedAt = time.Now().UTC().Add(time.Hour)
	if _, err := f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{buy}, nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sell := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-sell")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{sell},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want the sell applied", len(applied), refused)
	}
	ops, positions := journalOf(t, f, f.accountID)
	if len(ops) != 2 || ops[0].Type != operation.TypeBuy {
		t.Errorf("journal reads back as %+v, want the buy first", ops)
	}
	if pnl := portfoliotest.Realized(t, positions[f.sberID]); pnl != 20_000 {
		t.Errorf("realized = %d, want 20000", pnl)
	}
}

// New stamps start after the youngest row that survives the removals. op-young
// is stamped far ahead and is being removed; op-old stays. The replacement must be
// stamped near now, not near op-young.
func TestApplyImportDeltaBasesTimestampsOnWhatSurvivesRemoval(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	old := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-old")
	old.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	young := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 2_000, Currency: "RUB",
	}, "op-young")
	young.CreatedAt = time.Now().UTC().Add(48 * time.Hour)
	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{old, young}, nil, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var youngID uuid.UUID
	for _, o := range stored {
		if o.AmountMinor == 2_000 {
			youngID = o.ID
		}
	}

	replacement := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 3_000, Currency: "RUB",
	}, "op-new")
	before := time.Now().UTC()
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{youngID},
		Add:    []operation.Operation{replacement},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want the replacement applied", len(applied), refused)
	}
	if applied[0].CreatedAt.Before(before) || applied[0].CreatedAt.After(before.Add(time.Minute)) {
		t.Errorf("new row created_at = %s, want within a minute of %s — the row being removed (stamped 48h ahead) must not raise the floor",
			applied[0].CreatedAt, before)
	}
}

// A row replacing one the delta removes may inherit its created_at and keep
// its place. The two deposits differ only in their stamps; without the old stamp
// the first would move behind the second.
func TestApplyImportDeltaLetsARewrittenRowKeepItsPlace(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	early := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-early")
	early.CreatedAt = time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	late := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 2_000, Currency: "RUB",
	}, "op-late")
	late.CreatedAt = time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID, []operation.Operation{early, late}, nil, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var earlyID uuid.UUID
	var earlyAt time.Time
	for _, o := range stored {
		if o.AmountMinor == 1_000 {
			earlyID, earlyAt = o.ID, o.CreatedAt
		}
	}

	rewritten := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_500, Currency: "RUB",
	}, "op-early")
	rewritten.CreatedAt = earlyAt
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Remove: []uuid.UUID{earlyID},
		Add:    []operation.Operation{rewritten},
	})
	if err != nil {
		t.Fatalf("ApplyImportDelta: %v", err)
	}
	if len(refused) != 0 || len(applied) != 1 {
		t.Fatalf("applied %d, refused %+v, want the rewrite applied", len(applied), refused)
	}
	if !applied[0].CreatedAt.Equal(earlyAt) {
		t.Errorf("rewritten row created_at = %s, want the %s it inherited", applied[0].CreatedAt, earlyAt)
	}
	ops, _ := journalOf(t, f, f.accountID)
	if len(ops) != 2 {
		t.Fatalf("journal holds %d operations, want 2", len(ops))
	}
	if ops[0].AmountMinor != 1_500 || ops[1].AmountMinor != 2_000 {
		t.Errorf("the day reads back as %d then %d, want 1500 then 2000 — the rewrite kept its place",
			ops[0].AmountMinor, ops[1].AmountMinor)
	}
}

// A created_at from no removed row is refused: it would let an importer
// choose where a row folds, and so the realized profit.
func TestApplyImportDeltaRefusesATimestampItCannotHaveInherited(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	invented := imported(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1_000, Currency: "RUB",
	}, "op-invented")
	invented.CreatedAt = time.Now().UTC().Add(-72 * time.Hour)
	_, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{invented},
	})
	if !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("a stamp inherited from nothing: err = %v, want ErrImportContract", err)
	}
	if ops, _ := journalOf(t, f, f.accountID); len(ops) != 0 {
		t.Errorf("journal holds %d operations, want none written", len(ops))
	}
}

// The lock covers one account, so a delta built under it may write only
// there.
func TestBuildAndApplyImportDeltaIsConfinedToTheAccountItLocked(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	_, _, _, err := svc.BuildAndApplyImportDelta(f.ctx, f.spaceID, f.accountID,
		func([]operation.Operation) (operation.ImportDelta, error) {
			return operation.ImportDelta{Add: []operation.Operation{imported(operation.Operation{
				AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
				OccurredOn: date("2026-03-02"), Quantity: dec("1"), Price: dec("100"),
				AmountMinor: -10_000, Currency: "RUB",
			}, "other-account")}}, nil
		})
	if !errors.Is(err, operation.ErrImportContract) {
		t.Fatalf("err = %v, want ErrImportContract", err)
	}
	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	if len(journal) != 0 {
		t.Errorf("the other account's journal holds %d rows, want none", len(journal))
	}
}
