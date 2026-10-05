package corporateaction_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// A purchase entered today and dated before a known split is split as soon
// as it is committed (#188); deleting it takes the split row away.
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

// An entry on a paper with no events writes nothing.
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

// The holding is read under the account lock. A hand entry holding the lock
// sells the share before the split's day; a run started meanwhile must wait, see
// the sale and write nothing.
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

	// Reported at the end: the entry holds a pooled connection until release
	// is closed.
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
