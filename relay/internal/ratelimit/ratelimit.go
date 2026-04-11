package ratelimit

import (
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
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

// SweepStale removes rate limiter entries not accessed in the given duration.
// Returns the number of entries removed.
func SweepStale(maxAge time.Duration) int {
	return keyLimiters.entries.Sweep(maxAge)
}

// AllowKey checks whether a request with the given route pattern and auth key
// is allowed under per-key rate limiting. Returns true if allowed.
func AllowKey(pattern, key string) bool {
	return keyLimiters.get(pattern + lrumap.KeyHash(key)).Allow()
}

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

		if !AllowKey(r.Pattern, key) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
