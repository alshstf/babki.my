package tinvest

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// findByExternalID is byExternalID for the tests that expect an entry to be
// ABSENT: the shared helper fails the test when it finds nothing, which is the
// opposite of what an explained row has to prove.
func findByExternalID(ops []operation.Operation, id string) (operation.Operation, bool) {
	for _, o := range ops {
		if o.ExternalID != nil && *o.ExternalID == id {
			return o, true
		}
	}
	return operation.Operation{}, false
}

// uidTechFund is the fund whose partial redemption the owner's own account
// carries — the case this whole feature exists for.
const uidTechFund = "7c3f9a2e-1111-4222-8333-abcdefabcdef"

// techFundRows builds the three broker rows of the owner's live October 2025:
// a purchase, the withdrawal of the redeemed units ("вывод в другой
// депозитарий", the fraction only in the prose), and the money a fortnight
// later under the bond-redemption type. The last two are ONE partial
// redemption and no rule can see it — which is what the owner explains.
func techFundRows(t *testing.T, f *rebuildFixture) (buy, out, payout OperationItem) {
	t.Helper()
	f.src.instruments[uidTechFund] = InstrumentBrief{
		UID: uidTechFund, FIGI: "TCS20A101X68", ISIN: "RU000A101X68",
		Ticker: "TECH", Name: "Технологии Америки", Currency: "RUB", InstrumentType: "etf",
	}

	buy = loadOperationItem(t, "buy.json")
	buy.ID = "op-tech-buy"
	buy.InstrumentUID, buy.FIGI, buy.InstrumentType = uidTechFund, "TCS20A101X68", "etf"

	out = loadOperationItem(t, "output_securities.json")
	out.ID = "op-tech-out"
	out.InstrumentUID, out.FIGI, out.InstrumentType = uidTechFund, "TCS20A101X68", "etf"
	out.Quantity = 30
	out.Description = "Вывод 30.5 лотов фонда Технологии Америки в другой депозитарий"
	// A date of its own, for the reason the fund-payout test states: the
	// fixture's own is in the future and the journal refuses that.
	out.Date = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)

	payout = loadOperationItem(t, "bond_repayment_full_no_quantity.json")
	payout.ID = "op-tech-payout"
	payout.InstrumentUID, payout.FIGI, payout.InstrumentType = uidTechFund, "TCS97A101X68", "etf"
	return buy, out, payout
}

// explainRows is the store half of what the service does, for the tests that
// are about the projection rather than about the service.
func (f *rebuildFixture) explainRows(t *testing.T, opID uuid.UUID, brokerIDs ...string) {
	t.Helper()
	keys := make([]string, 0, len(brokerIDs))
	for _, id := range brokerIDs {
		keys = append(keys, f.mirrorRow(t, f.link, id).ContentKey)
	}
	if err := f.store.CreateExplanations(f.ctx, f.link.ID, opID, keys); err != nil {
		t.Fatalf("CreateExplanations: %v", err)
	}
}

// manualRedemption is the operation the owner enters for the two rows: the
// units that were actually redeemed, and the money that came for them.
func (f *rebuildFixture) manualRedemption(t *testing.T, instrumentID uuid.UUID) operation.Operation {
	t.Helper()
	qty := decimal.RequireFromString("30.5")
	op, err := operation.NewService(f.ops).Create(f.ctx, f.spaceID, operation.Operation{
		AccountID:    f.accountID,
		InstrumentID: &instrumentID,
		Type:         operation.TypeRedemption,
		OccurredOn:   time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		Quantity:     &qty,
		AmountMinor:  255980,
		Currency:     "RUB",
		Note:         "частичное погашение паёв фонда",
	})
	if err != nil {
		t.Fatalf("create the manual redemption: %v", err)
	}
	return op
}

// TestRebuildSkipsExplainedRowsAndStopsCountingThemUnparsed is the whole
// feature end to end, on the owner's own shape: the withdrawal and the payout
// are explained by one manual redemption, so the rebuild writes neither of
// them, leaves neither on the unparsed count, and the position afterwards is
// what the owner's own operation says it is.
func TestRebuildSkipsExplainedRowsAndStopsCountingThemUnparsed(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := techFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)

	// The first rebuild is the state this feature starts from: the payout is
	// unparsed and the withdrawal is projected as a transfer to another broker,
	// which is what it is NOT.
	before := f.rebuild(t)
	if before.Unparsed != 1 {
		t.Fatalf("before explaining, %d rows are unparsed, want 1 — the payout", before.Unparsed)
	}
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-tech-buy"), 1)).InstrumentID

	manual := f.manualRedemption(t, instrumentID)
	f.explainRows(t, manual.ID, "op-tech-out", "op-tech-payout")

	after := f.rebuild(t)
	if after.Unparsed != 0 {
		t.Errorf("after explaining, %d rows are unparsed, want 0 — an explained row is not one this program could not read", after.Unparsed)
	}
	if after.Removed != 1 {
		t.Errorf("the rebuild removed %d operations, want 1 — the transfer the withdrawal used to be", after.Removed)
	}

	for _, id := range []string{"op-tech-out", "op-tech-payout"} {
		row := f.mirrorRow(t, f.link, id)
		if row.UnparsedReason != "" {
			t.Errorf("%s carries reason %q, want none: the owner answered for it, and «could not be read» is not what is true of it",
				id, row.UnparsedReason)
		}
	}

	journal := f.journalOf(t, f.accountID)
	for _, id := range []string{"op-tech-out", "op-tech-payout"} {
		row := f.mirrorRow(t, f.link, id)
		// Legs are 1-based (see externalIDFor); two of them is more than any
		// shape in this package produces, so an entry under either is one
		// entry too many.
		for leg := 1; leg <= 2; leg++ {
			if op, ok := findByExternalID(journal, externalIDFor(row, leg)); ok {
				t.Errorf("%s still produced journal entry %s (%s): an explained row is accounted for by the manual operation, and writing it too would record the event twice",
					id, op.ID, op.Type)
			}
		}
	}

	// The position is the purchase minus what the owner said was redeemed, and
	// it replays — which is the point of putting the manual operation through
	// the journal's own door.
	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	fund := positions[instrumentID]
	if got := fund.Quantity.String(); got != "69.5" {
		t.Errorf("the fund position is %s units, want 69.5 — 100 bought less the 30,5 the owner redeemed", got)
	}
}

// TestUnparsedListShowsExplainedRowsTheCountDoesNot pins the pair this feature
// has to keep in step: an explained row is NOT counted as unparsed (it carries
// no reason) and IS on the list (this is the only screen it appears on, and the
// only place the answer can be taken back). Both halves are asserted against
// the same two rows, because it is the DIFFERENCE between them that a reader
// would otherwise take for a bug.
func TestUnparsedListShowsExplainedRowsTheCountDoesNot(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := techFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-tech-buy"), 1)).InstrumentID
	manual := f.manualRedemption(t, instrumentID)
	f.explainRows(t, manual.ID, "op-tech-out", "op-tech-payout")
	f.rebuild(t)

	count, err := f.store.unparsedCountByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("unparsedCountByLink: %v", err)
	}
	if count != 0 {
		t.Errorf("the link counts %d unparsed rows, want 0 — both are explained", count)
	}

	rows, hasMore, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 50, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore is true on a page of %d rows", len(rows))
	}
	if len(rows) != 2 {
		t.Fatalf("the list has %d rows, want 2 — the two explained ones, which live nowhere else", len(rows))
	}
	// The list and the count disagree BY EXACTLY the explained rows, and every
	// one of them says which operation explains it.
	explained := 0
	for _, m := range rows {
		if m.ExplainedBy == nil {
			t.Errorf("row %s is on the list with no explanation and no reason — it is neither unparsed nor explained, which is not a state", m.BrokerOperationID)
			continue
		}
		explained++
		if m.ExplainedBy.OperationID != manual.ID {
			t.Errorf("row %s names operation %s, want the manual redemption %s", m.BrokerOperationID, m.ExplainedBy.OperationID, manual.ID)
		}
		if m.ExplainedBy.OperationType != string(operation.TypeRedemption) {
			t.Errorf("row %s names a %s, want redemption", m.BrokerOperationID, m.ExplainedBy.OperationType)
		}
		if got := m.ExplainedBy.OperationOn.Format("2006-01-02"); got != "2026-05-21" {
			t.Errorf("row %s dates its explanation %s, want 2026-05-21 — the operation's own day, which is neither broker row's", m.BrokerOperationID, got)
		}
	}
	if explained != len(rows)-count {
		t.Errorf("%d rows on the list are explained, but list (%d) minus count (%d) is %d: the two are supposed to differ by exactly the explained rows",
			explained, len(rows), count, len(rows)-count)
	}
}

// TestDeletingTheManualOperationUnexplainsItsRows is the un-explaining rule,
// asserted where it is implemented — the foreign key. Nothing in this package
// deletes an explanation beside the operation, so this is what guarantees that
// an explanation cannot outlive what it names, whether the operation goes
// through this feature's own DELETE or through the journal screen.
func TestDeletingTheManualOperationUnexplainsItsRows(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := techFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-tech-buy"), 1)).InstrumentID
	manual := f.manualRedemption(t, instrumentID)
	f.explainRows(t, manual.ID, "op-tech-out", "op-tech-payout")
	f.rebuild(t)

	if err := operation.NewService(f.ops).Delete(f.ctx, f.spaceID, manual.ID); err != nil {
		t.Fatalf("delete the manual operation: %v", err)
	}
	keys, err := f.store.ExplainedKeysByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("ExplainedKeysByLink: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("%d explanations outlived the operation they name", len(keys))
	}

	// And the rows come back to what they were: the payout unparsed again.
	after := f.rebuild(t)
	if after.Unparsed != 1 {
		t.Errorf("after the operation was deleted, %d rows are unparsed, want 1 — the payout is nobody's answer any more", after.Unparsed)
	}
	if row := f.mirrorRow(t, f.link, "op-tech-payout"); row.UnparsedReason != string(ReasonFundPayoutUnitsUnknown) {
		t.Errorf("the payout's reason is %q, want fund_payout_units_unknown", row.UnparsedReason)
	}
}

// TestBrokerFeeOfAnExplainedTradeIsItsOwnUnparsedRow is the fourth answer to
// "is this commission a duplicate": its trade is neither in the journal nor on
// the unparsed list, because the owner accounted for it by hand. Dropping the
// fee on the strength of the unparsed-trade rule would lose the money in
// silence — that rule leans on the trade's own row reporting it, and an
// explained row reports nothing.
func TestBrokerFeeOfAnExplainedTradeIsItsOwnUnparsedRow(t *testing.T) {
	f := newRebuildFixture(t)
	trade := loadOperationItem(t, "buy.json")
	trade.ID = "op-trade"
	fee := loadOperationItem(t, "broker_fee.json")
	fee.ID = "op-fee"
	fee.ParentOperationID = "op-trade"
	fee.InstrumentUID, fee.FIGI, fee.InstrumentType = trade.InstrumentUID, trade.FIGI, trade.InstrumentType
	f.sync(t, f.link, trade, fee)
	f.rebuild(t)

	// Any manual operation will do here: what is under test is the fee's
	// verdict, which depends on the trade being explained and not on what it
	// was explained with.
	manual, err := operation.NewService(f.ops).Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn:  time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		AmountMinor: 1000, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create the manual operation: %v", err)
	}
	f.explainRows(t, manual.ID, "op-trade")
	f.rebuild(t)

	row := f.mirrorRow(t, f.link, "op-fee")
	if row.UnparsedReason != string(ReasonBrokerFeeParentExplained) {
		t.Errorf("the commission's reason is %q, want broker_fee_parent_explained — its trade is accounted for by hand, so neither booking nor dropping the charge is this program's answer to make",
			row.UnparsedReason)
	}
	journal := f.journalOf(t, f.accountID)
	if op, ok := findByExternalID(journal, externalIDFor(row, 1)); ok {
		t.Errorf("the commission was booked as %s: whether the manual entry already includes it is the owner's to say", op.ID)
	}
}

// TestExplainRowsRefusesWhatItCannotAccountFor covers the two refusals the
// contract promises by status code, and the account rule that the client is
// deliberately not trusted with.
func TestExplainRowsRefusesWhatItCannotAccountFor(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := techFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-tech-buy"), 1)).InstrumentID

	svc := NewService(f.store, nil, operation.NewService(f.ops), f.ops, nil, nil, &fakeInserter{}, slog.Default())
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	qty := decimal.RequireFromString("30.5")
	newOp := func() operation.Operation {
		return operation.Operation{
			// An account this link does not name, on purpose: the service is
			// supposed to overwrite it rather than believe it.
			AccountID:    uuid.New(),
			InstrumentID: &instrumentID,
			Type:         operation.TypeRedemption,
			OccurredOn:   time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
			Quantity:     &qty,
			AmountMinor:  255980,
			Currency:     "RUB",
		}
	}

	if _, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID, []string{"not-a-key"}, newOp()); err == nil ||
		!errors.Is(err, ErrRowNotInLink) {
		t.Errorf("explaining an unknown content key returned %v, want ErrRowNotInLink", err)
	}
	// And nothing was written for it — the keys are checked BEFORE the journal
	// is touched, which is what makes the two halves safe without one
	// transaction over both.
	if ops := f.journalOf(t, f.accountID); len(ops) != 2 {
		t.Errorf("the journal holds %d operations after a refused explanation, want the 2 the import wrote", len(ops))
	}

	outKey := f.mirrorRow(t, f.link, "op-tech-out").ContentKey
	explained, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID, []string{outKey}, newOp())
	if err != nil {
		t.Fatalf("ExplainRows: %v", err)
	}
	stored, err := operation.NewStore(f.pool).ByID(f.ctx, f.spaceID, explained.OperationID)
	if err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if stored.AccountID != f.accountID {
		t.Errorf("the operation landed on account %s, want the linked one %s — the request's own account_id is not to be believed here",
			stored.AccountID, f.accountID)
	}

	if _, _, err := svc.ExplainRows(f.ctx, owner, f.link.ID, []string{outKey}, newOp()); err == nil ||
		!errors.Is(err, ErrRowAlreadyExplained) {
		t.Errorf("explaining an already-explained row returned %v, want ErrRowAlreadyExplained", err)
	}
}

// TestRemoveExplanationTakesTheOperationWithIt is the DELETE half: the point
// of the endpoint is that the journal entry goes too, since an operation left
// behind would be an entry explaining nothing, invisible on this screen and
// double-counting the event on the account.
func TestRemoveExplanationTakesTheOperationWithIt(t *testing.T) {
	f := newRebuildFixture(t)
	buy, out, payout := techFundRows(t, f)
	f.sync(t, f.link, buy, out, payout)
	f.rebuild(t)
	instrumentID := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-tech-buy"), 1)).InstrumentID

	inserter := &fakeInserter{}
	svc := NewService(f.store, nil, operation.NewService(f.ops), f.ops, nil, nil, inserter, slog.Default())
	owner := family.Principal{SpaceID: f.spaceID, Role: family.RoleOwner}
	qty := decimal.RequireFromString("30.5")
	created, queued, err := svc.ExplainRows(f.ctx, owner, f.link.ID,
		[]string{f.mirrorRow(t, f.link, "op-tech-out").ContentKey},
		operation.Operation{
			InstrumentID: &instrumentID, Type: operation.TypeRedemption,
			OccurredOn: time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
			Quantity:   &qty, AmountMinor: 255980, Currency: "RUB",
		})
	if err != nil {
		t.Fatalf("ExplainRows: %v", err)
	}
	if !queued {
		t.Error("no sync was queued for an active connection, so the journal would keep the row it was told to stop projecting")
	}

	rows, _, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 50, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	var explanationID uuid.UUID
	for _, m := range rows {
		if m.ExplainedBy != nil {
			explanationID = m.ExplainedBy.ID
		}
	}
	if explanationID == uuid.Nil {
		t.Fatal("no listed row carries the explanation just written")
	}

	if _, err := svc.RemoveExplanation(f.ctx, owner, explanationID); err != nil {
		t.Fatalf("RemoveExplanation: %v", err)
	}
	if _, err := operation.NewStore(f.pool).ByID(f.ctx, f.spaceID, created.OperationID); err == nil {
		t.Error("the manual operation is still in the journal: it now explains nothing and shows nowhere, while its money still counts")
	}
	keys, err := f.store.ExplainedKeysByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("ExplainedKeysByLink: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("%d explanations survived their operation", len(keys))
	}
}
