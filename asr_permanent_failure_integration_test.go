// Package langstream_test - QA's Sprint 2026-07-20 integration coverage
// for Tech's ASR-permanent-failure leg-visibility fix
// (pkg/langstream/session.go's runLeg `tr, ok := <-transcripts` branch,
// plus pkg/langstream/fallback.go's reasonASRStreamClosed).
//
// Before this fix, a leg whose asr.StreamSession's Transcripts() channel
// closed on its own mid-call (a permanent backend failure, as opposed to
// Session.Close() deliberately closing it) made runLeg silently return:
// the leg's degraded flags never flipped, and any raw audio still
// buffered for the in-flight utterance was dropped on the floor with no
// observability signal at all.
//
// Tech's own unit tests
// (pkg/langstream/session_test.go's
// TestSessionASRStreamPermanentClosureDegradesLegAndForwardsBufferedAudio /
// TestSessionCloseDoesNotTriggerASRStreamClosedFallback) already exercise
// this thoroughly against Tech's own hand-rolled, unexported,
// package-internal fakes (fakeRecognizer/fakeStreamSession). This file
// re-drives the identical trigger - a StreamSession's Transcripts()
// channel closing on its own - through the real, exported
// langstream.Session API from an external test package (langstream_test,
// same convention as langstream_integration_test.go's package doc
// comment), using real translate.MockTranslator/tts.MockSynthesizer
// backends and a small QA-local fake asr.Recognizer/StreamSession pair
// (distinct from Tech's own unexported fakes, which this file cannot
// import from outside package langstream), so the observable,
// cross-package contract is what's actually checked here, not just
// internals only reachable from within package langstream itself.
package langstream_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// permaFailStream is a minimal asr.StreamSession test double. Its
// Transcripts() channel is closed either normally (via Close(), called by
// Session.Close() as part of ordinary shutdown) or by
// simulateBackendDeath (called directly by this file's tests, standing
// in for a vendor backend that has permanently failed mid-call, e.g.
// Deepgram's failAndClose after exhausting maxReconnectAttempts - see
// pkg/asr/backoff.go). Both paths are idempotent and mutually safe to
// call in either order, exactly mirroring asr/mock.go's
// mockStreamSession.Close() idempotency. PushAudio is intentionally a
// no-op: the audio itself is captured by Session's own per-leg raw-audio
// ring buffer (fallback.go's audioRingBuffer), not by this fake.
type permaFailStream struct {
	mu     sync.Mutex
	out    chan asr.Transcript
	closed bool
}

func newPermaFailStream() *permaFailStream {
	return &permaFailStream{out: make(chan asr.Transcript, 4)}
}

func (s *permaFailStream) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *permaFailStream) Transcripts() <-chan asr.Transcript { return s.out }

// Close implements the normal Session.Close()-driven shutdown path.
func (s *permaFailStream) Close() error {
	s.closeChannel()
	return nil
}

// simulateBackendDeath closes the Transcripts() channel exactly the way
// Close() does, but models the ASR backend itself giving up unprompted,
// mid-call - independent of Session.Close(), which this file's tests
// deliberately do not call before invoking this method.
func (s *permaFailStream) simulateBackendDeath() {
	s.closeChannel()
}

func (s *permaFailStream) closeChannel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.out)
	}
}

// permaFailRecognizer is a minimal asr.Recognizer test double that hands
// out permaFailStream instances and records every one it creates, in
// call order, so a test can reach into a specific leg's stream (caller =
// index 0, agent = index 1, matching langstream.NewSession's own
// StartStream call order - see session.go's NewSession) and kill it
// directly.
type permaFailRecognizer struct {
	mu      sync.Mutex
	streams []*permaFailStream
}

func (r *permaFailRecognizer) Name() string { return "perma-fail-fake" }

func (r *permaFailRecognizer) SupportedLanguages() []asr.Language {
	return []asr.Language{"en", "hi"}
}

func (r *permaFailRecognizer) StartStream(ctx context.Context, hint asr.Language) (asr.StreamSession, error) {
	s := newPermaFailStream()
	r.mu.Lock()
	r.streams = append(r.streams, s)
	r.mu.Unlock()
	return s, nil
}

func (r *permaFailRecognizer) stream(i int) *permaFailStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[i]
}

func (r *permaFailRecognizer) streamCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

var _ asr.Recognizer = (*permaFailRecognizer)(nil)
var _ asr.StreamSession = (*permaFailStream)(nil)

// newPermaFailSession builds a real *langstream.Session wired to
// permaFailRecognizer plus real translate.MockTranslator/
// tts.MockSynthesizer backends. None of this file's tests ever reach
// translate/TTS - the ASR stream dies before any transcript ever arrives
// - but real backends are used anyway, per this repo's established
// integration-test convention of preferring real components over fakes
// wherever a fake isn't specifically what's under test (see
// langstream_integration_test.go's package doc comment).
func newPermaFailSession(t *testing.T, ctx context.Context, rec *permaFailRecognizer) *langstream.Session {
	t.Helper()
	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}
	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}

// TestASRPermanentFailure_RealSessionDegradesCallerLegAndForwardsBufferedAudio
// drives Tech's fix through the real, exported langstream.Session API:
// the caller leg's ASR StreamSession dies unprompted (its Transcripts()
// channel closes on its own) after one frame of caller audio has already
// been pushed and buffered, but before any transcript ever arrives.
// Asserts the three externally-observable guarantees this fix adds:
// CallerLegDegraded() becomes true, the buffered audio reaches the agent
// as a final passthrough chunk on AgentHearsAudio() instead of being
// silently dropped, and the agent leg (whose ASR stream never closed) is
// unaffected.
func TestASRPermanentFailure_RealSessionDegradesCallerLegAndForwardsBufferedAudio(t *testing.T) {
	rec := &permaFailRecognizer{}
	sess := newPermaFailSession(t, context.Background(), rec)
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{9, 8, 7, 6, 5}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	if rec.streamCount() < 1 {
		t.Fatal("expected permaFailRecognizer to have started at least the caller stream")
	}
	callerStream := rec.stream(0)

	// Simulate the ASR backend dying permanently, mid-call - independent
	// of Session.Close, which is never called before this point.
	callerStream.simulateBackendDeath()

	var sawOriginal, sawFinal bool
	deadline := time.After(3 * time.Second)
	for !sawFinal {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly before the passthrough chunk arrived")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			sawFinal = chunk.IsFinal
		case <-deadline:
			t.Fatal("timed out waiting for the buffered audio to be forwarded as passthrough after the ASR stream closed")
		}
	}
	if !sawOriginal {
		t.Fatal("expected the buffered original caller audio to be forwarded as a passthrough chunk, but it never appeared on AgentHearsAudio()")
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("expected CallerLegDegraded() to be true after its ASR stream closed unprompted")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("the agent leg's ASR stream never closed; AgentLegDegraded() must remain false")
	}

	// Close must still shut the session down cleanly: the caller leg's
	// goroutine already exited on its own (proven by the passthrough
	// chunk above), and the agent leg must still exit normally when
	// Close closes its still-live ASR stream - no hang, no panic.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
	if _, ok := <-sess.AgentHearsAudio(); ok {
		t.Fatal("AgentHearsAudio not closed after Close")
	}
	if _, ok := <-sess.CallerHearsAudio(); ok {
		t.Fatal("CallerHearsAudio not closed after Close")
	}
}

// TestASRPermanentFailure_AgentLegSymmetric is the caller-leg test's
// mirror: the agent leg's ASR stream dies unprompted instead, proving the
// fix applies symmetrically (session.go's runLeg is parameterized
// identically for both legs) and that it is CallerHearsAudio()
// specifically (not AgentHearsAudio(), which must stay unaffected) that
// carries the resulting passthrough chunk.
func TestASRPermanentFailure_AgentLegSymmetric(t *testing.T) {
	rec := &permaFailRecognizer{}
	sess := newPermaFailSession(t, context.Background(), rec)
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{1, 2, 3, 4}, SampleRate: 8000}
	if err := sess.PushAgentAudio(frame); err != nil {
		t.Fatalf("PushAgentAudio: %v", err)
	}

	if rec.streamCount() < 2 {
		t.Fatal("expected permaFailRecognizer to have started both caller and agent streams")
	}
	agentStream := rec.stream(1)
	agentStream.simulateBackendDeath()

	var sawOriginal, sawFinal bool
	deadline := time.After(3 * time.Second)
	for !sawFinal {
		select {
		case chunk, ok := <-sess.CallerHearsAudio():
			if !ok {
				t.Fatal("CallerHearsAudio closed unexpectedly before the passthrough chunk arrived")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			sawFinal = chunk.IsFinal
		case <-deadline:
			t.Fatal("timed out waiting for passthrough audio on CallerHearsAudio() after the agent ASR stream closed")
		}
	}
	if !sawOriginal {
		t.Fatal("expected the buffered original agent audio to be forwarded as a passthrough chunk on CallerHearsAudio(), but it never appeared")
	}

	if !sess.AgentLegDegraded() {
		t.Fatal("expected AgentLegDegraded() to be true after the agent leg's ASR stream closed unprompted")
	}
	if sess.CallerLegDegraded() {
		t.Fatal("the caller leg's ASR stream never closed; CallerLegDegraded() must remain false")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
}

// TestASRPermanentFailure_NoGoroutineLeak drives the same caller-leg
// permanent-failure trigger repeatedly and confirms no goroutine is ever
// leaked: each iteration's caller-leg goroutine must exit on its own (via
// the fixed runLeg branch) well before Close() runs, so Close() only
// ever has to shut down the still-healthy agent leg - exactly the shape
// that would leak a goroutine if the fixed runLeg branch ever forgot to
// return.
func TestASRPermanentFailure_NoGoroutineLeak(t *testing.T) {
	settleGoroutines(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		rec := &permaFailRecognizer{}
		sess := newPermaFailSession(t, context.Background(), rec)

		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: []byte{1, 2, 3}, SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio: %v", err)
		}

		rec.stream(0).simulateBackendDeath()

		// Drain until the final passthrough chunk (or channel close) so
		// the leg goroutine has fully finished its post-death work
		// before Close() runs.
		for {
			chunk, ok := <-sess.AgentHearsAudio()
			if !ok || chunk.IsFinal {
				break
			}
		}

		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	settleGoroutines(t)
	after := runtime.NumGoroutine()

	if after > before+4 {
		t.Fatalf("possible goroutine leak after repeated ASR-permanent-failure cycles: before=%d after=%d", before, after)
	}
}
