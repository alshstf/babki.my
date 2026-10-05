package operation_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

// at is an instant on a journal day, as a broker reports one.
func at(day, clock string) *time.Time {
	t, err := time.Parse("2006-01-02 15:04", day+" "+clock)
	if err != nil {
		panic(err)
	}
	return &t
}

// timed is an imported row carrying the broker's instant.
func timed(op operation.Operation, externalID string, when *time.Time) operation.Operation {
	op = imported(op, externalID)
	op.OccurredAt = when
	return op
}

// A purchase the broker reported late folds at its own instant: of two
// parcels bought one day, a later sale consumes the earlier one (#198).
func TestALatePurchaseFoldsAtItsInstant(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	buy := func(ext, clock string, price string, amount int64) operation.Operation {
		return timed(operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec(price),
			AmountMinor: amount, Currency: "RUB",
		}, ext, at("2026-07-02", clock))
	}
	// The 11:00 purchase reaches the journal first, the 09:00 one a sync later.
	for _, op := range []operation.Operation{buy("op-late", "11:00", "200", -200_000), buy("op-early", "09:00", "100", -100_000)} {
		if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{op}}); err != nil || len(refused) > 0 {
			t.Fatalf("import %s: %v %v", *op.ExternalID, err, refused)
		}
	}
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date("2026-07-03"), Quantity: dec("10"), Price: dec("150"),
		AmountMinor: 150_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("sell: %v", err)
	}

	ops, positions := journalOf(t, f, f.accountID)
	if *ops[0].ExternalID != "op-early" {
		t.Errorf("the day folds %s first, want the 09:00 purchase", *ops[0].ExternalID)
	}
	if p := positions[f.sberID]; p == nil || p.CostMinor != 200_000 {
		t.Errorf("what is left = %+v, want the 11:00 parcel (200 000): the sale took the 09:00 one", p)
	}
}

// Rows with an instant fold ahead of rows of the same day without one, and
// the database and the write paths agree on it.
func TestSQLAndMemoryFoldInstantsTheSameWay(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("1"), Price: dec("100"),
		AmountMinor: -10_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("buy by hand: %v", err)
	}
	for _, c := range []struct{ ext, clock string }{{"op-b", "15:00"}, {"op-a", "10:00"}} {
		op := timed(operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-02"), Quantity: dec("1"), Price: dec("100"),
			AmountMinor: -10_000, Currency: "RUB",
		}, c.ext, at("2026-07-02", c.clock))
		if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{op}}); err != nil || len(refused) > 0 {
			t.Fatalf("import %s: %v %v", c.ext, err, refused)
		}
	}

	fromSQL, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range fromSQL {
		name := "by hand"
		if o.ExternalID != nil {
			name = *o.ExternalID
		}
		got = append(got, name)
	}
	if want := []string{"op-a", "op-b", "by hand"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("the database folds %v, want %v", got, want)
	}
	inMemory := []operation.Operation{fromSQL[2], fromSQL[1], fromSQL[0]}
	operation.SortJournalForTest(inMemory)
	for i := range inMemory {
		if inMemory[i].ID != fromSQL[i].ID {
			t.Fatalf("row %d: in memory %s, from the database %s — the two orders must be one rule", i, inMemory[i].ID, fromSQL[i].ID)
		}
	}
}

// An imported transfer releases the parcels held at its own instant, not those
// left at the end of its day. Here the end of the day holds only the afternoon
// parcel — the morning one is sold by then — while at 10:00 the shares that
// left were the morning ones, bought at 100.
func TestAnImportedTransferReleasesWhatWasHeldAtItsInstant(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	day := "2026-07-02"
	trade := func(ext, clock string, typ operation.Type, qty, price string, amount int64) operation.Operation {
		return timed(operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: typ,
			OccurredOn: date(day), Quantity: dec(qty), Price: dec(price),
			AmountMinor: amount, Currency: "RUB",
		}, ext, at(day, clock))
	}
	if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{
		trade("op-buy-1", "09:00", operation.TypeBuy, "10", "100", -100_000),
		trade("op-sell-1", "12:00", operation.TypeSell, "5", "150", 75_000),
		trade("op-buy-2", "14:00", operation.TypeBuy, "10", "200", -200_000),
		trade("op-sell-2", "16:00", operation.TypeSell, "5", "250", 125_000),
	}}); err != nil || len(refused) > 0 {
		t.Fatalf("import the day's trades: %v %v", err, refused)
	}

	out, in := pairOfLegs(f, uuid.New(), "5", day)
	out.OccurredAt, in.OccurredAt = at(day, "10:00"), at(day, "10:00")
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{out, in}})
	if err != nil || len(refused) > 0 {
		t.Fatalf("import the transfer: %v %v", err, refused)
	}
	for _, leg := range applied {
		if leg.AmountMinor != 50_000 {
			t.Errorf("%s carries a basis of %d, want 50 000: five of the shares bought at 100", leg.Type, leg.AmountMinor)
		}
	}
	journalOf(t, f, f.accountID)
	journalOf(t, f, f.account2ID)
}

// One delta, listed out of order: the transfer before the morning purchase it
// moves. The delta is judged in the order its rows happened, so the transfer
// finds the shares bought at 09:00 rather than an empty account.
func TestADeltaIsJudgedInTheOrderItsRowsHappened(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	day := "2026-07-02"
	out, in := pairOfLegs(f, uuid.New(), "5", day)
	out.OccurredAt, in.OccurredAt = at(day, "10:00"), at(day, "10:00")
	buy := timed(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date(day), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "op-buy", at(day, "09:00"))

	_, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{out, in, buy}})
	if err != nil || len(refused) > 0 {
		t.Fatalf("ApplyImportDelta: %v, refused %+v — want the transfer to find the 09:00 purchase", err, refused)
	}
	journalOf(t, f, f.accountID)
}

// In memory too, a row with an instant folds ahead of a row of its day without
// one, even one recorded earlier.
func TestARowWithAnInstantFoldsAheadOfOneWithout(t *testing.T) {
	byHand := operation.Operation{
		ID: uuid.New(), OccurredOn: date("2026-07-02"), Source: operation.SourceManual,
		CreatedAt: time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC),
	}
	broker := operation.Operation{
		ID: uuid.New(), OccurredOn: date("2026-07-02"), Source: "tinvest",
		CreatedAt: time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC), OccurredAt: at("2026-07-02", "15:00"),
	}
	for _, journal := range [][]operation.Operation{{byHand, broker}, {broker, byHand}} {
		operation.SortJournalForTest(journal)
		if journal[0].ID != broker.ID {
			t.Errorf("folds %s first, want the broker's row", journal[0].Source)
		}
	}
}

// A row loaded from a table keeps the instant it came with through an edit of
// anything but its day, and loses it when the day changes: it was the instant
// of the old day.
func TestEditingTheDayOfARowDropsItsInstant(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	row := timed(operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-02"), AmountMinor: 100_000, Currency: "RUB",
	}, "row-1", at("2026-07-02", "10:00"))
	row.Source = operation.SourceTable
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{row}})
	if err != nil || len(refused) > 0 || len(applied) != 1 {
		t.Fatalf("load the row: %v %v", err, refused)
	}
	edited := applied[0]
	edited.Note = "зарплата"
	edited.OccurredAt = nil // an edit arrives as a request, which carries no instant
	got, err := svc.Update(f.ctx, f.spaceID, edited.ID, edited)
	if err != nil {
		t.Fatalf("edit the note: %v", err)
	}
	if got.OccurredAt == nil || !got.OccurredAt.Equal(*at("2026-07-02", "10:00")) {
		t.Errorf("after editing the note the instant is %v, want 10:00 kept", got.OccurredAt)
	}
	edited = got
	edited.OccurredOn = date("2026-07-03")
	edited.OccurredAt = nil
	got, err = svc.Update(f.ctx, f.spaceID, edited.ID, edited)
	if err != nil {
		t.Fatalf("edit the day: %v", err)
	}
	if got.OccurredAt != nil {
		t.Errorf("after moving the row to another day the instant is %v, want none", got.OccurredAt)
	}
}

// The edit is checked at the row's own place in its day: a purchase at 10:00
// that a 12:00 sale rests on stays at 10:00 while its note is changed, and the
// edit is not refused as if the purchase had moved after the sale.
func TestAnEditIsCheckedAtTheRowsInstant(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	day := "2026-07-02"
	buy := timed(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date(day), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}, "row-buy", at(day, "10:00"))
	buy.Source = operation.SourceTable
	sell := timed(operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
		OccurredOn: date(day), Quantity: dec("10"), Price: dec("120"),
		AmountMinor: 120_000, Currency: "RUB",
	}, "op-sell", at(day, "12:00"))
	applied, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{buy, sell}})
	if err != nil || len(refused) > 0 {
		t.Fatalf("load the day: %v %v", err, refused)
	}
	var edited operation.Operation
	for _, o := range applied {
		if o.Type == operation.TypeBuy {
			edited = o
		}
	}
	edited.Note = "первая покупка"
	edited.OccurredAt = nil // an edit arrives as a request, which carries no instant
	if _, err := svc.Update(f.ctx, f.spaceID, edited.ID, edited); err != nil {
		t.Fatalf("editing the purchase's note: %v", err)
	}
}

// The journal screen lists a day newest first in the engine's own order: the
// operation the broker reported late but made at 09:00 sits below the 11:00
// one, and a row entered by hand — which folds after the broker's rows of its
// day — on top.
func TestTheJournalListsADayInTheEnginesOrderNewestFirst(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	buy := func(ext, clock string) operation.Operation {
		return timed(operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-02"), Quantity: dec("1"), Price: dec("100"),
			AmountMinor: -10_000, Currency: "RUB",
		}, ext, at("2026-07-02", clock))
	}
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-02"), AmountMinor: 50_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("deposit by hand: %v", err)
	}
	for _, op := range []operation.Operation{buy("op-late", "11:00"), buy("op-early", "09:00")} {
		if _, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{Add: []operation.Operation{op}}); err != nil || len(refused) > 0 {
			t.Fatalf("import %s: %v %v", *op.ExternalID, err, refused)
		}
	}
	page, _, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0, operation.JournalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range page {
		name := "by hand"
		if o.ExternalID != nil {
			name = *o.ExternalID
		}
		got = append(got, name)
	}
	if want := []string{"by hand", "op-late", "op-early"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("the journal lists %v, want %v", got, want)
	}
}
