package asr

import (
	"fmt"
	"sync"
	"time"
)

// defaultBreakerFailureThreshold / defaultBreakerCooldown are this
// package's circuit breaker defaults (see WithCircuitBreaker /
// WithSarvamCircuitBreaker): five consecutive full-retry-exhaustion
// failures trip the breaker, and it stays open for 10s before letting a
// single probe call through. This mirrors pkg/translate and pkg/tts's
// breakers exactly, but tracks a different unit of "call": here, one
// "call" is one StreamSession's attempt to establish its very first
// (initial) connection to the vendor, not an individual HTTP/WebSocket
// dial attempt. StartStream is invoked once per leg at Session
// construction (see pkg/langstream/session.go's NewSession) to open a
// brand-new streaming connection; if the vendor is down, every such
// StartStream call would otherwise pay the full ensureConnected
// dial-and-backoff cost (bounded by that recognizer's
// maxReconnectAttempts) before failing. This breaker avoids that once a
// sustained outage is already confirmed by several consecutive sessions
// that never managed to connect at all.
//
// Deliberately out of scope for this breaker: pkg/asr/backoff.go's
// reconnectBackoff / maxReconnectAttempts mechanism, used for
// *mid-session* reconnects after a WebSocket drop on a session that has
// already connected at least once. A session that connects successfully
// and only later drops and reconnects mid-stream never touches this
// breaker at all -- only the outcome of a session's very first connect
// attempt (success, or giving up without ever connecting) counts.
const (
	defaultBreakerFailureThreshold = 5
	defaultBreakerCooldown         = 10 * time.Second
)

// ErrCircuitOpen is wrapped (with vendor-specific context) and returned
// by StartStream when a call is rejected because its circuit breaker is
// open.
var ErrCircuitOpen = fmt.Errorf("circuit breaker open: too many consecutive failures, cooling down")

// circuitBreaker is a small, thread-safe fail-fast breaker layered on
// top of a Recognizer's StartStream/ensureConnected initial-connect
// logic (see backoff.go for the separate, untouched mid-session
// reconnect policy). It is shared, in shape, by both DeepgramRecognizer
// and SarvamRecognizer -- each holds its own *circuitBreaker instance,
// so the two vendors' health is tracked (and can trip) independently.
// Within one Recognizer, a single breaker instance is shared across
// every StartStream call made against it (and, transitively, every
// StreamSession it produces), exactly like pkg/translate's breaker is
// shared across every Translate call on one client.
//
// It tracks *consecutive full-connect-exhaustion* failures -- sessions
// whose very first connect attempt never succeeded, i.e. ensureConnected
// gave up (exhausted maxReconnectAttempts, or the vendor refused the
// dial and the session gave up on ever connecting) before a single
// WebSocket connection was ever established for that session. A session
// that connects successfully at least once, then later drops and
// reconnects mid-stream via the existing reconnectBackoff path, never
// touches the breaker at all, regardless of whether that later
// mid-stream reconnect succeeds or fails.
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
// recordSuccess, recordFailure, or abort.
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

// recordSuccess reports that a call let through by allow() succeeded
// (here: a session's very first connect attempt established a live
// connection). This always fully closes the breaker and resets the
// consecutive-failure count, whether the call was an ordinary session or
// the cooldown probe session.
func (b *circuitBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFails = 0
	b.open = false
	b.probeInFlight = false
}

// recordFailure reports that a call let through by allow() exhausted its
// full connect budget without ever succeeding (here: a session gave up
// on its very first connect attempt without ever establishing a live
// connection). If the breaker was already open (a failed probe), it
// stays open and the cooldown window restarts. If the breaker was
// closed, this counts toward the consecutive-failure threshold, tripping
// the breaker once the threshold is reached.
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
// cancelled before the session's first connect attempt ever resolved, or
// the session was closed/torn down before PushAudio ever attempted to
// connect it at all. It only clears the in-flight probe marker; it never
// flips open/closed state or touches the consecutive-failure count,
// since neither of those outcomes is a vendor-health signal. Safe to
// call even when the breaker was closed and no probe was in flight (a
// no-op in that case) -- callers should invoke it unconditionally
// (typically via defer, or at session-teardown time) after any call for
// which allow() returned true, unless recordSuccess or recordFailure was
// already called.
func (b *circuitBreaker) abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
}
