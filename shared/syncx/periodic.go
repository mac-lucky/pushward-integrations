package syncx

import (
	"context"
	"sync"
	"time"
)

// Periodic runs a function at a fixed interval in a background goroutine.
// Stop the goroutine with Stop() or by cancelling the context passed to
// Start. The zero value is ready to use.
type Periodic struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	started  bool
}

// Start launches the background goroutine. fn receives the Periodic's
// context (derived from ctx) and should return promptly. Calling Start
// more than once panics.
func (p *Periodic) Start(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		panic("syncx: Periodic.Start called twice")
	}
	p.started = true
	p.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer cancel()
		defer close(p.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				fn(runCtx)
			}
		}
	}()
}

// Stop cancels the Periodic's context and waits for any in-flight fn to
// return. Safe to call multiple times; only the first call has effect.
// Safe to call before Start (no-op).
func (p *Periodic) Stop() {
	p.stopOnce.Do(func() {
		if p.cancel == nil {
			return
		}
		p.cancel()
		<-p.done
	})
}
