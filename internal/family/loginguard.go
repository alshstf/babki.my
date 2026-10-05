package family

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Sign-in limits. Failures are counted per address and username: five lock the
// pair for a minute, each further one doubles the lock up to fifteen minutes,
// until the pair has been quiet that long. Not by username alone (anyone could
// lock the owner out), nor by address alone (behind a proxy everyone shares
// one address, and forwarding headers are not trusted). The cost of each hash
// bounds a username walk.
const (
	pairFreeFailures = 5
	pairFirstLock    = time.Minute
	pairMaxLock      = 15 * time.Minute

	// forgetAfter is how long a pair must be quiet after its lock for its failures to reset.
	forgetAfter = 15 * time.Minute

	// sweepAbove is the table size past which a failure also clears forgotten
	// entries.
	sweepAbove = 1024

	// maxGuardedUsername bounds what is kept of an unvalidated username.
	maxGuardedUsername = 64
)

type attemptKey struct{ addr, username string }

type attemptCount struct {
	failures   int
	lastFailed time.Time
	lockedTill time.Time
}

// loginGuard counts failed sign-ins and says when the next may be tried. It is
// in memory: a restart forgets it and processes count separately, which is
// acceptable.
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

// wait is how long addr must wait before trying username; zero means now.
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

// succeeded forgets the pair's failures.
func (g *loginGuard) succeeded(addr, username string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pairs, guardKey(addr, username))
}

// forgotten reports whether a count is quiet long enough, since its last
// failure and since its lock ended, to start over.
func forgotten(c *attemptCount, now time.Time) bool {
	quietSince := c.lastFailed
	if c.lockedTill.After(quietSince) {
		quietSince = c.lockedTill
	}
	return now.Sub(quietSince) >= forgetAfter
}

// clientAddr is the connection's address without the port; forwarding headers
// are not read.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
