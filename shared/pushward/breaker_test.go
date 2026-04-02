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
