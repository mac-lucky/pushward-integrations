package state

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ReadyGrace is how long the database may stay unreachable before /ready
// fails.
const ReadyGrace = 30 * time.Second

// ReadyCheck wraps a database ping for /ready. A failed ping fails the check
// only once pings have kept failing for grace. Every relay pod pings the same
// database through the same pooler, so failing on the first error took all of
// them out of the gateway together on a pooler blip, and the edge answered 503
// to every webhook until the probes passed again. Liveness never calls it.
func ReadyCheck(ping func(context.Context) error, grace time.Duration) func(context.Context) error {
	r := &readiness{ping: ping, grace: grace, now: time.Now}
	return r.check
}

type readiness struct {
	ping  func(context.Context) error
	grace time.Duration
	now   func() time.Time

	mu        sync.Mutex
	downSince time.Time // first failed ping of the current run; zero after a success
}

func (r *readiness) check(ctx context.Context) error {
	err := r.ping(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.downSince = time.Time{}
		return nil
	}
	now := r.now()
	if r.downSince.IsZero() {
		r.downSince = now
	}
	down := now.Sub(r.downSince)
	if down >= r.grace {
		return fmt.Errorf("database unreachable for %s: %w", down.Round(time.Second), err)
	}
	slog.Warn("database ping failed, staying ready", "down_for", down.Round(time.Second), "grace", r.grace, "error", err)
	return nil
}
