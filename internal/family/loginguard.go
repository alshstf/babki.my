package family

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// The limits of the sign-in door.
//
// A password is guessed per account, so failures are counted per address AND
// username: five wrong passwords lock that pair for a minute, and every further
// one doubles the lock up to a quarter of an hour, where it stays until the pair
// has been quiet for as long.
//
// NOT BY USERNAME ALONE: anyone who can reach the port could then lock the owner
// out by typing wrong passwords under the owner's name.
//
// AND NOT BY ADDRESS ALONE, nor with a second allowance for the address as a
// whole. Behind a reverse proxy every client arrives from the proxy's address —
// this server does not read forwarding headers, because without knowing which
// proxy to trust they are whatever the client typed — so an allowance for the
// address would be one allowance for the household and every stranger on the
// internet together, and a bot walking through usernames would lock the family
// out of its own server. What bounds that walk instead is the cost of each
// attempt (see hashSlots) and the fact that a username has to exist to be worth
// guessing at.
const (
	pairFreeFailures = 5
	pairFirstLock    = time.Minute
	pairMaxLock      = 15 * time.Minute

	// forgetAfter is how long a pair has to stay quiet, once any lock on it has
	// ended, for its failures to stop counting.
	forgetAfter = 15 * time.Minute

	// sweepAbove is the table size past which a failure also clears out what
	// has been forgotten, so the table is bounded by what was tried lately
	// rather than by the life of the process.
	sweepAbove = 1024

	// maxGuardedUsername bounds what is kept of a username nobody validated:
	// the sign-in door checks no shape (see LoginRequest), and a megabyte typed
	// into the field must not become a megabyte of key.
	maxGuardedUsername = 64
)

type attemptKey struct{ addr, username string }

type attemptCount struct {
	failures   int
	lastFailed time.Time
	lockedTill time.Time
}

// loginGuard counts failed sign-ins and says when the next one may be tried.
//
// IT LIVES IN THE PROCESS, not in the database: a restart forgets it, and two
// processes serving the same instance count separately. Both are accepted — it
// turns "as fast as the server can hash" into a handful an hour either way.
type loginGuard struct {
	mu    sync.Mutex
	now   func() time.Time
	pairs map[attemptKey]*attemptCount
}

func newLoginGuard() *loginGuard {
	return &loginGuard{now: time.Now, pairs: map[attemptKey]*attemptCount{}}
}

func guardKey(addr, username string) attemptKey {
	if len(username) > maxGuardedUsername {
		username = username[:maxGuardedUsername]
	}
	return attemptKey{addr: addr, username: username}
}

// wait reports how long this address has to wait before trying this username,
// or zero when it may try now.
func (g *loginGuard) wait(addr, username string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if c, ok := g.pairs[guardKey(addr, username)]; ok && c.lockedTill.After(now) {
		return c.lockedTill.Sub(now)
	}
	return 0
}

// failed records one refused sign-in.
func (g *loginGuard) failed(addr, username string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	key := guardKey(addr, username)
	c, ok := g.pairs[key]
	if !ok || forgotten(c, now) {
		c = &attemptCount{}
		g.pairs[key] = c
	}
	c.failures++
	c.lastFailed = now
	if over := c.failures - pairFreeFailures; over >= 0 {
		lock := pairMaxLock
		// 1, 2, 4, 8 minutes; the shift is bounded so it cannot overflow.
		if over < 4 {
			lock = pairFirstLock << over
		}
		c.lockedTill = now.Add(lock)
	}

	if len(g.pairs) > sweepAbove {
		for key, c := range g.pairs {
			if forgotten(c, now) {
				delete(g.pairs, key)
			}
		}
	}
}

// succeeded forgets the pair's failures: the person at this address knows this
// account's password.
func (g *loginGuard) succeeded(addr, username string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pairs, guardKey(addr, username))
}

// forgotten says whether a count is old enough to start again from nothing:
// quiet for forgetAfter since its last failure and since its lock ended.
func forgotten(c *attemptCount, now time.Time) bool {
	quietSince := c.lastFailed
	if c.lockedTill.After(quietSince) {
		quietSince = c.lockedTill
	}
	return now.Sub(quietSince) >= forgetAfter
}

// clientAddr is the address the connection came from, without the port. See
// the note above on why forwarding headers are not read.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
