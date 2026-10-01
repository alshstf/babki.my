package corporateaction_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// TestAHandEntryIsFollowedByTheRegistryAtOnce: the registry has known about the
// split all along, so a purchase entered today and dated before it is held in
// the split quantity as soon as the entry is committed — not at the next daily
// sweep, which is what it used to wait for (#188). Deleting the purchase takes
// the split's row away again: nothing is left for it to multiply.
func TestAHandEntryIsFollowedByTheRegistryAtOnce(t *testing.T) {
	f := newFixture(t)
	f.svc.OnManualWrite(f.materializer.AfterManualWrite)
	f.splitEvent(t, "2022-06-06", 1, 20)

	bought, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.amazonID, Type: operation.TypeBuy,
		OccurredOn: date("2021-05-04"), Quantity: dec("1"), AmountMinor: -323_000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Fatalf("holding = %s, want %s the moment the purchase is recorded", got, want)
	}

	if err := f.svc.Delete(f.ctx, f.spaceID, bought.ID); err != nil {
		t.Fatalf("delete the purchase: %v", err)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("got %d registry rows after the purchase was deleted, want none", len(rows))
	}
}

// TestAHandEntryOnAPaperWithNoEventsWritesNothing: most entries are about papers
// the registry has never heard of, and the hook must cost them nothing and write
// nothing.
func TestAHandEntryOnAPaperWithNoEventsWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.svc.OnManualWrite(f.materializer.AfterManualWrite)

	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)

	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("got %d registry rows, want none", len(rows))
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("1"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}

// TestTheRowsAreWorkedOutUnderTheAccountsLock: what the registry writes depends
// on what the account held, so the holding has to be read under the same lock
// the rows are written under. Here a hand entry holds the lock and sells the one
// share before the split's day; a run started meanwhile must wait, and once let
// in must see the sale and write nothing — the account held nothing on the day.
// Read before the lock, the journal would still show the share and the run
// would write a split for a holding that is gone.
func TestTheRowsAreWorkedOutUnderTheAccountsLock(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	f.splitEvent(t, "2022-06-06", 1, 20)

	written := make(chan struct{})
	release := make(chan struct{})
	handDone := make(chan error, 1)
	go func() {
		handDone <- f.ops.WithAccountsLocked(f.ctx, f.spaceID, []uuid.UUID{f.accountID}, func(st *operation.Store) error {
			if _, err := operation.NewService(st).Create(f.ctx, f.spaceID, operation.Operation{
				AccountID: f.accountID, InstrumentID: &f.amazonID, Type: operation.TypeSell,
				OccurredOn: date("2021-12-01"), Quantity: dec("1"), AmountMinor: 340_000, Currency: "USD",
			}); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-handDone:
		t.Fatalf("the hand-entered sale never got written: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := f.materializer.ForISIN(f.ctx, amazonISIN)
		runDone <- err
	}()

	// Recorded and reported at the end: the hand entry holds a pooled connection
	// until `release` is closed, and leaving early would hang the package.
	finishedEarly := false
	var runErr error
	select {
	case runErr = <-runDone:
		finishedEarly = true
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-handDone; err != nil {
		t.Fatalf("hand-entered sale: %v", err)
	}
	if !finishedEarly {
		select {
		case runErr = <-runDone:
		case <-time.After(10 * time.Second):
			t.Fatal("the run never got in after the hand entry released the account")
		}
	}
	if finishedEarly {
		t.Error("the run finished while a hand entry still held the account's journal lock")
	}
	if runErr != nil {
		t.Fatalf("materialize: %v", runErr)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("got %d registry rows, want none — the share was sold before the split's day", len(rows))
	}
}
