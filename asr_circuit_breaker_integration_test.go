// Package langstream_test - QA's Sprint 2026-07-20 integration coverage
// for PE's ASR circuit breaker (pkg/asr/circuitbreaker.go, wired into
// pkg/asr/deepgram.go's and pkg/asr/sarvam.go's StartStream/
// ensureConnected path), driven from the pkg/langstream.Session
// construction path specifically.
//
// pkg/asr's own unit tests (pkg/asr/deepgram_test.go, pkg/asr/sarvam_test.go,
// and PE's own pkg/asr/circuitbreaker_test.go if/when added) already
// exercise the breaker's trip/cooldown/probe state machine directly and
// thoroughly, entirely within package asr. What they can't exercise -
// they're internal to that package - is the one thing that matters most
// operationally: what happens at langstream.NewSession, which calls
// Recognizer.StartStream directly (see pkg/langstream/session.go's
// NewSession) with no breaker-awareness of its own; NewSession's only
// contract with StartStream is "returns an error, or a live
// StreamSession". This file drives PE's *real* DeepgramRecognizer and
// SarvamRecognizer clients (both landed in time as of this sprint, not a
// local fake - see this file's git history) against small dead
// httptest endpoints - the same "start-then-close-a-server" trick
// pkg/asr/deepgram_test.go's TestDeepgramRecognizer_ConnectFailureSurfacesError
// already uses to get a guaranteed-refused connection with no real vendor
// network dependency - to trip each recognizer's own circuit breaker via
// one genuine failed initial-connect, then constructs a real
// *langstream.Session against the now-tripped recognizer and asserts
// NewSession fails fast (bounded well under the vendor's normal
// dial-and-backoff budget, so a regression that stopped gating on the
// breaker would show up as a slow test, not just a wrong error value)
// with an error wrapping asr.ErrCircuitOpen.
package langstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// deadWebSocketURL starts and immediately closes an httptest.Server,
// returning a ws:// URL nothing is listening on - guaranteeing an
// immediately-refused connection with no real timeout wait, exactly
// mirroring pkg/asr/deepgram_test.go's
// TestDeepgramRecognizer_ConnectFailureSurfacesError.
func deadWebSocketURL(t *testing.T, path string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + path
	srv.Close()
	return u
}

// primeDeepgramCircuitOpen forces exactly one failed initial-connect
// attempt against rec (via a real StartStream + PushAudio round trip
// against a dead endpoint), tripping rec's circuit breaker given
// threshold 1 (see asr.WithCircuitBreaker). Fails the test outright if
// the priming attempt does not actually fail - if it silently succeeded
// (e.g. the dead endpoint stopped being dead), the rest of the test's
// premise (a genuinely open circuit) would be false.
func primeDeepgramCircuitOpen(t *testing.T, rec *asr.DeepgramRecognizer) {
	t.Helper()
	sess, err := rec.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("priming StartStream (breaker should still be closed at this point): %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("priming PushAudio unexpectedly succeeded against a dead endpoint -- can't trip the breaker this way")
	}
}

// primeSarvamCircuitOpen is primeDeepgramCircuitOpen's Sarvam counterpart.
func primeSarvamCircuitOpen(t *testing.T, rec *asr.SarvamRecognizer) {
	t.Helper()
	sess, err := rec.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("priming StartStream (breaker should still be closed at this point): %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("priming PushAudio unexpectedly succeeded against a dead endpoint -- can't trip the breaker this way")
	}
}

// maxNewSessionFailFastDuration bounds how long a NewSession call may
// take while the ASR circuit breaker is open before this test considers
// it a regression. Both pkg/asr/deepgram.go's dgReconnectBase and
// pkg/asr/sarvam.go's sarvamReconnectBase are 250ms, and the default
// maxReconnectAttempts is 3 - a NewSession call that actually attempted
// to dial the dead endpoint (breaker not gating StartStream at all) would
// take at least one dial attempt plus real backoff delay, comfortably
// over this bound. A near-instant return proves the breaker's
// zero-dial-attempts fast path fired, not that a real dial happened to
// fail quickly this time.
const maxNewSessionFailFastDuration = 200 * time.Millisecond

// TestASRCircuitBreakerIntegration_Deepgram_NewSessionFailsFastWhenCircuitOpen
// primes a real DeepgramRecognizer's circuit breaker open (one failed
// initial connect, threshold 1), then confirms langstream.NewSession -
// which calls Recognizer.StartStream directly for the caller leg first -
// fails fast with an error wrapping asr.ErrCircuitOpen instead of
// hanging or timing out through a real dial/backoff cycle.
func TestASRCircuitBreakerIntegration_Deepgram_NewSessionFailsFastWhenCircuitOpen(t *testing.T) {
	t.Setenv("DEEPGRAM_API_KEY", "fake-test-key")

	deadURL := deadWebSocketURL(t, "/v1/listen")
	rec, err := asr.NewDeepgramRecognizer(
		asr.WithBaseURL(deadURL),
		asr.WithMaxReconnectAttempts(1),
		asr.WithCircuitBreaker(1, time.Minute),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	primeDeepgramCircuitOpen(t, rec)

	// Confirm the breaker is actually open now, independent of Session,
	// so any failure below is attributable to NewSession's own
	// StartStream call rather than a priming bug.
	if _, err := rec.StartStream(context.Background(), "en"); !errors.Is(err, asr.ErrCircuitOpen) {
		t.Fatalf("expected the recognizer's own StartStream to report an open circuit right after priming, got: %v", err)
	}

	cfg := langstream.SessionConfig{
		CallerLanguage: "en",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer(),
	}

	start := time.Now()
	sess, err := langstream.NewSession(context.Background(), cfg)
	elapsed := time.Since(start)
	if sess != nil {
		defer sess.Close()
	}

	if err == nil {
		t.Fatal("expected NewSession to fail while the ASR circuit breaker is open, got a live Session instead")
	}
	if !errors.Is(err, asr.ErrCircuitOpen) {
		t.Fatalf("NewSession error = %v, want an error wrapping asr.ErrCircuitOpen", err)
	}
	if elapsed > maxNewSessionFailFastDuration {
		t.Fatalf("NewSession took %v to fail with the circuit open, want under %v -- expected a near-instant fast-fail with zero dial attempts, not a real dial/backoff cycle", elapsed, maxNewSessionFailFastDuration)
	}
}

// TestASRCircuitBreakerIntegration_Sarvam_NewSessionFailsFastWhenCircuitOpen
// is the Deepgram test's exact counterpart against the real
// SarvamRecognizer client, confirming the same fast-fail contract holds
// for both vendors PE wired the breaker into, not just one.
func TestASRCircuitBreakerIntegration_Sarvam_NewSessionFailsFastWhenCircuitOpen(t *testing.T) {
	t.Setenv("SARVAM_API_KEY", "fake-test-key")

	deadURL := deadWebSocketURL(t, "/speech-to-text/ws")
	rec, err := asr.NewSarvamRecognizer(
		asr.WithSarvamBaseURL(deadURL),
		asr.WithSarvamMaxReconnectAttempts(1),
		asr.WithSarvamCircuitBreaker(1, time.Minute),
	)
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}

	primeSarvamCircuitOpen(t, rec)

	if _, err := rec.StartStream(context.Background(), "hi"); !errors.Is(err, asr.ErrCircuitOpen) {
		t.Fatalf("expected the recognizer's own StartStream to report an open circuit right after priming, got: %v", err)
	}

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}

	start := time.Now()
	sess, err := langstream.NewSession(context.Background(), cfg)
	elapsed := time.Since(start)
	if sess != nil {
		defer sess.Close()
	}

	if err == nil {
		t.Fatal("expected NewSession to fail while the ASR circuit breaker is open, got a live Session instead")
	}
	if !errors.Is(err, asr.ErrCircuitOpen) {
		t.Fatalf("NewSession error = %v, want an error wrapping asr.ErrCircuitOpen", err)
	}
	if elapsed > maxNewSessionFailFastDuration {
		t.Fatalf("NewSession took %v to fail with the circuit open, want under %v -- expected a near-instant fast-fail with zero dial attempts, not a real dial/backoff cycle", elapsed, maxNewSessionFailFastDuration)
	}
}
