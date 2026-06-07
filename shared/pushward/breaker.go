package pushward

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when the circuit breaker is open and not
// allowing requests through.
var ErrCircuitOpen = errors.New("circuit breaker is open")

type circuitState int

const (
	stateClosed circuitState = iota
	stateOpen
	stateHalfOpen
)

// CircuitBreaker tracks consecutive failures and short-circuits calls when
// a threshold is reached. After a cooldown period it allows a single probe
// request (half-open state) to determine whether the backend has recovered.
type CircuitBreaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	state     circuitState
	openUntil time.Time
	cooldown  time.Duration
	now       func() time.Time // for testing
}

// NewCircuitBreaker returns a breaker that opens after threshold consecutive
// failures and stays open for cooldown before allowing a probe.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
	}
}

// Allow reports whether a request may proceed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateClosed:
		return true
	case stateOpen:
		if cb.now().After(cb.openUntil) {
			cb.state = stateHalfOpen
			return true // allow one probe
		}
		return false
	default: // stateHalfOpen — already probing
		return false
	}
}

// RecordSuccess records a successful request (a genuine 2xx). It both closes a
// half-open probe and resets the closed-state failure streak.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.state = stateClosed
}

// RecordReachable records an outcome that proves the backend is reachable but is
// not itself a success — a non-retryable 4xx, a resolved 409, or sustained 429
// throttling. In half-open it closes the probe (the backend responded, so it has
// recovered). In the closed state it deliberately leaves the failure streak
// intact: a reachable-but-erroring response is neither a success nor a fault, so
// it must not zero a streak of real 5xx/network faults that is climbing toward
// the open threshold (the breaker is shared relay-wide, and routine per-tenant
// 4xx/429 would otherwise perpetually reset it and prevent it from ever opening).
func (cb *CircuitBreaker) RecordReachable() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == stateHalfOpen {
		cb.state = stateClosed
		cb.failures = 0
	}
}

// RecordFailure records a failed request and may open the circuit.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateClosed:
		cb.failures++
		if cb.failures >= cb.threshold {
			cb.state = stateOpen
			cb.openUntil = cb.now().Add(cb.cooldown)
		}
	case stateHalfOpen:
		cb.state = stateOpen
		cb.openUntil = cb.now().Add(cb.cooldown)
	}
}

// Abort re-arms a half-open probe that never reached the backend (e.g. the
// request body failed to marshal, the request could not be built, or the
// context was cancelled during retry backoff). Without this, a probe admitted
// by Allow() that bypasses both RecordSuccess and RecordFailure would leave the
// breaker wedged in half-open forever — Allow() returns false for every
// subsequent caller and the circuit never recovers without a restart. Aborting
// returns the breaker to open with a fresh cooldown so a later probe can run.
// It is a no-op in the closed state: a local/transport hiccup must not count
// against a healthy circuit.
func (cb *CircuitBreaker) Abort() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == stateHalfOpen {
		cb.state = stateOpen
		cb.openUntil = cb.now().Add(cb.cooldown)
	}
}

// IsOpen reports whether the circuit breaker is currently open or half-open
// (i.e. not fully healthy). Use this for metrics gauges.
func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state != stateClosed
}
