package ratelimit

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
)

type entry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

type limiterMap struct {
	mu         sync.RWMutex
	entries    map[string]*entry
	rate       rate.Limit
	burst      int
	maxEntries int
}

func newLimiterMap(r rate.Limit, burst, maxEntries int) *limiterMap {
	return &limiterMap{
		entries:    make(map[string]*entry),
		rate:       r,
		burst:      burst,
		maxEntries: maxEntries,
	}
}

func (m *limiterMap) get(key string) *rate.Limiter {
	m.mu.RLock()
	if e, ok := m.entries[key]; ok {
		m.mu.RUnlock()
		return e.limiter
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check
	if e, ok := m.entries[key]; ok {
		e.lastAccess = time.Now()
		return e.limiter
	}

	// Evict LRU if at capacity
	if len(m.entries) >= m.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, e := range m.entries {
			if first || e.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.lastAccess
				first = false
			}
		}
		delete(m.entries, oldestKey)
	}

	l := rate.NewLimiter(m.rate, m.burst)
	m.entries[key] = &entry{limiter: l, lastAccess: time.Now()}
	return l
}

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}

var keyLimiters = newLimiterMap(1, 10, 10_000)

// Middleware applies per-key rate limiting. Must run after auth.Middleware
// so that auth.KeyFromContext returns the hlk_ key.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := auth.KeyFromContext(r.Context())
		if key == "" {
			// No key in context — auth middleware should have rejected this.
			next.ServeHTTP(w, r)
			return
		}

		limiter := keyLimiters.get(keyHash(key))
		if !limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
