// Package langstream_test is QA's first real cross-workstream integration
// test: it wires the *actual* backends PE built (asr.MockRecognizer,
// translate.MockTranslator, tts.MockSynthesizer) into the *actual*
// orchestrator Tech built (langstream.Session), rather than the
// hand-rolled fakes each package's own unit tests use. Those per-package
// unit tests (pkg/langstream/session_test.go in particular) are correct
// and valuable, but they can't catch a bug that only manifests when the
// real components are combined - which is exactly what happened here (see
// TestSessionClose_FlushesFinalUtteranceOnHangup below, which found and
// now guards a real Day-1 bug in Session.Close()'s shutdown ordering).
//
// This file lives at the repo root (not inside any pkg/ directory) and
// uses an external test package (langstream_test) importing everything by
// its module path, per QA's charter in references/workstreams.md: QA owns
// tests across all packages without owning the packages themselves.
package langstream_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// newRealMockSession builds a langstream.Session out of PE's real mock
// backends (not fakes) for the pilot's one supported language pair,
// hi (caller) <-> en (agent).
func newRealMockSession(t *testing.T, ctx context.Context) *langstream.Session {
	t.Helper()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asr.NewMockRecognizer("hi", "en"),
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession with real mock backends: %v", err)
	}
	return sess
}

// TestPipelineInteroperability_ASRTranslateTTS proves, independent of the
// Session orchestrator, that PE's three mock backends genuinely
// interoperate: a transcript produced by asr.MockRecognizer feeds cleanly
// into translate.MockTranslator, whose output feeds cleanly into
// tts.MockSynthesizer, in both directions of the pilot's hi<->en pair.
// This is the "do the independently-built pieces fit together" check at
// the component level, decoupled from Session so it isn't affected by the
// Session-level bug documented in TestSessionClose_DropsFinalUtteranceOnHangup.
func TestPipelineInteroperability_ASRTranslateTTS(t *testing.T) {
	cases := []struct {
		name       string
		speakLang  asr.Language
		targetLang translate.Language
	}{
		{"hindi caller heard in english", "hi", "en"},
		{"english agent heard in hindi", "en", "hi"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			recognizer := asr.NewMockRecognizer("hi", "en")
			stream, err := recognizer.StartStream(ctx, tc.speakLang)
			if err != nil {
				t.Fatalf("StartStream(%q): %v", tc.speakLang, err)
			}

			// Push a frame smaller than the mock's 8000-byte auto-flush
			// threshold, so the only transcript this stream ever emits is
			// the final one flushed by Close (below), exactly the path a
			// short single-utterance call takes before hangup.
			if err := stream.PushAudio(ctx, asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000}); err != nil {
				t.Fatalf("PushAudio: %v", err)
			}

			// Closing the *raw* ASR stream directly (not through a
			// langstream.Session) is deliberate here: it isolates
			// asr.MockRecognizer's documented "final transcript on
			// Close()" contract from Session's shutdown-ordering bug, so
			// this test exercises only the ASR->MT->TTS handoff.
			go func() { _ = stream.Close() }()

			var tr asr.Transcript
			select {
			case got, ok := <-stream.Transcripts():
				if !ok {
					t.Fatal("Transcripts() closed with no final transcript")
				}
				tr = got
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for final transcript")
			}
			if !tr.IsFinal {
				t.Fatalf("expected the flushed transcript to be final, got %+v", tr)
			}

			translator := translate.NewMockTranslator()
			source := translate.Language(tc.speakLang)
			chunk, err := translator.Translate(ctx, tr.Text, source, tc.targetLang, tr.IsFinal)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			wantPrefix := fmt.Sprintf("[%s] ", upper(string(tc.targetLang)))
			if len(chunk.Text) < len(wantPrefix) || chunk.Text[:len(wantPrefix)] != wantPrefix {
				t.Fatalf("translated text = %q, want prefix %q", chunk.Text, wantPrefix)
			}
			if chunk.Text[len(wantPrefix):] != tr.Text {
				t.Fatalf("translated text dropped/altered original: got %q, want original %q preserved after prefix", chunk.Text, tr.Text)
			}

			synth := tts.NewMockSynthesizer("hi", "en")
			audioCh, err := synth.SynthesizeStream(ctx, chunk.Text, tts.Persona{Language: tts.Language(tc.targetLang)})
			if err != nil {
				t.Fatalf("SynthesizeStream: %v", err)
			}

			var chunks int
			var sawFinal bool
			for c := range audioCh {
				chunks++
				if len(c.PCM) == 0 {
					t.Error("synthesized chunk has empty PCM")
				}
				if c.IsFinal {
					sawFinal = true
				}
			}
			if chunks == 0 {
				t.Fatal("SynthesizeStream produced no audio chunks")
			}
			if !sawFinal {
				t.Error("SynthesizeStream never emitted an IsFinal=true chunk")
			}
		})
	}
}

// upper is a tiny local helper so this file doesn't need to import
// strings just for ToUpper on an ASCII language tag.
func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// TestSessionLifecycle_RealMockBackends drives a real langstream.Session
// (Tech's orchestrator) with PE's real mock backends through a full
// construct -> push both legs -> close lifecycle, and asserts the
// invariants that must hold for *any* backend combination: PushCallerAudio
// / PushAgentAudio are accepted without error, sub-flush-threshold audio
// produces no premature output (Week 1 scope: only final transcripts are
// translated - see pkg/langstream/session.go runLeg), and Close() shuts
// both outbound channels down cleanly.
//
// It deliberately does NOT assert that translated audio arrives on
// AgentHearsAudio()/CallerHearsAudio() here: both frames pushed below are
// sub-flush-threshold, so no final transcript exists yet when Close() runs.
// TestSessionClose_FlushesFinalUtteranceOnHangup below covers the
// Close()-time-flush path specifically.
func TestSessionLifecycle_RealMockBackends(t *testing.T) {
	ctx := context.Background()
	sess := newRealMockSession(t, ctx)

	callerFrame := asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000} // hi, sub-flush-threshold
	agentFrame := asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000}  // en, sub-flush-threshold

	if err := sess.PushCallerAudio(callerFrame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}
	if err := sess.PushAgentAudio(agentFrame); err != nil {
		t.Fatalf("PushAgentAudio: %v", err)
	}

	// Neither leg has produced a final transcript yet (both are still
	// below the mock ASR's flush threshold), so nothing should be
	// waiting on either outbound channel.
	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		t.Fatalf("unexpected audio on AgentHearsAudio before any final transcript: chunk=%+v ok=%v", chunk, ok)
	case chunk, ok := <-sess.CallerHearsAudio():
		t.Fatalf("unexpected audio on CallerHearsAudio before any final transcript: chunk=%+v ok=%v", chunk, ok)
	case <-time.After(150 * time.Millisecond):
		// expected: quiet pipeline
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both pushes were non-zero (3000 bytes), so Close() now correctly
	// flushes a final transcript for each leg (see
	// TestSessionClose_FlushesFinalUtteranceOnHangup) - the channels may
	// therefore carry that flushed audio before closing. Drain fully
	// rather than doing a single non-blocking-style receive, and assert
	// only the invariant this test actually cares about: both channels
	// eventually close so a reader never blocks forever.
	for range sess.AgentHearsAudio() {
	}
	for range sess.CallerHearsAudio() {
	}

	// Close must be idempotent even across the real backends.
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

// TestSessionClose_FlushesFinalUtteranceOnHangup guards against a real bug
// found on Day 1 while wiring PE's and Tech's work together for the first
// time, and fixed the same day (see pkg/langstream/session.go's Close()
// doc comment for the full ordering explanation):
//
// Session.Close() used to cancel the session context *before* closing the
// ASR streams. Because asr.MockRecognizer's stream derives its internal
// context from the Session's own context, that early cancel raced the
// stream's Close()-time flush of its final buffered transcript (exactly
// what a real streaming ASR vendor does with a trailing utterance at
// end-of-call) against a context that was often already done by the time
// the flush tried to send - silently discarding the last thing said before
// hangup, every time. Close() now closes the ASR streams first and only
// cancels the context afterward (or as a bounded backstop if a backend
// never closes its Transcripts() channel), so the flushed transcript rides
// the still-live pipeline through translation and synthesis before
// anything is torn down.
//
// This test used to assert the drop (0 chunks received); it now asserts
// the fix (delivered, translated, on AgentHearsAudio()), so a regression
// back to the old ordering fails this test immediately instead of silently
// reintroducing dropped last-words-before-hangup in production.
func TestSessionClose_FlushesFinalUtteranceOnHangup(t *testing.T) {
	ctx := context.Background()
	sess := newRealMockSession(t, ctx)

	// Leave a non-zero remainder below the mock's flush threshold so
	// asr.MockRecognizer.Close() has something to flush as a final
	// transcript (see pkg/asr/mock.go: "if s.buffered > 0 || s.seq == 0").
	frame := asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	var received int
	var sawFinal bool
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for chunk := range sess.AgentHearsAudio() {
			received++
			if chunk.IsFinal {
				sawFinal = true
			}
		}
	}()

	// Give the translation/synthesis pipeline a moment to process the
	// buffered (not-yet-flushed) audio before hanging up, so this test
	// exercises the Close()-time flush path specifically, not the
	// periodic mid-call flush path.
	time.Sleep(100 * time.Millisecond)

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-drainDone

	if received == 0 {
		t.Fatal("expected the final utterance flushed by ASR Close() to be translated, synthesized, and delivered on AgentHearsAudio(), but received 0 chunks - the Day-1 shutdown-ordering bug may have regressed")
	}
	if !sawFinal {
		t.Error("expected at least one delivered chunk to have IsFinal=true")
	}
}

// TestSessionDoesNotLeakGoroutines_RealMockBackends is Tech's own
// goroutine-leak regression pattern (pkg/langstream/session_test.go),
// re-run here against the real mock backends instead of Tech's hand-rolled
// fakes, since a leak could in principle depend on how the concrete ASR/
// TTS backend behaves (e.g. whether it launches its own goroutines, as
// tts.MockSynthesizer's SynthesizeStream does).
func TestSessionDoesNotLeakGoroutines_RealMockBackends(t *testing.T) {
	settleGoroutines(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 25; i++ {
		ctx := context.Background()
		sess := newRealMockSession(t, ctx)

		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio: %v", err)
		}
		if err := sess.PushAgentAudio(asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000}); err != nil {
			t.Fatalf("PushAgentAudio: %v", err)
		}
		// A couple of extra pushes past the mock ASR's flush threshold so
		// the periodic non-final-transcript path (and its goroutines/
		// timers, if any) is exercised too, not just the Close()-time
		// flush path.
		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: make([]byte, 8000), SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio (flush-triggering): %v", err)
		}

		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	settleGoroutines(t)
	after := runtime.NumGoroutine()

	if after > before+4 {
		t.Fatalf("possible goroutine leak with real mock backends: before=%d after=%d", before, after)
	}
}

// settleGoroutines mirrors pkg/langstream/session_test.go's helper of the
// same name: it gives background goroutines a chance to actually exit
// before the test samples runtime.NumGoroutine(), since Close()'s
// wg.Wait() only guarantees the goroutine functions have returned, not
// that the scheduler has reflected that in NumGoroutine() yet.
func settleGoroutines(t *testing.T) {
	t.Helper()
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestObservabilityRecordsRealSessionTiming is a light smoke test proving
// pkg/observability.LatencyRecorder (SRE/Tech's instrumentation stub) can
// be pointed at a real Session's construction/teardown cost end-to-end -
// the same pattern tools/latency_benchmark/main.go uses, just asserted as
// a unit test here so a regression in either package's wiring is caught
// by `go test ./...` and not only by manually running the benchmark tool.
func TestObservabilityRecordsRealSessionTiming(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		start := time.Now()
		sess := newRealMockSession(t, ctx)
		rec.Record("session_setup_ms", float64(time.Since(start).Microseconds())/1000)

		closeStart := time.Now()
		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		rec.Record("session_close_ms", float64(time.Since(closeStart).Microseconds())/1000)
	}

	if got := rec.Count("session_setup_ms"); got != 10 {
		t.Fatalf("session_setup_ms sample count = %d, want 10", got)
	}
	if got := rec.Count("session_close_ms"); got != 10 {
		t.Fatalf("session_close_ms sample count = %d, want 10", got)
	}
	if p50 := rec.Percentile("session_setup_ms", 50); p50 < 0 {
		t.Fatalf("session_setup_ms p50 = %v, want >= 0", p50)
	}
}
