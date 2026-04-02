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
	stateClosed   circuitState = iota
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

// RecordSuccess records a successful request.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.state = stateClosed
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

// IsOpen reports whether the circuit breaker is currently open or half-open
// (i.e. not fully healthy). Use this for metrics gauges.
func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state != stateClosed
}
