package operation_test

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// SQL and in-memory sorting fold a day in the same order (see foldRank). The
// split is stamped after the same-day trade, so created_at alone and the rank
// disagree; a split stamped first would prove nothing. The arithmetic half is
// TestASameDayBuyIsNotMultipliedByThatDaysSplit.
func TestSQLAndMemoryFoldADayInTheSameOrder(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// The buy is written FIRST and dated the same day as the split, so its
	// created_at is the older of the two.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("seed the purchase: %v", err)
	}
	seedSplit(t, f, svc, splitOf(f, f.sberID, "2026-07-02", "10"))

	// What the database hands the engine.
	fromSQL, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if len(fromSQL) != 2 {
		t.Fatalf("journal has %d rows, want 2", len(fromSQL))
	}
	if fromSQL[0].Type != operation.TypeSplit {
		t.Errorf("the database folds %s first on 2026-07-02, want the registry's split — "+
			"it is stamped after the trades it has to precede", fromSQL[0].Type)
	}

	// What the write paths fold in memory, over the very same rows in the
	// opposite order, so a sort that did nothing at all would be caught.
	inMemory := []operation.Operation{fromSQL[1], fromSQL[0]}
	operation.SortJournalForTest(inMemory)
	for i := range inMemory {
		if inMemory[i].ID != fromSQL[i].ID {
			t.Fatalf("row %d: in memory %s (%s), from the database %s (%s) — "+
				"the two orders must be one rule",
				i, inMemory[i].Type, inMemory[i].ID, fromSQL[i].Type, fromSQL[i].ID)
		}
	}
}

// A buy on a split's effective day is already in post-split units. Ten held
// before, ten bought that day, one into ten: 100 + 10 = 110. Folded last, the
// split would multiply the same-day buy too: 200.
func TestASameDayBuyIsNotMultipliedByThatDaysSplit(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	before := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	sameDay := before
	sameDay.OccurredOn = date("2026-07-02")
	sameDay.Price = dec("10")
	sameDay.AmountMinor = -10_000
	for _, op := range []operation.Operation{before, sameDay} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed the %s purchase: %v", op.OccurredOn.Format("2006-01-02"), err)
		}
	}
	seedSplit(t, f, svc, splitOf(f, f.sberID, "2026-07-02", "10"))

	held := positionsOf(t, f, f.accountID)[f.sberID].Quantity
	if want := decimal.RequireFromString("110"); !held.Equal(want) {
		t.Errorf("position = %s, want %s — ten shares held from before become a hundred, "+
			"and the ten bought on the split day are already post-split", held, want)
	}
}
