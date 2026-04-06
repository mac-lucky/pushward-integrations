package poller

import (
	"testing"
	"time"
)

func TestStartStop(t *testing.T) {
	// Verify start/stop lifecycle without real HTTP calls.
	// metricsClient and pwClient are nil — the goroutine will tick but
	// poll() will panic if called, so we stop before the first tick.
	p := New(nil, nil, 1*time.Hour) // long interval so it won't tick
	p.Start("test-slug", "up")

	if p.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", p.ActiveCount())
	}

	// Start again — should be a no-op
	p.Start("test-slug", "up")
	if p.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d after duplicate start, want 1", p.ActiveCount())
	}

	p.Stop("test-slug")
	p.Wait()

	if p.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after stop, want 0", p.ActiveCount())
	}
}

func TestStopAll(t *testing.T) {
	p := New(nil, nil, 1*time.Hour)
	p.Start("slug-1", "up")
	p.Start("slug-2", "up")

	if p.ActiveCount() != 2 {
		t.Fatalf("ActiveCount = %d, want 2", p.ActiveCount())
	}

	p.StopAll()
	p.Wait()

	if p.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after StopAll, want 0", p.ActiveCount())
	}
}

func TestStopNonExistent(t *testing.T) {
	p := New(nil, nil, 1*time.Hour)
	p.Stop("does-not-exist") // should not panic
}
