package translate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// alwaysFailServer replies with a 500 to every request so every attempt
// in Translate's retry loop is transient and fails, exhausting the full
// retry budget on every call (the precondition for the circuit breaker
// to count a "consecutive failure").
func alwaysFailServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	}))
	return srv, &attempts
}

func TestCircuitBreaker_OpensAfterConsecutiveFullExhaustions(t *testing.T) {
	srv, attempts := alwaysFailServer(t)
	defer srv.Close()

	const threshold = 2
	tr, err := NewGPT4oTranslator(
		WithBaseURL(srv.URL), WithAPIKey("test-api-key"),
		WithCircuitBreaker(threshold, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Each of these calls exhausts the full gpt4oMaxAttempts retry
	// budget (every attempt gets a 500), so after `threshold` such
	// calls the breaker should trip.
	for i := 0; i < threshold; i++ {
		if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
			t.Fatalf("call %d: expected error from persistent 500s, got nil", i)
		}
	}
	wantAttempts := threshold * gpt4oMaxAttempts
	if got := int(atomic.LoadInt32(attempts)); got != wantAttempts {
		t.Fatalf("attempts after %d exhausted calls = %d, want %d (breaker should not have opened yet)", threshold, got, wantAttempts)
	}

	// The breaker should now be open: the next call must fail
	// immediately, without attempting the vendor at all (no new HTTP
	// requests) and well within the time a single backoff delay would
	// take.
	start := time.Now()
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error while the breaker is open, got nil")
	}
	if !strings.Contains(err.Error(), "circuit breaker") {
		t.Errorf("expected a circuit-breaker error, got: %v", err)
	}
	if got := int(atomic.LoadInt32(attempts)); got != wantAttempts {
		t.Errorf("attempts while breaker open = %d, want unchanged %d (no vendor call should have been made)", got, wantAttempts)
	}
	if elapsed >= gpt4oRetryBaseDelay {
		t.Errorf("elapsed = %v while breaker open, want well under one backoff delay (%v) since no retries should happen", elapsed, gpt4oRetryBaseDelay)
	}
}

// TestCircuitBreaker_OpenErrorIsErrCircuitOpen confirms that the error
// returned when the breaker rejects a call satisfies errors.Is against
// the exported ErrCircuitOpen sentinel, even though Translate wraps it
// with vendor-specific context (see gpt4o.go). Callers outside this
// package (e.g. the orchestrator in pkg/langstream) rely on this to
// distinguish "our own circuit breaker just rejected this call" from any
// other Translate failure.
func TestCircuitBreaker_OpenErrorIsErrCircuitOpen(t *testing.T) {
	srv, _ := alwaysFailServer(t)
	defer srv.Close()

	const threshold = 1
	tr, err := NewGPT4oTranslator(
		WithBaseURL(srv.URL), WithAPIKey("test-api-key"),
		WithCircuitBreaker(threshold, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Trip the breaker.
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected the first call to fail (persistent 500s), got nil")
	}

	// The next call should be rejected by the (now open) breaker with an
	// error that wraps ErrCircuitOpen.
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected an error while the breaker is open, got nil")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("errors.Is(err, ErrCircuitOpen) = false for err = %v, want true", err)
	}
}

func TestCircuitBreaker_ProbeAfterCooldownSucceeds(t *testing.T) {
	var failing int32 = 1 // 1 = fail every request, 0 = succeed
	var attempts int32
	sse := sseChunk("recovered") + "data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if atomic.LoadInt32(&failing) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("down"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	const threshold = 1
	const cooldown = 40 * time.Millisecond
	tr, err := NewGPT4oTranslator(
		WithBaseURL(srv.URL), WithAPIKey("test-api-key"),
		WithCircuitBreaker(threshold, cooldown),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Trip the breaker with one fully-exhausted failing call.
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected the first call to fail (persistent 500s), got nil")
	}

	// Immediately after tripping, calls must fail fast (breaker open,
	// cooldown not yet elapsed).
	attemptsBeforeProbe := atomic.LoadInt32(&attempts)
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an immediate failure while breaker is open, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != attemptsBeforeProbe {
		t.Errorf("attempts while still cooling down = %d, want unchanged %d", got, attemptsBeforeProbe)
	}

	// Let the cooldown elapse and flip the vendor back to healthy, then
	// the next call should be let through as the probe and succeed.
	time.Sleep(cooldown + 20*time.Millisecond)
	atomic.StoreInt32(&failing, 0)

	chunk, err := tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("expected the post-cooldown probe to succeed, got error: %v", err)
	}
	if chunk.Text != "recovered" {
		t.Errorf("chunk.Text = %q, want %q", chunk.Text, "recovered")
	}

	// Breaker should now be fully closed: a subsequent call must not be
	// fail-fast even if we flip the vendor back to failing and don't
	// wait out any cooldown -- it should pay the normal retry budget.
	atomic.StoreInt32(&failing, 1)
	attemptsBeforeNext := atomic.LoadInt32(&attempts)
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an error from persistent 500s, got nil")
	}
	if got := atomic.LoadInt32(&attempts) - attemptsBeforeNext; int(got) != gpt4oMaxAttempts {
		t.Errorf("attempts after breaker closed = %d, want gpt4oMaxAttempts = %d (breaker should have retried normally, not fail-fast)", got, gpt4oMaxAttempts)
	}
}

func TestCircuitBreaker_FailedProbeReopensCooldown(t *testing.T) {
	srv, attempts := alwaysFailServer(t)
	defer srv.Close()

	const threshold = 1
	const cooldown = 30 * time.Millisecond
	tr, err := NewGPT4oTranslator(
		WithBaseURL(srv.URL), WithAPIKey("test-api-key"),
		WithCircuitBreaker(threshold, cooldown),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Trip the breaker.
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}
	attemptsAfterTrip := atomic.LoadInt32(attempts)

	// Wait out the cooldown; the vendor is still down, so the probe call
	// should be let through (paying the full retry budget), fail, and
	// reopen the cooldown.
	time.Sleep(cooldown + 20*time.Millisecond)
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected the probe call to fail (vendor still down), got nil")
	}
	attemptsAfterProbe := atomic.LoadInt32(attempts)
	if got := attemptsAfterProbe - attemptsAfterTrip; int(got) != gpt4oMaxAttempts {
		t.Errorf("attempts during probe = %d, want gpt4oMaxAttempts = %d (probe must use full retry logic)", got, gpt4oMaxAttempts)
	}

	// Immediately after the failed probe, the breaker must be open again
	// (fresh cooldown): the very next call must fail fast without
	// attempting the vendor.
	start := time.Now()
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an immediate failure after the probe reopened the cooldown, got nil")
	}
	elapsed := time.Since(start)
	if got := atomic.LoadInt32(attempts); got != attemptsAfterProbe {
		t.Errorf("attempts right after failed probe = %d, want unchanged %d", got, attemptsAfterProbe)
	}
	if elapsed >= gpt4oRetryBaseDelay {
		t.Errorf("elapsed = %v after reopened cooldown, want well under one backoff delay (%v)", elapsed, gpt4oRetryBaseDelay)
	}
}

func TestCircuitBreaker_PermanentErrorsDoNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	tr, err := NewGPT4oTranslator(
		WithBaseURL(srv.URL), WithAPIKey("test-api-key"),
		WithCircuitBreaker(1, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Many consecutive permanent (400) failures must never trip the
	// breaker, since a bad request/bad auth is a client-configuration
	// problem, not vendor health, and the breaker's job is specifically
	// to detect vendor outages.
	for i := 0; i < 5; i++ {
		if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
			t.Fatalf("call %d: expected 400 error, got nil", i)
		}
	}

	if tr.breaker.open {
		t.Error("breaker should not be open after only permanent (non-retryable) failures")
	}
}

func TestCircuitBreaker_DefaultEnabledWithoutOption(t *testing.T) {
	tr, err := NewGPT4oTranslator(WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	if tr.breaker == nil {
		t.Fatal("expected a non-nil default circuit breaker when WithCircuitBreaker is not used")
	}
	if tr.breaker.threshold != defaultBreakerFailureThreshold {
		t.Errorf("default threshold = %d, want %d", tr.breaker.threshold, defaultBreakerFailureThreshold)
	}
	if tr.breaker.cooldown != defaultBreakerCooldown {
		t.Errorf("default cooldown = %v, want %v", tr.breaker.cooldown, defaultBreakerCooldown)
	}
}

// --- unit tests on circuitBreaker directly, including concurrency -------

func TestCircuitBreaker_AllowRecordSuccessFailureUnit(t *testing.T) {
	b := newCircuitBreaker(2, 20*time.Millisecond)

	if !b.allow() {
		t.Fatal("expected allow() to be true on a fresh closed breaker")
	}
	b.recordFailure()
	if b.open {
		t.Fatal("breaker should still be closed after 1 of 2 failures")
	}

	if !b.allow() {
		t.Fatal("expected allow() to be true before threshold reached")
	}
	b.recordFailure()
	if !b.open {
		t.Fatal("breaker should be open after reaching the threshold")
	}

	if b.allow() {
		t.Fatal("expected allow() to be false immediately after opening (still cooling down)")
	}

	time.Sleep(30 * time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow() to let exactly one probe through after cooldown elapses")
	}
	// A concurrent caller during the probe must be rejected.
	if b.allow() {
		t.Fatal("expected allow() to reject a second concurrent probe attempt")
	}

	b.recordSuccess()
	if b.open {
		t.Fatal("breaker should be closed after a successful probe")
	}
	if !b.allow() {
		t.Fatal("expected allow() to be true after breaker closed")
	}
}

func TestCircuitBreaker_ConcurrentAccessIsRace_free(t *testing.T) {
	b := newCircuitBreaker(5, 5*time.Millisecond)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if b.allow() {
					if j%2 == 0 {
						b.recordSuccess()
					} else {
						b.recordFailure()
					}
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestCircuitBreaker_AbortReleasesStuckProbe is a regression test for a
// real bug found during Sprint 10 integration: if a cooldown-elapsed
// probe call (allow() returning true with probeInFlight set) ends without
// ever calling recordSuccess or recordFailure -- e.g. the caller's ctx
// was cancelled mid-retry, or a permanent (non-retryable) error occurred
// while probing -- probeInFlight was never cleared, so the breaker got
// stuck open forever: allow() would keep returning false indefinitely,
// even arbitrarily far past openUntil, because the "probe already in
// flight" check came before checking whether that stale probe had ever
// resolved. abort() (called via defer in Translate/SynthesizeStream
// whenever allow() returned true but neither recordSuccess nor
// recordFailure ran) fixes this by clearing probeInFlight without
// touching open/closed state or the failure count.
func TestCircuitBreaker_AbortReleasesStuckProbe(t *testing.T) {
	b := newCircuitBreaker(1, 5*time.Millisecond)
	b.recordFailure() // trips the breaker (threshold 1)
	if !b.open {
		t.Fatal("expected breaker to be open after tripping")
	}

	// Simulate the cooldown elapsing, then a probe being let through.
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	if !b.allow() {
		t.Fatal("expected the cooldown-elapsed probe to be allowed")
	}

	// Without abort(), a stuck probe (ctx cancelled, permanent error,
	// crash, etc.) would leave probeInFlight permanently true.
	if !b.probeInFlight {
		t.Fatal("expected probeInFlight to be true after the probe was let through")
	}
	if b.allow() {
		t.Fatal("expected a second concurrent allow() to be rejected while the probe is in flight")
	}

	// Simulate the probe's caller aborting without ever settling the
	// breaker (the bug scenario).
	b.abort()

	if b.probeInFlight {
		t.Fatal("abort() should have cleared probeInFlight")
	}
	// Even arbitrarily far in the future, allow() must eventually let a
	// fresh probe through again -- before the fix, this would fail
	// forever once probeInFlight got stuck.
	b.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if !b.allow() {
		t.Fatal("breaker should allow a fresh probe after abort() released the stuck one, even much later")
	}
}

func TestCircuitBreaker_AbortIsNoOpWhenClosed(t *testing.T) {
	b := newCircuitBreaker(5, 5*time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow() true on a fresh closed breaker")
	}
	b.abort() // should not panic or affect closed-state behavior
	if b.open {
		t.Fatal("abort() must never open a closed breaker")
	}
	if !b.allow() {
		t.Fatal("breaker should remain closed and allow calls after abort() on a closed breaker")
	}
}
