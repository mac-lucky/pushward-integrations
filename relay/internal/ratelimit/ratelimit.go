package ratelimit

import (
	"log/slog"
	"time"

	"golang.org/x/time/rate"

	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
)

type limiterMap struct {
	entries *lrumap.Map[*rate.Limiter]
	rate    rate.Limit
	burst   int
}

func newLimiterMap(r rate.Limit, burst, maxEntries int) *limiterMap {
	return &limiterMap{
		entries: lrumap.New[*rate.Limiter](maxEntries),
		rate:    r,
		burst:   burst,
	}
}

func (m *limiterMap) get(key string) *rate.Limiter {
	return m.entries.GetOrCreate(key, func() *rate.Limiter {
		return rate.NewLimiter(m.rate, m.burst)
	})
}

var keyLimiters = newLimiterMap(1, 10, 100_000)

func init() {
	keyLimiters.entries.SetOnEvict(func(key string, _ *rate.Limiter) {
		slog.Debug("rate limiter evicted", "limiter_key", key)
	})
}

// SweepStale removes rate limiter entries not accessed in the given duration
// from BOTH the per-key and per-IP maps. Returns the number of entries removed.
// Sweeping the IP map too keeps it below its cap during IP churn so new IPs
// avoid the O(n) LRU eviction scan.
func SweepStale(maxAge time.Duration) int {
	return keyLimiters.entries.Sweep(maxAge) + ipLimiters.entries.Sweep(maxAge)
}

// AllowKey checks whether a request with the given route pattern and auth key
// is allowed under per-key rate limiting. Returns true if allowed.
func AllowKey(pattern, key string) bool {
	return keyLimiters.get(pattern + lrumap.KeyHash(key)).Allow()
}
