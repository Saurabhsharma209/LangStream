package tts

import (
	"fmt"
	"sync"
	"time"
)

// defaultBreakerFailureThreshold / defaultBreakerCooldown are this
// package's circuit breaker defaults (see WithCircuitBreaker /
// WithElevenLabsCircuitBreaker): five consecutive full-retry-exhaustion
// failures trip the breaker, and it stays open for 10s before letting a
// single probe call through. This avoids paying the full retry-backoff
// latency budget on every SynthesizeStream call during a sustained
// vendor outage, once the outage is already confirmed by several
// consecutive fully-exhausted calls. An isolated flaky call (one attempt
// fails but a retry within the same call still succeeds) never trips the
// breaker; only several consecutive full exhaustions do.
const (
	defaultBreakerFailureThreshold = 5
	defaultBreakerCooldown         = 10 * time.Second
)

// ErrCircuitOpen is wrapped (with vendor-specific context) and returned
// by SynthesizeStream when a call is rejected because its circuit
// breaker is open.
var ErrCircuitOpen = fmt.Errorf("circuit breaker open: too many consecutive failures, cooling down")

// circuitBreaker is a small, thread-safe fail-fast breaker layered on
// top of SynthesizeStream's retry-with-backoff logic (see backoff.go).
// It is shared, in shape, by both CartesiaSynthesizer and
// ElevenLabsSynthesizer -- each holds its own *circuitBreaker instance,
// so the two vendors' health is tracked (and can trip) independently.
//
// It tracks *consecutive full-retry-exhaustion* failures -- calls where
// every attempt in SynthesizeStream's connect/request retry loop failed
// with a transient error -- not every individual attempt, and not
// permanent (4xx) failures, which already fail fast and never touch the
// breaker.
//
// States (derived from the fields below, no separate enum):
//   - closed: open == false. Every call proceeds normally.
//   - open, cooling down: open == true && now < openUntil. allow()
//     returns false: the call fails immediately, no attempt, no backoff.
//   - open, cooldown elapsed (probing): open == true && now >= openUntil.
//     allow() lets exactly one call through as a probe (guarded by
//     probeInFlight); a successful probe (recordSuccess) closes the
//     breaker, a failed probe (recordFailure) reopens the cooldown.
type circuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration

	open             bool
	consecutiveFails int
	openUntil        time.Time
	probeInFlight    bool

	now func() time.Time // overridable in tests; defaults to time.Now
}

// newCircuitBreaker constructs a circuitBreaker. threshold <= 0 and
// cooldown <= 0 each independently fall back to this package's defaults.
func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	if threshold <= 0 {
		threshold = defaultBreakerFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = defaultBreakerCooldown
	}
	return &circuitBreaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// allow reports whether a call may proceed: true for a closed breaker or
// for the single cooldown-elapsed probe attempt, false otherwise. Every
// true result must be paired with exactly one later call to
// recordSuccess or recordFailure.
func (b *circuitBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.now().Before(b.openUntil) {
		return false
	}
	if b.probeInFlight {
		return false
	}
	b.probeInFlight = true
	return true
}

// recordSuccess reports that a call let through by allow() succeeded.
// This always fully closes the breaker and resets the consecutive-
// failure count, whether the call was an ordinary request or the
// cooldown probe.
func (b *circuitBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFails = 0
	b.open = false
	b.probeInFlight = false
}

// recordFailure reports that a call let through by allow() exhausted its
// full retry budget without succeeding. If the breaker was already open
// (a failed probe), it stays open and the cooldown window restarts. If
// the breaker was closed, this counts toward the consecutive-failure
// threshold, tripping the breaker once the threshold is reached.
func (b *circuitBreaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
	b.consecutiveFails++
	if b.open || b.consecutiveFails >= b.threshold {
		b.open = true
		b.openUntil = b.now().Add(b.cooldown)
	}
}

// abort reports that a call let through by allow() ended without a
// definitive success or failure verdict -- e.g. the caller's context was
// cancelled mid-retry, or a permanent non-retryable error occurred while
// a cooldown probe was in flight. It only clears the in-flight probe
// marker; it never flips open/closed state or touches the
// consecutive-failure count, since neither ctx cancellation nor an
// already-excluded permanent error is a vendor-health signal. Safe to
// call even when the breaker was closed and no probe was in flight (a
// no-op in that case) -- callers should invoke it unconditionally
// (typically via defer) after any call for which allow() returned true,
// unless recordSuccess or recordFailure was already called.
func (b *circuitBreaker) abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
}
