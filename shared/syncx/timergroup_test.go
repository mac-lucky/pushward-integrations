package syncx

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimerGroup(t *testing.T) {
	t.Run("fires callback", func(t *testing.T) {
		var g TimerGroup
		var n atomic.Int32
		g.Reset(5*time.Millisecond, func() { n.Add(1) })
		g.Wait()
		if n.Load() != 1 {
			t.Errorf("got %d, want 1", n.Load())
		}
	})

	t.Run("reset replaces prior timer before it fires", func(t *testing.T) {
		var g TimerGroup
		var first, second atomic.Int32
		g.Reset(50*time.Millisecond, func() { first.Add(1) })
		time.Sleep(5 * time.Millisecond)
		g.Reset(5*time.Millisecond, func() { second.Add(1) })
		g.Wait()
		if first.Load() != 0 {
			t.Errorf("first callback fired (got %d)", first.Load())
		}
		if second.Load() != 1 {
			t.Errorf("second not fired (got %d)", second.Load())
		}
	})

	t.Run("stop prevents pending callback", func(t *testing.T) {
		var g TimerGroup
		var n atomic.Int32
		g.Reset(50*time.Millisecond, func() { n.Add(1) })
		g.Stop()
		g.Wait()
		if n.Load() != 0 {
			t.Errorf("callback fired after Stop (got %d)", n.Load())
		}
	})

	t.Run("stop does not wait for in-flight", func(t *testing.T) {
		var g TimerGroup
		release := make(chan struct{})
		var done atomic.Bool
		g.Reset(1*time.Millisecond, func() {
			<-release
			done.Store(true)
		})
		time.Sleep(10 * time.Millisecond) // callback should be running
		start := time.Now()
		g.Stop()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("Stop blocked for %v — should not wait", elapsed)
		}
		close(release)
		g.Wait()
		if !done.Load() {
			t.Error("callback didn't complete")
		}
	})

	t.Run("wait blocks until callback returns", func(t *testing.T) {
		var g TimerGroup
		var n atomic.Int32
		g.Reset(1*time.Millisecond, func() {
			time.Sleep(20 * time.Millisecond)
			n.Add(1)
		})
		g.Wait()
		if n.Load() != 1 {
			t.Error("Wait returned before callback finished")
		}
	})

	t.Run("wait with no activity returns immediately", func(t *testing.T) {
		var g TimerGroup
		done := make(chan struct{})
		go func() { g.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Error("Wait blocked with nothing scheduled")
		}
	})

	t.Run("stop on empty is noop", func(t *testing.T) {
		var g TimerGroup
		g.Stop()
		g.Stop()
	})

	t.Run("two-phase handoff pattern", func(t *testing.T) {
		var g TimerGroup
		var phase1, phase2 atomic.Int32
		g.Reset(5*time.Millisecond, func() {
			phase1.Add(1)
			g.Reset(5*time.Millisecond, func() { phase2.Add(1) })
		})
		time.Sleep(50 * time.Millisecond)
		g.Wait()
		if phase1.Load() != 1 || phase2.Load() != 1 {
			t.Errorf("phase1=%d phase2=%d, want 1/1", phase1.Load(), phase2.Load())
		}
	})

	t.Run("concurrent resets do not leak wg", func(t *testing.T) {
		var g TimerGroup
		var wg sync.WaitGroup
		var fired atomic.Int32
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.Reset(5*time.Millisecond, func() { fired.Add(1) })
			}()
		}
		wg.Wait()
		g.Wait()
		// At most one callback from the last surviving Reset should fire,
		// but concurrent races may schedule more; just ensure Wait returns.
		if fired.Load() < 1 {
			t.Errorf("no callbacks fired; fired=%d", fired.Load())
		}
	})

	t.Run("reset after wait", func(t *testing.T) {
		var g TimerGroup
		var n atomic.Int32
		g.Reset(1*time.Millisecond, func() { n.Add(1) })
		g.Wait()
		g.Reset(1*time.Millisecond, func() { n.Add(1) })
		g.Wait()
		if n.Load() != 2 {
			t.Errorf("got %d, want 2", n.Load())
		}
	})
}
