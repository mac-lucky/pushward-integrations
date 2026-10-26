package client

import (
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// serverTenants is how many different tenants a fault streak has to come from
// before the server breaker opens. One tenant failing over and over is that
// tenant's problem (a resource of its own that keeps answering 5xx, requests
// of its own that time out) and opens only its own breaker. When the server
// itself is down or refusing connections every tenant fails, and the streak
// picks up three of them within a few calls.
const serverTenants = 3

// serverBreaker opens for every tenant. It counts consecutive faults across
// the whole pool, and lets them through to its CircuitBreaker only once they
// reach the threshold and came from at least minTenants tenants. Any genuine
// success, from any tenant, ends the streak: the server is up. So does a gap
// longer than window since the last fault: faults from different tenants
// hours apart say nothing about the server now.
type serverBreaker struct {
	cb         *pushward.CircuitBreaker // threshold 1: the gate below decides
	threshold  int
	minTenants int
	window     time.Duration
	now        func() time.Time

	mu        sync.Mutex
	streak    int
	lastFault time.Time
	// tenants holds the key hashes in the current streak. It stays small:
	// it is cleared whenever the streak ends, and the streak ends at the
	// latest when it reaches max(threshold, minTenants) tenants.
	tenants map[string]struct{}
}

// newServerBreaker uses cooldown for the streak window too.
func newServerBreaker(threshold int, cooldown time.Duration) *serverBreaker {
	return &serverBreaker{
		cb:         pushward.NewCircuitBreaker(1, cooldown),
		threshold:  threshold,
		minTenants: serverTenants,
		window:     cooldown,
		now:        time.Now,
		tenants:    make(map[string]struct{}),
	}
}

func (s *serverBreaker) allow() bool { return s.cb.Allow() }
func (s *serverBreaker) abort()      { s.cb.Abort() }

// blocking reports whether the breaker refuses calls right now.
func (s *serverBreaker) blocking() bool { return s.cb.Blocking() }

func (s *serverBreaker) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cb.RecordSuccess()
	s.resetLocked()
}

// recordReachable leaves the streak alone, as CircuitBreaker.RecordReachable
// does: a routine 4xx or 429 from one tenant must not keep resetting a streak
// of real faults from the others.
func (s *serverBreaker) recordReachable() { s.cb.RecordReachable() }

func (s *serverBreaker) recordFailure(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cb.IsOpen() {
		// Half-open: the probe failed and the breaker re-opens. Open: a call
		// admitted before it opened, which changes nothing.
		s.cb.RecordFailure()
		return
	}
	now := s.now()
	if s.streak > 0 && now.Sub(s.lastFault) > s.window {
		s.resetLocked()
	}
	s.streak++
	s.lastFault = now
	s.tenants[tenant] = struct{}{}
	if s.streak >= s.threshold && len(s.tenants) >= s.minTenants {
		s.cb.RecordFailure()
		s.resetLocked()
	}
}

func (s *serverBreaker) resetLocked() {
	s.streak = 0
	clear(s.tenants)
}

// tenantBreaker is the pushward.Breaker each pooled client carries: the
// tenant's own CircuitBreaker in front of the shared server breaker. A call
// goes out only when both admit it, and its outcome is reported to both.
type tenantBreaker struct {
	tenant string // key hash, never the key
	own    *pushward.CircuitBreaker
	server *serverBreaker
}

func (b *tenantBreaker) Allow() bool {
	if !b.own.Allow() {
		return false
	}
	if !b.server.allow() {
		// own may have just admitted this call as its half-open probe. The
		// call will not run, so re-arm own rather than leave it half-open.
		b.own.Abort()
		return false
	}
	return true
}

func (b *tenantBreaker) RecordSuccess() {
	b.own.RecordSuccess()
	b.server.recordSuccess()
}

func (b *tenantBreaker) RecordReachable() {
	b.own.RecordReachable()
	b.server.recordReachable()
}

func (b *tenantBreaker) RecordFailure() {
	b.own.RecordFailure()
	b.server.recordFailure(b.tenant)
}

func (b *tenantBreaker) Abort() {
	b.own.Abort()
	b.server.abort()
}
