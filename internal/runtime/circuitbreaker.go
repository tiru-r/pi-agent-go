package runtime

import (
	"sync"
	"time"
)

// CBState is the state of a circuit breaker.
type CBState int

const (
	CBClosed   CBState = iota // normal operation
	CBOpen                    // fast-fail; no requests forwarded
	CBHalfOpen                // probe requests allowed
)

// CircuitBreaker implements the three-state circuit breaker pattern.
// Safe for concurrent use.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            CBState
	failures         int   // consecutive failures in CLOSED
	successes        int   // consecutive successes in HALF-OPEN
	failureThreshold int   // failures to trip CLOSED → OPEN
	successThreshold int   // successes in HALF-OPEN to close
	timeout          time.Duration
	openedAt         time.Time
}

// NewCircuitBreaker constructs a breaker with the given thresholds and timeout.
// failureThreshold: consecutive failures before opening.
// successThreshold: consecutive successes in HALF-OPEN before closing.
// timeout: duration in OPEN state before probing (HALF-OPEN).
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CBClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		timeout:          timeout,
	}
}

// Allow returns true when the breaker permits a call. It transitions
// OPEN → HALF-OPEN when the timeout has elapsed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CBClosed:
		return true
	case CBOpen:
		if time.Since(cb.openedAt) >= cb.timeout {
			cb.state = CBHalfOpen
			cb.successes = 0
			return true // one probe allowed
		}
		return false
	case CBHalfOpen:
		return true
	}
	return false
}

// RecordSuccess records a successful call result.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CBClosed:
		cb.failures = 0
	case CBHalfOpen:
		cb.successes++
		if cb.successes >= cb.successThreshold {
			cb.state = CBClosed
			cb.failures = 0
			cb.successes = 0
		}
	}
}

// RecordFailure records a failed call result.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CBClosed:
		cb.failures++
		if cb.failures >= cb.failureThreshold {
			cb.state = CBOpen
			cb.openedAt = time.Now()
		}
	case CBHalfOpen:
		// Any failure in HALF-OPEN trips back to OPEN.
		cb.state = CBOpen
		cb.openedAt = time.Now()
		cb.successes = 0
	}
}

// State returns the current breaker state.
func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// circuitBreakerRegistry holds one circuit breaker per stage.
type circuitBreakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*CircuitBreaker
	// template parameters applied to newly-created breakers
	failureThreshold int
	successThreshold int
	timeout          time.Duration
}

func newCircuitBreakerRegistry() *circuitBreakerRegistry {
	return &circuitBreakerRegistry{
		breakers:         make(map[string]*CircuitBreaker),
		failureThreshold: 5,
		successThreshold: 2,
		timeout:          30 * time.Second,
	}
}

func (r *circuitBreakerRegistry) get(stage string) *CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	cb, ok := r.breakers[stage]
	if !ok {
		cb = NewCircuitBreaker(r.failureThreshold, r.successThreshold, r.timeout)
		r.breakers[stage] = cb
	}
	return cb
}

// States returns a snapshot of every stage's breaker state.
func (r *circuitBreakerRegistry) States() map[string]CBState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]CBState, len(r.breakers))
	for k, cb := range r.breakers {
		out[k] = cb.State()
	}
	return out
}
