package family

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
)

// guardAt is a guard whose clock the test moves.
func guardAt(start time.Time) (*loginGuard, *time.Time) {
	now := start
	g := newLoginGuard()
	g.now = func() time.Time { return now }
	return g, &now
}

var guardDay = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func failTimes(g *loginGuard, n int, addr, username string) {
	for range n {
		g.failed(addr, username)
	}
}

// Five wrong passwords lock the pair for a minute; each further one doubles the
// lock, up to a quarter of an hour.
func TestWrongPasswordsLockThePairForLongerEachTime(t *testing.T) {
	g, now := guardAt(guardDay)

	failTimes(g, pairFreeFailures-1, "10.0.0.7", "alex")
	if wait := g.wait("10.0.0.7", "alex"); wait != 0 {
		t.Fatalf("locked for %s after %d failures, want the fifth to be the one that locks", wait, pairFreeFailures-1)
	}

	for i, want := range []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute,
	} {
		g.failed("10.0.0.7", "alex")
		if wait := g.wait("10.0.0.7", "alex"); wait != want {
			t.Fatalf("failure %d locks for %s, want %s", pairFreeFailures+i, wait, want)
		}
		*now = now.Add(want - time.Second)
		if wait := g.wait("10.0.0.7", "alex"); wait != time.Second {
			t.Fatalf("a second before the lock ends the wait is %s, want 1s", wait)
		}
		*now = now.Add(time.Second)
		if wait := g.wait("10.0.0.7", "alex"); wait != 0 {
			t.Fatalf("the lock of %s is still %s when it should have ended", want, wait)
		}
	}
}

// Somebody typing wrong passwords for the owner's name from another address
// does not lock the owner out, and other names at the guessing address are not
// locked either — behind a proxy that address is everybody's.
func TestALockIsOfOneAddressAndOneUsername(t *testing.T) {
	g, _ := guardAt(guardDay)
	failTimes(g, pairFreeFailures, "203.0.113.9", "alex")

	if wait := g.wait("203.0.113.9", "alex"); wait == 0 {
		t.Fatal("the guessing address is not locked")
	}
	if wait := g.wait("10.0.0.7", "alex"); wait != 0 {
		t.Errorf("the owner's own address waits %s for failures made elsewhere", wait)
	}
	if wait := g.wait("203.0.113.9", "kate"); wait != 0 {
		t.Errorf("another username waits %s at the guessing address", wait)
	}
}

// Signing in forgets the pair's failures.
func TestSigningInForgetsThePairsFailures(t *testing.T) {
	g, _ := guardAt(guardDay)
	failTimes(g, pairFreeFailures-1, "10.0.0.7", "alex")
	g.succeeded("10.0.0.7", "alex")
	failTimes(g, pairFreeFailures-1, "10.0.0.7", "alex")
	if wait := g.wait("10.0.0.7", "alex"); wait != 0 {
		t.Errorf("locked for %s: the failures before a successful sign-in still count", wait)
	}
}

// Failures stop counting after a quiet quarter of an hour — counted from the
// end of the lock, so that the longest lock is not also the moment the count
// starts again from nothing.
func TestFailuresAreForgottenAfterAQuietSpell(t *testing.T) {
	g, now := guardAt(guardDay)
	failTimes(g, pairFreeFailures-1, "10.0.0.7", "alex")
	*now = now.Add(forgetAfter)
	g.failed("10.0.0.7", "alex")
	if wait := g.wait("10.0.0.7", "alex"); wait != 0 {
		t.Errorf("locked for %s by failures made before a quiet %s", wait, forgetAfter)
	}

	failTimes(g, pairFreeFailures, "10.0.0.8", "alex")
	*now = now.Add(time.Minute + forgetAfter - time.Second)
	g.failed("10.0.0.8", "alex")
	if wait := g.wait("10.0.0.8", "alex"); wait != 2*time.Minute {
		t.Errorf("a failure a second short of a quiet %s after the lock ended locks for %s, want the doubled 2m0s",
			forgetAfter, wait)
	}
}

// The table holds what was tried lately, not everything ever tried, and a
// username of any length costs a bounded key.
func TestTheTableIsBounded(t *testing.T) {
	g, now := guardAt(guardDay)
	for i := range sweepAbove {
		g.failed("198.51.100.7", "user"+strconv.Itoa(i))
	}
	if len(g.pairs) != sweepAbove {
		t.Fatalf("%d entries after %d different usernames", len(g.pairs), sweepAbove)
	}
	*now = now.Add(forgetAfter)
	g.failed("10.0.0.7", strings.Repeat("a", 1<<20))
	if n := len(g.pairs); n != 1 {
		t.Errorf("%d entries remain after everything but one was forgotten, want 1", n)
	}
	for key := range g.pairs {
		if len(key.username) > maxGuardedUsername {
			t.Errorf("a username of %d bytes is kept as a key", len(key.username))
		}
	}
}

// At most len(hashSlots) passwords are hashed at once, and a request that
// gives up while waiting for a slot is let go.
func TestHashingIsBoundedAndGivesUpWithTheRequest(t *testing.T) {
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := withHashSlot(context.Background(), func() {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				running.Add(-1)
			}); err != nil {
				t.Errorf("withHashSlot: %v", err)
			}
		})
	}
	wg.Wait()
	if got := int(peak.Load()); got > cap(hashSlots) {
		t.Errorf("%d hashes ran at once, want at most %d", got, cap(hashSlots))
	}

	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withHashSlot(ctx, func() { t.Error("ran with every slot taken") }); err == nil {
		t.Error("a cancelled request waited for a slot and got none, yet no error came back")
	}
}

// The hash an unknown username is compared against costs what a real one does:
// same memory, same passes, same lanes.
func TestTheDummyHashCostsWhatARealOneDoes(t *testing.T) {
	real, err := (&Service{}).HashPassword(context.Background(), "secret123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	want, _, _, err := argon2id.DecodeHash(real)
	if err != nil {
		t.Fatalf("decode a real hash: %v", err)
	}
	got, _, _, err := argon2id.DecodeHash(dummyHash())
	if err != nil {
		t.Fatalf("decode the dummy hash: %v", err)
	}
	if *got != *want {
		t.Errorf("the dummy hash is made with %+v, a real one with %+v", *got, *want)
	}
}
