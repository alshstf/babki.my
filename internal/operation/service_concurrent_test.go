package operation_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// Two concurrent sells of the same ten shares: exactly one is refused, and
// the account still replays (#17). Several rounds on fresh accounts, since the
// fault is a window, not a rule.
func TestConcurrentSellsOfOneHoldingLeaveAJournalThatReplays(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	for round := range 6 {
		accountID := f.newAccount(t)
		if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-07-01"), Quantity: dec("10"), AmountMinor: -100_000,
			Currency: "RUB",
		}); err != nil {
			t.Fatalf("round %d: buy: %v", round, err)
		}

		sell := operation.Operation{
			AccountID: accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
			OccurredOn: date("2026-07-02"), Quantity: dec("10"), AmountMinor: 120_000,
			Currency: "RUB",
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = svc.Create(f.ctx, f.spaceID, sell)
			}()
		}
		close(start)
		wg.Wait()

		accepted := 0
		for _, err := range errs {
			if err == nil {
				accepted++
			}
		}
		if accepted != 1 {
			t.Errorf("round %d: %d of the two sells were accepted, want exactly 1 (errors: %v, %v)",
				round, accepted, errs[0], errs[1])
		}
		// And what the account holds still folds.
		journal, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatalf("round %d: list: %v", round, err)
		}
		if _, err := portfolio.Compute(journal); err != nil {
			t.Fatalf("round %d: the account's journal no longer replays: %v", round, err)
		}
	}
}

// While one caller holds an account's journal lock, a second waits.
func TestAccountLockSerializesTwoWriters(t *testing.T) {
	f := newFixture(t)
	ids := []uuid.UUID{f.accountID}

	inside := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- f.store.WithAccountsLocked(f.ctx, f.spaceID, ids, func(*operation.Store) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	secondInside := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- f.store.WithAccountsLocked(f.ctx, f.spaceID, ids, func(*operation.Store) error {
			close(secondInside)
			return nil
		})
	}()

	// Reported at the end, not fataled: the first writer holds a pooled
	// connection until release is closed, and leaving it out hangs the
	// package.
	gotInEarly := false
	select {
	case <-secondInside:
		gotInEarly = true
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer never got in after the first released")
	}
	if gotInEarly {
		t.Error("the second writer got inside while the first still held the account's journal lock")
	}
}

// Locks are taken in the accounts' own order, so opposite transfers cannot
// deadlock. Checked directly: another transaction holds the higher id, a caller
// asks for the pair high first, and a NOWAIT attempt on the lower id must then
// fail because the caller already took it.
func TestAccountLocksAreTakenInTheAccountsOwnOrder(t *testing.T) {
	f := newFixture(t)
	lo, hi := f.accountID, f.newAccount(t)
	if bytes.Compare(lo[:], hi[:]) > 0 {
		lo, hi = hi, lo
	}

	holder, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := holder.Exec(f.ctx,
		`SELECT id FROM accounts WHERE id = $1 FOR NO KEY UPDATE`, hi); err != nil {
		_ = holder.Rollback(f.ctx)
		t.Fatalf("holder lock: %v", err)
	}

	waiting := make(chan error, 1)
	go func() {
		waiting <- f.store.WithAccountsLocked(f.ctx, f.spaceID,
			[]uuid.UUID{hi, lo}, func(*operation.Store) error { return nil })
	}()
	// Long enough for the waiter to take the first lock and block on the
	// other.
	time.Sleep(500 * time.Millisecond)

	probe, err := f.pool.Begin(f.ctx)
	if err != nil {
		_ = holder.Rollback(f.ctx)
		t.Fatalf("begin probe: %v", err)
	}
	_, lowStillFree := probe.Exec(f.ctx,
		`SELECT id FROM accounts WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, lo)
	_ = probe.Rollback(f.ctx)

	// Release everything and collect the waiter before reporting, or a parked
	// connection hangs the pool's shutdown.
	_ = holder.Rollback(f.ctx)
	select {
	case err := <-waiting:
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got its locks after the holder let go")
	}

	if lowStillFree == nil {
		t.Error("the lower account id was still free — the locks were taken in the order they were asked for, so two transfers in opposite directions can deadlock")
	}
}

// The import door replays under the same lock as a hand entry (#186). Ten
// held; a hand sale of eight is uncommitted under the lock; an imported sale of
// eight must wait, then see the first and be refused.
func TestApplyImportDeltaWaitsForTheAccountLock(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-03-02"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed buy: %v", err)
	}
	sell := func(on string) operation.Operation {
		return operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSell,
			OccurredOn: date(on), Quantity: dec("8"), Price: dec("110"),
			AmountMinor: 88_000, Currency: "RUB",
		}
	}

	written := make(chan struct{})
	release := make(chan struct{})
	manualDone := make(chan error, 1)
	go func() {
		manualDone <- f.store.WithAccountsLocked(f.ctx, f.spaceID, []uuid.UUID{f.accountID}, func(st *operation.Store) error {
			if _, err := operation.NewService(st).Create(f.ctx, f.spaceID, sell("2026-03-10")); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-manualDone:
		t.Fatalf("the hand-entered sale never got written: %v", err)
	}

	type outcome struct {
		refused []operation.ImportRefusal
		err     error
	}
	importDone := make(chan outcome, 1)
	go func() {
		_, refused, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
			Add: []operation.Operation{imported(sell("2026-03-11"), "import-sell")},
		})
		importDone <- outcome{refused, err}
	}()

	// Reported at the end, as above.
	var early *outcome
	select {
	case o := <-importDone:
		early = &o
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-manualDone; err != nil {
		t.Fatalf("hand-entered sale: %v", err)
	}
	var got outcome
	if early != nil {
		got = *early
	} else {
		select {
		case got = <-importDone:
		case <-time.After(10 * time.Second):
			t.Fatal("the import never got in after the hand entry released the account")
		}
	}
	if early != nil {
		t.Error("the import finished while a hand entry still held the account's journal lock")
	}
	if got.err != nil {
		t.Fatalf("ApplyImportDelta: %v", got.err)
	}
	if len(got.refused) != 1 {
		t.Errorf("refused = %+v, want the imported sale refused — eight of the ten shares were already sold", got.refused)
	}

	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	if _, err := portfolio.Compute(journal); err != nil {
		t.Errorf("the account's journal no longer replays: %v", err)
	}
}
