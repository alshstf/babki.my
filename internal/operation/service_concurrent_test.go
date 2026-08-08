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

// TestConcurrentSellsOfOneHoldingLeaveAJournalThatReplays is issue #17's
// check-then-write race, written as the damage it does rather than as the
// mechanism that does it.
//
// Two clients sell the same ten shares at the same moment. Each is a perfectly
// good request on its own: the account holds ten, ten is what it asks for. What
// the account must not be left with is BOTH of them — a journal that sells
// twenty out of ten replays for nobody, so every later read of that account's
// positions answers 422, for ever, and nobody who did anything wrong is
// anywhere near it. Exactly one of the two has to be refused, which is what
// would have happened had they arrived a second apart.
//
// SEVERAL ROUNDS, ON A FRESH ACCOUNT EACH TIME, because the fault it guards is
// a window rather than a rule: one round that happens to serialize by itself
// proves nothing. Each round is its own account, so the rounds cannot mask each
// other by leaving a position behind.
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
		// The refusal is only half the property. What the account is actually
		// left holding has to fold, which is the thing the owner sees.
		journal, err := f.store.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatalf("round %d: list: %v", round, err)
		}
		if _, err := portfolio.Compute(journal); err != nil {
			t.Fatalf("round %d: the account's journal no longer replays: %v", round, err)
		}
	}
}

// TestAccountLockSerializesTwoWriters pins the mutual exclusion itself: while
// one caller holds an account's journal lock, a second one waits outside rather
// than reading the journal the first is about to change.
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

	// The verdict is recorded and reported at the end rather than fataled here:
	// the first writer is parked on `release` holding a pooled connection, and a
	// test that leaves this function without closing that channel leaves the
	// connection out for good — the pool's own shutdown then waits for it and
	// the whole package hangs instead of reporting a failure.
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

// TestAccountLocksAreTakenInTheAccountsOwnOrder pins the one thing that keeps a
// transfer from deadlocking against a transfer going the other way: the locks
// are taken in an order that belongs to the ACCOUNTS, not to the caller's
// argument list. Two transfers naming the same pair in opposite orders would
// otherwise take them in opposite orders, each holding what the other is
// waiting for, and Postgres would abort one of them a second later with a
// deadlock nobody could act on.
//
// It is checked directly rather than by racing two transfers and hoping the
// interleaving lands, because "no deadlock was observed this time" is exactly
// what a broken version says most runs. A separate transaction holds the HIGHER
// of the two account ids; a caller then asks for the pair HIGH-first. If the
// order is the caller's, it blocks on the high id and never touches the low one,
// which is then free to lock elsewhere. If the order is the accounts' own, the
// low id is already taken before the wait begins — so a NOWAIT attempt on it
// must fail, and that failure is the assertion.
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
	// Long enough for the waiter to have taken whatever it takes first and to
	// have blocked on the other; it cannot get past the holder's lock at all.
	time.Sleep(500 * time.Millisecond)

	probe, err := f.pool.Begin(f.ctx)
	if err != nil {
		_ = holder.Rollback(f.ctx)
		t.Fatalf("begin probe: %v", err)
	}
	_, lowStillFree := probe.Exec(f.ctx,
		`SELECT id FROM accounts WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, lo)
	_ = probe.Rollback(f.ctx)

	// Everything is released and the waiter is collected BEFORE anything is
	// reported: a connection left parked in a goroutine outlives the test and
	// the pool's shutdown then waits on it for ever, which turns a failure into
	// a hang.
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
