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
)

// imported dresses an operation the way an importer must hand it over: a
// source that is not a person's own entry, and the id of the broker record it
// was projected from.
func imported(op operation.Operation, externalID string) operation.Operation {
	op.Source = "tinvest"
	op.ExternalID = &externalID
	return op
}

// journalOf reads the account's journal back the way every later read does —
// through the engine's own listing — and folds it. Assertions go through this
// rather than through the rows ApplyImportDelta returned, because the fault
// this whole path guards against is precisely a difference between the two.
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

// TestApplyImportDeltaIsolatesTheCandidateThatCannotApply is the reason the
// candidates are checked one at a time before anything is written: a broker
// operation this program cannot record must become visible on its own, with the
// real reason, while the rest of the history still loads.
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

// TestApplyImportDeltaRefusesASplitFromTheImporter pins that a split is
// refused UNCONDITIONALLY by the import path itself (validateImported's own
// TypeSplit case) — not merely because validateByType happens to refuse the
// same candidate for some unrelated reason of its own.
//
// The candidate below carries no instrument, which validateByType's TypeSplit
// case refuses first and for a completely different reason ("split requires
// an instrument", checked before it ever looks at Source). Checking only
// errors.Is(err, family.ErrValidation) cannot tell the two apart — both are
// ErrValidation — so if validateImported ever stopped naming splits itself and
// fell through to validateByType, this candidate would still be refused, just
// for the wrong reason. The message is checked for the one phrase only the
// import path's own refusal carries.
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

// TestApplyImportDeltaRefusesABreakdownFromAnyoneButTheRegistry pins the ONE
// hole in "the parcel is computed here, not supplied".
//
// The hole was opened for the corporate-actions registry, whose pairs cannot
// have their breakdown computed from the journal: how many units of the new
// paper N of the old become, and what share of the basis a spin-off carves
// out, live in the registry and nowhere else. Everything else must keep the
// old refusal — a broker's transfer names a quantity and the FIFO queue
// answers the rest, so a breakdown arriving with it is a cost basis somebody
// invented, and this path exists to make that impossible.
//
// WITHOUT THIS CASE THE GUARD IS UNTESTED: opening it to every caller (the
// function simply returning true) leaves the whole suite green, because no
// fixture ever supplies a breakdown it is not entitled to. The mutation is the
// point of the test.
func TestApplyImportDeltaRefusesABreakdownFromAnyoneButTheRegistry(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	on := date("2021-01-08")
	// An ordinary imported transfer, arriving with a parcel of its own: the
	// shape the importer is never allowed to send.
	transfer := imported(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: 100_000,
		Currency: "RUB",
		TransferLots: []operation.ReleasedLot{
			{Quantity: *dec("10"), CostMinor: 100_000, AcquiredOn: &on},
		},
	}, "op-transfer-with-its-own-parcel")

	applied, _, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
		Add: []operation.Operation{transfer},
	})
	// THE WHOLE DELTA FAILS, and this row is not merely refused among others:
	// a candidate carrying a parcel it did not earn is a caller breaking the
	// contract, not a row the journal cannot take — the difference between a
	// bug in the program and news about the broker's data.
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

// TestApplyImportDeltaTakesATaxThatGaveMoneyBack is the second point on which
// the import path differs from the hand-entry one, and it is worth its own test
// because the difference is a single sign.
//
// A broker's tax CORRECTION arrives positive: of the nine on the owner's own
// account seven are. Refused, they were seven real credits missing from the
// journal for as long as the account existed. Nothing downstream needed
// teaching — a tax is folded into income by its signed amount, so a refund
// restores exactly what the withholding took.
//
// A ZERO is still refused, because money that did not move corrects nothing.
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

// TestCreateStillRefusesATaxThatIsNotACharge is the other half: the hand-entry
// path keeps the guard the import path drops. There a positive tax is a sign
// somebody typed wrong, and refusing it catches the mistake while it can still
// be fixed; here the sign is the broker's own statement about money that moved.
//
// The two paths differ on exactly two rules and this is one of them, so a change
// that "simplified" them into one would take this guard away from the door where
// it earns its keep.
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

// pairOfLegs builds the two legs of one transfer, already carrying the shared
// group id its caller computed — which is how a delta presents a pair.
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

// TestApplyImportDeltaRefusesBothLegsWhenOneIsRefused pins that a transfer is
// one event: a pair half-written would leave shares in an account nothing ever
// sent them from.
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

// TestApplyImportDeltaPairCarriesTheDatesItMoved is the property the whole
// breakdown mechanism exists for, on the import path: the receiving account
// keeps the day the shares were BOUGHT, not the day they changed accounts —
// and the breakdown is computed here, from the journal, rather than taken from
// the caller, which cannot know it.
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
	// The basis is the one the journal holds, computed here — 100 000 minor
	// units for the lot that was bought — on both legs alike.
	if storedOut.AmountMinor != 100_000 || storedIn.AmountMinor != 100_000 {
		t.Errorf("legs carry %d / %d, want the released basis 100000 on both",
			storedOut.AmountMinor, storedIn.AmountMinor)
	}
	if len(storedOut.TransferLots) != 1 || len(storedIn.TransferLots) != 1 {
		t.Fatalf("legs carry %d / %d pieces, want one on each",
			len(storedOut.TransferLots), len(storedIn.TransferLots))
	}
	// The rows live next to the arriving leg only — the same place CreatePair
	// puts them, since attachTransferLots resolves the departing leg through
	// the group at every later read.
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

// TestApplyImportDeltaLoneTransferInArrivesUndated covers what the manual path
// refuses outright and the importer cannot do without: shares that came from
// another broker have no second leg anywhere in this system, and no purchase
// dates to recover either.
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

// TestApplyImportDeltaTransfersPartOfAnUndatedLot covers a composition only
// the import path can produce and nothing had exercised before: a lone
// transfer_in with no acquisition date (see
// TestApplyImportDeltaLoneTransferInArrivesUndated — shares that arrived from
// a broker outside this program, which never says when they were bought),
// followed by an ordinary import transfer PAIR that moves part of that very
// lot on to a third account.
//
// The path is legal — migration 0008 dropped
// operation_transfer_lots.acquired_on's NOT NULL, and portfolio.CheckTransferLots
// admits an empty date outright — but nothing had reached it: the manual path
// can only ever produce an undated lot through CostMinorOverride, never
// through a transfer's own FIFO release, so every existing test of a real
// release (TestApplyImportDeltaPairCarriesTheDatesItMoved,
// TestApplyImportDeltaLoneTransferOutFreezesWhatLeft) starts from a dated buy.
// It matters because a lot with no acquisition date is FIRST out of the FIFO
// queue (see the portfolio package's own doc) — a mistake in carrying the
// absence through a second transfer would misorder every later sale that
// touches shares mirrored in from another broker.
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

// TestApplyImportDeltaLoneTransferOutFreezesWhatLeft pins that a departing leg
// with no sibling still records WHICH lots went, rather than leaving a later
// read to work it out again from a queue that may have moved on.
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
	// With no sibling to hold them, the pieces are stored next to the departing
	// leg itself — which is where attachTransferLots looks when there is no peer.
	if n := f.lotRows(t, storedOut.ID); n != 1 {
		t.Errorf("lot rows on the lone departing leg = %d, want 1", n)
	}

	_, positions := journalOf(t, f, f.accountID)
	if c := positions[f.sberID].CostMinor; c != 200_000 {
		t.Errorf("remaining basis = %d, want 200000 — the newer lot is what is left", c)
	}
}

// TestApplyImportDeltaRefusesADeltaThatRepeatsAnExternalID pins the one class
// of trouble that is NOT a refused candidate: a delta that names the same
// broker record twice means the caller computed the difference wrongly, and
// swallowing it would write half a mistake.
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

	// It is the delta being wrong that matters, not whether the rows would
	// have applied: two copies of a candidate the engine refuses anyway are
	// still a difference computed wrongly, and must not come back as an
	// ordinary refusal for the owner to puzzle over.
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

// TestApplyImportDeltaRemovesBeforeItAdds is what lets a corrected broker
// record replace the one it corrects inside a single delta: the two carry the
// same external id, and the journal will not hold both.
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

// TestApplyImportDeltaBlamesTheRemovalThatBreaksTheJournal pins WHOSE fault a
// refusal is said to be. A removal that leaves the journal unable to replay is
// the caller's own difference being wrong; blaming the candidates that happen
// to be in the same delta would name a reason that is not the reason.
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

// TestApplyImportDeltaTakesACorrectionOfTheRowItRemoves is the fault that
// wedged an import for good, and the reason the delta is judged by the journal
// it would LEAVE rather than by the one that stands halfway through it.
//
// A broker that corrects an operation after the fact — a commission it
// restated, a description it reworded — produces a difference that says "take
// the old row out, put the corrected one in". Judging the removals on their own
// asks a question nobody wanted answered: what would this journal be if the
// purchase were simply gone? A sale with no purchase behind it is the answer,
// so the engine refuses, and the refusal was fatal to the whole difference.
// Nothing then reached the journal — not this hour and not any later one, since
// every rebuild computes the same difference — and no unparsed row named a
// cause. The owner could not even repair it by hand: an operation whose source
// is not a person's own entry is not theirs to delete.
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

	// The broker restates the purchase's commission. Same record, same
	// external id, so the row it corrects has to go before it can be written.
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

// TestApplyImportDeltaJudgesEveryCandidateBeforeBlamingOne is the same rule one
// step further: the state that has to hold is the FINAL one, and no candidate
// is blamed for a journal that only the middle of the difference ever holds.
//
// Two purchases cover one sale here, and the broker restates both of them at
// once. Offering the candidates one at a time — each against the journal the
// removals left — refuses both: the first replacement covers sixty of the
// hundred sold, the second forty. Neither is at fault, and refusing them would
// name a reason ("sold more than the account holds") that is true of no journal
// this delta was ever going to produce.
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

// TestApplyImportDeltaOrdersWhatItWroteTheWayItCheckedIt is the batch's own
// version of "what was checked is what is read back". A delta's rows go out
// back to back in one batch, and a clock left to date them is not a promise
// that two of them differ — while two operations of the same date sharing one
// created_at leave the read path's ORDER BY nothing to order them by. The buy
// and the sell below would then fold in either order, and in one of those
// orders the sell is an oversell: accepted on write, refused on every later
// read, which is the fault this project has already met twice.
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

// TestApplyImportDeltaJudgesCandidatesInTheOrderTheyHappened covers the shape a
// broker's own paging hands over: newest first. The sell arrives in the delta
// before the buy that covers it, and judging them in that order would refuse a
// perfectly ordinary week of trading as an oversell.
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
	if pnl := realizedOf(t, positions[f.sberID]); pnl != 20_000 {
		t.Errorf("realized = %d, want 20000", pnl)
	}
}

// TestApplyImportDeltaWillNotRemoveHalfATransfer covers the one break the
// engine cannot see. A lone arriving leg is perfectly legal — it is what shares
// from another broker look like — so an account left holding one after its
// departing half was removed replays cleanly and for ever, while the shares in
// it came from nowhere.
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

// TestApplyImportDeltaFoldsACandidateAfterWhatItsDateAlreadyHolds pins where a
// new row lands among the rows its date already carries: after all of them.
//
// The seeded buy below is dated today and stamped an hour ahead, which is what
// a delta's own numbering leaves behind — rows are stamped a microsecond apart
// from the moment the sync began, so a long one reaches past that moment, and a
// sync that follows would start underneath it. Ordering by the clock alone
// would then fold this sell before the buy that covers it and refuse an
// ordinary sale.
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
	if pnl := realizedOf(t, positions[f.sberID]); pnl != 20_000 {
		t.Errorf("realized = %d, want 20000", pnl)
	}
}

// TestApplyImportDeltaBasesTimestampsOnWhatSurvivesRemoval pins that the row a
// delta's own numbering starts AFTER (see base in ApplyImportDelta) is read
// from the journal as the removals leave it, not as it stood before them. A
// row this same delta is about to delete must not go on raising that floor —
// it will not be there to share a date with anything once the delta commits.
//
// The seeded pair below makes the two readings disagree by two days: op-young
// is stamped far in the future and is the one being removed, op-old is
// stamped in the past and is the one left behind. Basing the floor on the
// journal before removal would number the replacement row near op-young's
// stamp, days from now; basing it on what survives numbers it near "now".
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

// TestApplyImportDeltaLetsARewrittenRowKeepItsPlace is the write path's half of
// the rule the importer's rewrite rests on: a row put back in place of one this
// same delta removes may inherit that row's created_at, and then it folds where
// it folded before instead of at the end of its day.
//
// The two deposits below are one day apart in nothing but their stamps, and the
// journal reads a day in stamp order. Replacing the FIRST of them without its
// old stamp moves it behind the second.
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

// TestApplyImportDeltaRefusesATimestampItCannotHaveInherited pins the limit on
// the rule above. A created_at that belongs to no row this delta removes is not
// a rewrite keeping its place — it is an importer choosing where in a day an
// operation folds, which decides which parcel a later sale consumes and
// therefore what the realized profit is. It is the caller's own doing, so it is
// fatal to the delta rather than a refusal of the candidate.
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

// realizedOf is a position's realized result, and it FAILS THE TEST when the
// position has none — a disposal that settled in another currency leaves no
// figure in any single one (see portfolio.Position.RealizedPnL). Every call
// below therefore asserts two things at once: the number, and that there is a
// number, which is what keeps a test from quietly comparing a zero against a
// zero the moment the currency rule starts refusing to answer.
func realizedOf(t *testing.T, p *portfolio.Position) int64 {
	t.Helper()
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		t.Fatalf("position %s has no realized result in one currency: a disposal settled in another", p.InstrumentID)
	}
	return minor
}
