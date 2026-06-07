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
	mu      sync.Mutex
	timer   *time.Timer
	wg      sync.WaitGroup
	stopped bool // set by Close; once true Reset is a permanent no-op
}

// Reset stops the currently-scheduled callback (if any) and schedules
// fn to run after d. fn is tracked by the group's wait group so Wait
// blocks until it returns. After Close, Reset is a no-op (it will not
// re-arm a timer), so a callback that reschedules itself cannot keep the
// group alive past shutdown.
func (g *TimerGroup) Reset(d time.Duration, fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	// Add under the lock so the wg increment and the timer scheduling are
	// atomic with respect to Stop/Close (a racing Close otherwise risks
	// re-arming after it cleared the timer, leaving Wait blocked).
	g.wg.Add(1)
	if g.timer != nil && g.timer.Stop() {
		// Prior callback was cancelled before firing; balance its
		// unfulfilled wg.Add from the prior Reset.
		g.wg.Done()
	}
	g.timer = time.AfterFunc(d, func() {
		defer g.wg.Done()
		fn()
	})
}

// clearTimerLocked cancels the active timer (if any) and balances its
// outstanding wg.Add when it had not yet fired. The caller must hold g.mu. This
// is the single copy of the wg/timer bookkeeping shared by Stop and Close.
func (g *TimerGroup) clearTimerLocked() {
	if g.timer != nil {
		if g.timer.Stop() {
			g.wg.Done()
		}
		g.timer = nil
	}
}

// Stop stops the currently-scheduled callback (if any) without waiting
// for callbacks that have already started to finish. Unlike Close, Stop is
// not terminal: a later Reset may schedule a new timer (used to clear a
// stale timer before starting a fresh session).
func (g *TimerGroup) Stop() {
	g.mu.Lock()
	g.clearTimerLocked()
	g.mu.Unlock()
}

// Close permanently stops the group: it cancels any pending callback and
// makes every subsequent Reset a no-op. Use it at shutdown so a callback
// that reschedules itself (a two-phase end re-arming phase 2) cannot win a
// race against Stop and keep Wait blocked.
func (g *TimerGroup) Close() {
	g.mu.Lock()
	g.stopped = true
	g.clearTimerLocked()
	g.mu.Unlock()
}

// Wait blocks until all in-flight callbacks scheduled by Reset have
// returned.
func (g *TimerGroup) Wait() {
	g.wg.Wait()
}
