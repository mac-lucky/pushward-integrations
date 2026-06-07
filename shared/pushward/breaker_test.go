package pushward

import (
	"testing"
	"time"
)

func TestCircuitBreaker_ClosedAllowsRequests(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	if !cb.Allow() {
		t.Error("expected closed breaker to allow requests")
	}
	if cb.IsOpen() {
		t.Error("expected breaker to not be open")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	cb.now = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.IsOpen() {
		t.Error("expected breaker to still be closed after 2 failures")
	}

	cb.RecordFailure() // 3rd failure hits threshold
	if !cb.IsOpen() {
		t.Error("expected breaker to be open after 3 failures")
	}
	if cb.Allow() {
		t.Error("expected open breaker to reject requests")
	}
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }

	cb.RecordFailure() // opens circuit
	if !cb.IsOpen() {
		t.Fatal("expected breaker to be open")
	}

	// Advance past cooldown
	now = now.Add(6 * time.Second)
	if !cb.Allow() {
		t.Error("expected half-open breaker to allow one probe after cooldown")
	}

	// Second call while half-open should be rejected
	if cb.Allow() {
		t.Error("expected half-open breaker to reject second concurrent request")
	}
}

func TestCircuitBreaker_HalfOpenToClosedOnSuccess(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }

	cb.RecordFailure() // opens
	now = now.Add(6 * time.Second)
	cb.Allow() // transitions to half-open

	cb.RecordSuccess() // should close

	if cb.IsOpen() {
		t.Error("expected breaker to be closed after success in half-open")
	}
	if !cb.Allow() {
		t.Error("expected closed breaker to allow requests")
	}
}

func TestCircuitBreaker_HalfOpenToOpenOnFailure(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }

	cb.RecordFailure() // opens
	now = now.Add(6 * time.Second)
	cb.Allow() // transitions to half-open

	cb.RecordFailure() // should re-open

	if !cb.IsOpen() {
		t.Error("expected breaker to be open again after failure in half-open")
	}
	if cb.Allow() {
		t.Error("expected re-opened breaker to reject requests before cooldown")
	}
}

func TestCircuitBreaker_AbortReArmsHalfOpenProbe(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }

	cb.RecordFailure() // opens
	now = now.Add(6 * time.Second)
	if !cb.Allow() { // transitions to half-open, consumes the probe
		t.Fatal("expected half-open probe to be allowed after cooldown")
	}

	// Probe never reached the backend (e.g. ctx cancelled): abort it.
	cb.Abort()

	// Must not wedge: a fresh probe must be admitted after the new cooldown
	// rather than Allow() returning false forever.
	if cb.Allow() {
		t.Error("expected aborted probe to re-arm the open timer, rejecting before cooldown")
	}
	now = now.Add(6 * time.Second)
	if !cb.Allow() {
		t.Error("expected a new probe to be allowed after the re-armed cooldown — breaker wedged")
	}
}

func TestCircuitBreaker_AbortInClosedIsNoOp(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	cb.Abort() // closed state: a local hiccup must not penalize a healthy circuit
	if cb.IsOpen() {
		t.Error("expected Abort in closed state to be a no-op")
	}
	if !cb.Allow() {
		t.Error("expected closed breaker to still allow requests after Abort")
	}
}

func TestCircuitBreaker_ReachableDoesNotZeroClosedStreak(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)

	// A climbing streak of real faults must not be wiped by interleaved
	// reachable-but-erroring outcomes (4xx/409/429). At 10k-tenant scale the
	// breaker is shared, so routine per-tenant 4xx must not perpetually reset it.
	cb.RecordFailure()
	cb.RecordReachable() // 4xx between faults — must NOT zero the streak
	cb.RecordFailure()
	cb.RecordReachable()
	if cb.IsOpen() {
		t.Fatal("expected breaker still closed after 2 faults + interleaved reachables")
	}

	cb.RecordFailure() // 3rd genuine fault — streak preserved, hits threshold
	if !cb.IsOpen() {
		t.Error("expected breaker to open: RecordReachable must not have reset the fault streak")
	}
}

func TestCircuitBreaker_ReachableClosesHalfOpenProbe(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(1, 5*time.Second)
	cb.now = func() time.Time { return now }

	cb.RecordFailure() // opens
	now = now.Add(6 * time.Second)
	if !cb.Allow() { // half-open probe
		t.Fatal("expected half-open probe after cooldown")
	}

	// A 4xx during the probe proves the backend recovered (it responded), so the
	// breaker must close rather than stay wedged half-open.
	cb.RecordReachable()
	if cb.IsOpen() {
		t.Error("expected breaker closed after a reachable outcome in half-open")
	}
	if !cb.Allow() {
		t.Error("expected closed breaker to allow requests after recovery")
	}
}

func TestCircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)

	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess() // should reset failures to 0

	// Two more failures should not trip since count was reset
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.IsOpen() {
		t.Error("expected breaker to be closed; success should have reset failure count")
	}

	cb.RecordFailure() // 3rd failure since reset
	if !cb.IsOpen() {
		t.Error("expected breaker to open after 3 failures since last reset")
	}
}
