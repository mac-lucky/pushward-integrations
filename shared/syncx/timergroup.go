package syncx

import (
	"sync"
	"time"
)

// TimerGroup holds a single active timer plus a wait group tracking
// in-flight AfterFunc callbacks. Reset replaces the active timer,
// stopping the prior one. Wait blocks until all scheduled callbacks
// complete. The zero value is ready to use.
type TimerGroup struct {
	mu    sync.Mutex
	timer *time.Timer
	wg    sync.WaitGroup
}

// Reset stops the currently-scheduled callback (if any) and schedules
// fn to run after d. fn is tracked by the group's wait group so Wait
// blocks until it returns.
func (g *TimerGroup) Reset(d time.Duration, fn func()) {
	g.wg.Add(1)
	g.mu.Lock()
	if g.timer != nil && g.timer.Stop() {
		// Prior callback was cancelled before firing; balance its
		// unfulfilled wg.Add from the prior Reset.
		g.wg.Done()
	}
	g.timer = time.AfterFunc(d, func() {
		defer g.wg.Done()
		fn()
	})
	g.mu.Unlock()
}

// Stop stops the currently-scheduled callback (if any) without waiting
// for callbacks that have already started to finish.
func (g *TimerGroup) Stop() {
	g.mu.Lock()
	if g.timer != nil {
		if g.timer.Stop() {
			g.wg.Done()
		}
		g.timer = nil
	}
	g.mu.Unlock()
}

// Wait blocks until all in-flight callbacks scheduled by Reset have
// returned.
func (g *TimerGroup) Wait() {
	g.wg.Wait()
}
