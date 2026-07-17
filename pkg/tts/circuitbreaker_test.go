package tts

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

// --- direct unit tests on circuitBreaker ---------------------------------

func TestTTSCircuitBreaker_AllowRecordSuccessFailureUnit(t *testing.T) {
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

func TestTTSCircuitBreaker_ConcurrentAccess(t *testing.T) {
	b := newCircuitBreaker(5, 5*time.Millisecond)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if b.allow() {
					if (n+j)%2 == 0 {
						b.recordSuccess()
					} else {
						b.recordFailure()
					}
				}
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// --- Cartesia: circuit breaker wired into SynthesizeStream ---------------

// newToggleCartesiaServer starts a fake Cartesia WebSocket server whose
// /tts/websocket handler rejects the handshake with a 500 whenever
// *failing == 1, or completes a full handshake + immediate {"type":
// "done"} whenever *failing == 0, letting tests flip vendor health at
// will.
func newToggleCartesiaServer(t *testing.T, failing *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(failing) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("down"))
			return
		}
		acceptCartesiaHandshakeAndFinish(t, w, r)
	})
	return httptest.NewServer(mux)
}

func TestCartesiaCircuitBreaker_OpensAfterConsecutiveFullExhaustions(t *testing.T) {
	var failing int32 = 1
	srv := newToggleCartesiaServer(t, &failing)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	const threshold = 2
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(threshold, 10*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	for i := 0; i < threshold; i++ {
		if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
			t.Fatalf("call %d: expected error from persistent 500s, got nil", i)
		}
	}

	// Breaker should now be open: fail fast, no handshake attempt, well
	// under one backoff delay.
	start := time.Now()
	_, err = c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error while breaker is open, got nil")
	}
	if !strings.Contains(err.Error(), "circuit breaker") {
		t.Errorf("expected a circuit-breaker error, got: %v", err)
	}
	if elapsed >= cartesiaRetryBaseDelay {
		t.Errorf("elapsed = %v while breaker open, want well under one backoff delay (%v)", elapsed, cartesiaRetryBaseDelay)
	}
}

// TestCartesiaCircuitBreaker_OpenErrorIsErrCircuitOpen confirms that the
// error returned when Cartesia's breaker rejects a call satisfies
// errors.Is against the exported ErrCircuitOpen sentinel, even though
// SynthesizeStream wraps it with vendor-specific context (see
// cartesia.go). Callers outside this package (e.g. the orchestrator in
// pkg/langstream) rely on this to distinguish "our own circuit breaker
// just rejected this call" from any other SynthesizeStream failure.
func TestCartesiaCircuitBreaker_OpenErrorIsErrCircuitOpen(t *testing.T) {
	var failing int32 = 1
	srv := newToggleCartesiaServer(t, &failing)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	const threshold = 1
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(threshold, 10*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}

	_, err = c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error while the breaker is open, got nil")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("errors.Is(err, ErrCircuitOpen) = false for err = %v, want true", err)
	}
}

func TestCartesiaCircuitBreaker_ProbeAfterCooldownSucceeds(t *testing.T) {
	var failing int32 = 1
	srv := newToggleCartesiaServer(t, &failing)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	const cooldown = 40 * time.Millisecond
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(1, cooldown))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	// Trip the breaker.
	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}

	// Still cooling down: fails fast.
	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an immediate failure while breaker is open, got nil")
	}

	// Let the cooldown elapse and heal the vendor; the probe should
	// succeed and close the breaker.
	time.Sleep(cooldown + 20*time.Millisecond)
	atomic.StoreInt32(&failing, 0)

	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("expected the post-cooldown probe to succeed, got error: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	// Breaker closed: a subsequent call should pay the normal retry
	// budget rather than fail fast, even with no cooldown wait.
	atomic.StoreInt32(&failing, 1)
	start := time.Now()
	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an error from persistent 500s, got nil")
	}
	elapsed := time.Since(start)
	// With cartesiaMaxAttempts=3, at least one backoff sleep must have
	// elapsed -- proof the breaker retried normally instead of failing
	// fast again immediately.
	if elapsed < cartesiaRetryBaseDelay {
		t.Errorf("elapsed = %v, expected at least one retry backoff delay (%v), suggesting the breaker did not retry normally", elapsed, cartesiaRetryBaseDelay)
	}
}

func TestCartesiaCircuitBreaker_FailedProbeReopensCooldown(t *testing.T) {
	var failing int32 = 1
	srv := newToggleCartesiaServer(t, &failing)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	const cooldown = 30 * time.Millisecond
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(1, cooldown))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}

	// Vendor still down when cooldown elapses: probe is let through
	// (full retry budget) and fails, reopening the cooldown.
	time.Sleep(cooldown + 20*time.Millisecond)
	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the probe call to fail (vendor still down), got nil")
	}

	// Immediately after: breaker open again, fails fast.
	start := time.Now()
	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an immediate failure after the probe reopened the cooldown, got nil")
	}
	elapsed := time.Since(start)
	if elapsed >= cartesiaRetryBaseDelay {
		t.Errorf("elapsed = %v after reopened cooldown, want well under one backoff delay (%v)", elapsed, cartesiaRetryBaseDelay)
	}
}

func TestCartesiaCircuitBreaker_DoesNotTripOnPermanentErrors(t *testing.T) {
	srv, _ := newCountingCartesiaServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	})
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(1, 10*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
			t.Fatalf("call %d: expected 400 error, got nil", i)
		}
	}
	if c.breaker.open {
		t.Error("breaker should not open after only permanent (non-retryable) failures")
	}
}

func TestCartesiaCircuitBreaker_DefaultEnabledWithoutOption(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "test-key")
	c, err := NewCartesiaSynthesizer()
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	if c.breaker == nil {
		t.Fatal("expected a non-nil default circuit breaker")
	}
	if c.breaker.threshold != defaultBreakerFailureThreshold {
		t.Errorf("default threshold = %d, want %d", c.breaker.threshold, defaultBreakerFailureThreshold)
	}
	if c.breaker.cooldown != defaultBreakerCooldown {
		t.Errorf("default cooldown = %v, want %v", c.breaker.cooldown, defaultBreakerCooldown)
	}
}

// --- ElevenLabs: circuit breaker wired into SynthesizeStream -------------

func newToggleElevenLabsServer(t *testing.T, failing *int32, pcm []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/text-to-speech/", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(failing) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("down"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pcm)
	})
	return httptest.NewServer(mux)
}

func TestElevenLabsCircuitBreaker_OpensAfterConsecutiveFullExhaustions(t *testing.T) {
	var failing int32 = 1
	srv := newToggleElevenLabsServer(t, &failing, []byte{1, 2, 3, 4})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	const threshold = 2
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(threshold, 10*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	for i := 0; i < threshold; i++ {
		if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
			t.Fatalf("call %d: expected error from persistent 500s, got nil", i)
		}
	}

	start := time.Now()
	_, err = e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error while breaker is open, got nil")
	}
	if !strings.Contains(err.Error(), "circuit breaker") {
		t.Errorf("expected a circuit-breaker error, got: %v", err)
	}
	if elapsed >= elevenlabsRetryBaseDelay {
		t.Errorf("elapsed = %v while breaker open, want well under one backoff delay (%v)", elapsed, elevenlabsRetryBaseDelay)
	}
}

// TestElevenLabsCircuitBreaker_OpenErrorIsErrCircuitOpen confirms that
// the error returned when ElevenLabs's breaker rejects a call satisfies
// errors.Is against the exported ErrCircuitOpen sentinel, even though
// SynthesizeStream wraps it with vendor-specific context (see
// elevenlabs.go).
func TestElevenLabsCircuitBreaker_OpenErrorIsErrCircuitOpen(t *testing.T) {
	var failing int32 = 1
	srv := newToggleElevenLabsServer(t, &failing, []byte{1, 2, 3, 4})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	const threshold = 1
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(threshold, 10*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}

	_, err = e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error while the breaker is open, got nil")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("errors.Is(err, ErrCircuitOpen) = false for err = %v, want true", err)
	}
}

func TestElevenLabsCircuitBreaker_ProbeAfterCooldownSucceeds(t *testing.T) {
	var failing int32 = 1
	pcm := []byte{9, 9, 9, 9}
	srv := newToggleElevenLabsServer(t, &failing, pcm)
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	const cooldown = 40 * time.Millisecond
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(1, cooldown))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}
	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an immediate failure while breaker is open, got nil")
	}

	time.Sleep(cooldown + 20*time.Millisecond)
	atomic.StoreInt32(&failing, 0)

	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("expected the post-cooldown probe to succeed, got error: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)

	atomic.StoreInt32(&failing, 1)
	start := time.Now()
	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an error from persistent 500s, got nil")
	}
	elapsed := time.Since(start)
	if elapsed < elevenlabsRetryBaseDelay {
		t.Errorf("elapsed = %v, expected at least one retry backoff delay (%v), suggesting the breaker did not retry normally", elapsed, elevenlabsRetryBaseDelay)
	}
}

func TestElevenLabsCircuitBreaker_FailedProbeReopensCooldown(t *testing.T) {
	var failing int32 = 1
	srv := newToggleElevenLabsServer(t, &failing, []byte{1, 2, 3, 4})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	const cooldown = 30 * time.Millisecond
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(1, cooldown))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail, got nil")
	}

	time.Sleep(cooldown + 20*time.Millisecond)
	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the probe call to fail (vendor still down), got nil")
	}

	start := time.Now()
	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an immediate failure after the probe reopened the cooldown, got nil")
	}
	elapsed := time.Since(start)
	if elapsed >= elevenlabsRetryBaseDelay {
		t.Errorf("elapsed = %v after reopened cooldown, want well under one backoff delay (%v)", elapsed, elevenlabsRetryBaseDelay)
	}
}

func TestElevenLabsCircuitBreaker_DoesNotTripOnPermanentErrors(t *testing.T) {
	srv, _ := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_voice_id"}}`))
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(1, 10*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
			t.Fatalf("call %d: expected 400 error, got nil", i)
		}
	}
	if e.breaker.open {
		t.Error("breaker should not open after only permanent (non-retryable) failures")
	}
}

func TestElevenLabsCircuitBreaker_DefaultEnabledWithoutOption(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	if e.breaker == nil {
		t.Fatal("expected a non-nil default circuit breaker")
	}
	if e.breaker.threshold != defaultBreakerFailureThreshold {
		t.Errorf("default threshold = %d, want %d", e.breaker.threshold, defaultBreakerFailureThreshold)
	}
	if e.breaker.cooldown != defaultBreakerCooldown {
		t.Errorf("default cooldown = %v, want %v", e.breaker.cooldown, defaultBreakerCooldown)
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
