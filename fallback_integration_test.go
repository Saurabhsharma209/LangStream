// Package langstream_test - QA's Sprint 3 integration coverage for Tech's
// fallback/degrade-gracefully work (pkg/langstream/fallback.go,
// pkg/langstream/session.go).
//
// Tech's and SRE's own unit tests (pkg/langstream/fallback_test.go,
// pkg/langstream/session_test.go) already exercise this behavior
// thoroughly, but - per this repo's established QA pattern (see
// langstream_integration_test.go's package doc comment and DEVLOG.md
// 2026-07-07/08) - they do it with hand-rolled fakes local to that
// package's own tests. This file re-drives the same fallback triggers
// through a *real* langstream.Session wired to the *actual* backends other
// workstreams built (asr.MockRecognizer, translate.MockTranslator,
// tts.MockSynthesizer, exactly as cmd/langstream/main.go's runDemo wires
// them via the "mock" backend), to catch anything that only manifests when
// real components are combined.
//
// One real-mock limitation forced two small local test doubles below
// (confidenceOverrideRecognizer/scriptedRecognizer): asr.MockRecognizer
// (pkg/asr/mock.go) only ever emits exactly one IsFinal=true transcript
// per stream, at Close() time (see its "if s.buffered > 0 || s.seq == 0"
// flush condition) - it cannot script confidence values or multiple
// discrete utterances within one call, which the low-confidence and
// repeated-consecutive-failure scenarios below need. Both doubles delegate
// everything else (language handling, canned phrases, PushAudio/Close
// semantics for the confidence case) to real backends where possible, and
// are scoped as tightly as each scenario requires. This is not filed as a
// bug: pkg/asr/mock.go is pre-existing Week 1/2 PE-workstream code this
// sprint's Tech/SRE changes did not touch, and single-utterance-per-call
// is a reasonable scope for a deterministic canned-phrase mock.
package langstream_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// --- test doubles ---

// confidenceOverrideStream wraps a real asr.StreamSession, rewriting the
// Confidence field of every transcript it relays. asr.MockRecognizer
// always reports Confidence: 0.99 (see pkg/asr/mock.go's buildTranscript),
// so this is the only way to drive FallbackConfig.ConfidenceThreshold
// through the real mock backend rather than a hand-rolled ASR fake.
type confidenceOverrideStream struct {
	inner      asr.StreamSession
	confidence float64
	out        chan asr.Transcript
}

func wrapWithConfidence(inner asr.StreamSession, confidence float64) *confidenceOverrideStream {
	s := &confidenceOverrideStream{inner: inner, confidence: confidence, out: make(chan asr.Transcript, 8)}
	go func() {
		defer close(s.out)
		for tr := range inner.Transcripts() {
			tr.Confidence = confidence
			s.out <- tr
		}
	}()
	return s
}

func (s *confidenceOverrideStream) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	return s.inner.PushAudio(ctx, frame)
}
func (s *confidenceOverrideStream) Transcripts() <-chan asr.Transcript { return s.out }
func (s *confidenceOverrideStream) Close() error                       { return s.inner.Close() }

// confidenceOverrideRecognizer wraps a real asr.Recognizer (the actual
// asr.MockRecognizer in every test below), delegating Name/
// SupportedLanguages/StartStream's real behavior entirely except for
// rewriting the confidence of every transcript the resulting stream
// produces.
type confidenceOverrideRecognizer struct {
	inner      asr.Recognizer
	confidence float64

	mu    sync.Mutex
	calls int
}

func (r *confidenceOverrideRecognizer) Name() string { return r.inner.Name() }
func (r *confidenceOverrideRecognizer) SupportedLanguages() []asr.Language {
	return r.inner.SupportedLanguages()
}

// StartStream overrides confidence only for the *first* StartStream call
// (the caller leg - see session.go's NewSession, which always starts the
// caller ASR stream before the agent one). This keeps the override
// isolated to the leg under test: asr.MockRecognizer's mockStreamSession
// flushes a final transcript on Close() even for a leg that never received
// any PushAudio at all (see its "s.buffered > 0 || s.seq == 0" condition),
// so overriding indiscriminately for every stream would also force a
// second, unrelated low-confidence event out of the untouched agent leg.
func (r *confidenceOverrideRecognizer) StartStream(ctx context.Context, hint asr.Language) (asr.StreamSession, error) {
	s, err := r.inner.StartStream(ctx, hint)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	n := r.calls
	r.calls++
	r.mu.Unlock()
	if n != 0 {
		return s, nil
	}
	return wrapWithConfidence(s, r.confidence), nil
}

// scriptedStream/scriptedRecognizer emit a fixed sequence of *final*
// transcripts, one per PushAudio call, so a test can drive several
// consecutive utterances through one leg - something the real
// asr.MockRecognizer cannot do (see the package doc comment above).
type scriptedStream struct {
	mu     sync.Mutex
	out    chan asr.Transcript
	script []asr.Transcript
	pos    int
	closed bool
}

func newScriptedStream(script []asr.Transcript) *scriptedStream {
	return &scriptedStream{out: make(chan asr.Transcript, 16), script: script}
}

func (s *scriptedStream) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("scripted: session closed")
	}
	var next *asr.Transcript
	if s.pos < len(s.script) {
		t := s.script[s.pos]
		next = &t
		s.pos++
	}
	s.mu.Unlock()

	if next == nil {
		return nil
	}
	select {
	case s.out <- *next:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *scriptedStream) Transcripts() <-chan asr.Transcript { return s.out }

func (s *scriptedStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.out)
	return nil
}

// scriptedRecognizer hands out one scriptedStream per StartStream call, in
// order, driven by scripts[i] for the i-th call (NewSession always calls
// StartStream exactly twice: caller leg then agent leg).
type scriptedRecognizer struct {
	mu       sync.Mutex
	scripts  [][]asr.Transcript
	sessions []*scriptedStream
}

func (r *scriptedRecognizer) Name() string                       { return "scripted" }
func (r *scriptedRecognizer) SupportedLanguages() []asr.Language { return []asr.Language{"hi", "en"} }
func (r *scriptedRecognizer) StartStream(ctx context.Context, hint asr.Language) (asr.StreamSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.sessions)
	var script []asr.Transcript
	if idx < len(r.scripts) {
		script = r.scripts[idx]
	}
	s := newScriptedStream(script)
	r.sessions = append(r.sessions, s)
	return s, nil
}

// countingTranslator wraps a real translate.Translator (always the actual
// translate.MockTranslator below), counting calls and optionally forcing
// specific call numbers (1-indexed) to fail, so tests can assert exactly
// how many times the real Translator was invoked -- e.g. to prove a
// permanently-degraded leg stops calling it at all, or that low ASR
// confidence never reaches it in the first place.
type countingTranslator struct {
	mu      sync.Mutex
	inner   translate.Translator
	calls   int
	sources []translate.Language // source language passed to each call, in order
	failOn  map[int]error        // 1-indexed call number -> error to return instead of delegating
}

func (t *countingTranslator) Name() string { return t.inner.Name() }
func (t *countingTranslator) SupportedPairs() [][2]translate.Language {
	return t.inner.SupportedPairs()
}

func (t *countingTranslator) Translate(ctx context.Context, text string, source, target translate.Language, isFinal bool) (translate.Chunk, error) {
	t.mu.Lock()
	t.calls++
	n := t.calls
	t.sources = append(t.sources, source)
	var forced error
	if t.failOn != nil {
		forced = t.failOn[n]
	}
	t.mu.Unlock()

	if forced != nil {
		return translate.Chunk{}, forced
	}
	return t.inner.Translate(ctx, text, source, target, isFinal)
}

func (t *countingTranslator) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// callCountFromSource returns how many Translate calls were made with the
// given source language, so a test asserting "leg X's transcript never
// reached Translate" isn't confused by an unrelated leg (sharing the same
// Translator, per SessionConfig) legitimately calling it.
func (t *countingTranslator) callCountFromSource(lang translate.Language) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, s := range t.sources {
		if s == lang {
			n++
		}
	}
	return n
}

// fatalTestError implements langstream.FatalError (structurally: Error()
// string + Fatal() bool) so tests can force the immediate-permanent-degrade
// path independent of MaxConsecutiveFailures.
type fatalTestError struct{ msg string }

func (e fatalTestError) Error() string { return e.msg }
func (e fatalTestError) Fatal() bool   { return true }

// drainUntilFinal reads from ch until it observes a chunk with IsFinal
// true (returning all chunks seen, including the final one) or the
// deadline elapses / the channel closes early, whichever happens first.
// Shared by every scenario below since each one cares about "did the
// listening party eventually hear a complete (possibly passthrough)
// utterance", not exact chunk-by-chunk timing.
func drainUntilFinal(t *testing.T, ch <-chan tts.AudioChunk, deadline time.Duration) []tts.AudioChunk {
	t.Helper()
	var chunks []tts.AudioChunk
	timeout := time.After(deadline)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("audio channel closed unexpectedly before a final chunk arrived (chunks so far: %d)", len(chunks))
			}
			chunks = append(chunks, c)
			if c.IsFinal {
				return chunks
			}
		case <-timeout:
			t.Fatalf("timed out waiting for a final audio chunk (chunks so far: %d) - session may be hanging", len(chunks))
		}
	}
}

func containsPCM(chunks []tts.AudioChunk, want []byte) bool {
	for _, c := range chunks {
		if string(c.PCM) == string(want) {
			return true
		}
	}
	return false
}

// --- (1a) low ASR confidence -> passthrough, translator never called ---

func TestFallbackIntegration_LowASRConfidencePassesThroughOriginalAudio(t *testing.T) {
	ctx := context.Background()

	realASR := asr.NewMockRecognizer("hi", "en")
	translator := &countingTranslator{inner: translate.NewMockTranslator()}

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            &confidenceOverrideRecognizer{inner: realASR, confidence: 0.1}, // below default 0.55 threshold
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	frame := asr.AudioFrame{PCM: make([]byte, 3000), SampleRate: 8000} // below the mock's 8000-byte flush threshold
	for i := range frame.PCM {
		frame.PCM[i] = byte(i % 251) // non-zero, distinctive content so it can't be confused with a zeroed tone/marker chunk
	}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	// Close() flushes exactly one final transcript for this leg (see the
	// package doc comment), which is what carries the forced low
	// confidence through runLeg.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var chunks []tts.AudioChunk
	for c := range sess.AgentHearsAudio() {
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		t.Fatal("expected passthrough audio on AgentHearsAudio after a low-confidence final transcript, got none")
	}
	if !containsPCM(chunks, frame.PCM) {
		t.Fatalf("expected the original caller audio among the passthrough chunks; got %d chunks, none matching", len(chunks))
	}
	// The agent leg (independent of the caller leg under test here, but
	// sharing the same Translator per SessionConfig) also flushes its own
	// default-confidence final transcript on Close and legitimately calls
	// Translate for it (source "en") - see confidenceOverrideRecognizer's
	// doc comment above. What matters for *this* test is that the
	// caller leg's low-confidence transcript (source "hi") never reached
	// Translate at all.
	if got := translator.callCountFromSource("hi"); got != 0 {
		t.Fatalf("translator called %d times with source=hi, want 0 (the caller leg's low-confidence transcript must never reach Translate)", got)
	}
	if sess.CallerLegDegraded() {
		t.Fatal("a low-confidence utterance must not permanently degrade the leg (see runLeg: the confidence branch never touches leg.recordFailure) - only MT/TTS failures and FatalError do")
	}

	metrics := sess.Metrics()
	if got := metrics.ErrorCount("asr_confidence", "mock"); got != 1 {
		t.Fatalf("Metrics().ErrorCount(asr_confidence, mock) = %d, want 1", got)
	}
	// EventCount is 2, not 1: it's RecordError + RecordEvent calls combined
	// (see LatencyRecorder.EventCount's doc comment), and the agent leg's
	// own default-confidence flush (see the comment above) contributes one
	// *successful* asr_confidence event for the same (stage, vendor) key,
	// on top of the caller leg's one low-confidence error event.
	if got := metrics.EventCount("asr_confidence", "mock"); got != 2 {
		t.Fatalf("Metrics().EventCount(asr_confidence, mock) = %d, want 2 (1 low-confidence error from the caller leg + 1 success from the agent leg's own flush)", got)
	}
}

// --- (1b) fatal MT error -> immediate permanent degrade, passthrough,
// no panic/hang on subsequent audio, and no un-degrading ---

func TestFallbackIntegration_FatalTranslateErrorDegradesLegImmediately(t *testing.T) {
	ctx := context.Background()

	callerScript := []asr.Transcript{
		{Text: "first utterance", Language: "hi", IsFinal: true, Confidence: 0.99},
		{Text: "second utterance", Language: "hi", IsFinal: true, Confidence: 0.99},
	}
	asrRec := &scriptedRecognizer{scripts: [][]asr.Transcript{callerScript, nil}}

	translator := &countingTranslator{
		inner:  translate.NewMockTranslator(),
		failOn: map[int]error{1: fatalTestError{msg: "translate/test: simulated fatal vendor error"}},
	}

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asrRec,
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame1 := asr.AudioFrame{PCM: []byte{1, 2, 3, 4, 5}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame1); err != nil {
		t.Fatalf("PushCallerAudio #1: %v", err)
	}
	chunks1 := drainUntilFinal(t, sess.AgentHearsAudio(), 2*time.Second)
	if !containsPCM(chunks1, frame1.PCM) {
		t.Fatal("expected the original audio for utterance #1 among the passthrough chunks after a fatal translate error")
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("a single FatalError from Translate must degrade the leg immediately, regardless of MaxConsecutiveFailures")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("the agent leg is independent and must not be affected by the caller leg's fatal error")
	}
	if got := translator.callCount(); got != 1 {
		t.Fatalf("translator called %d times after utterance #1, want 1", got)
	}

	// Subsequent audio on an already-permanently-degraded leg: runLeg must
	// short-circuit straight to passthrough (leg.isDegraded() check) and
	// never call Translate again, never panic, never hang.
	frame2 := asr.AudioFrame{PCM: []byte{9, 8, 7, 6, 5}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame2); err != nil {
		t.Fatalf("PushCallerAudio #2: %v", err)
	}
	chunks2 := drainUntilFinal(t, sess.AgentHearsAudio(), 2*time.Second)
	if !containsPCM(chunks2, frame2.PCM) {
		t.Fatal("expected the original audio for utterance #2 among the passthrough chunks on an already-degraded leg")
	}
	if got := translator.callCount(); got != 1 {
		t.Fatalf("translator called %d times after utterance #2, want still 1 (a degraded leg must not call Translate again)", got)
	}
	if !sess.CallerLegDegraded() {
		t.Fatal("leg must remain degraded (must never un-degrade) across subsequent utterances")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- (1c) repeated non-fatal MT failures -> permanent degrade after
// MaxConsecutiveFailures, and it sticks ---

func TestFallbackIntegration_RepeatedNonFatalFailuresPermanentlyDegradeLeg(t *testing.T) {
	ctx := context.Background()

	const attempts = 5 // > default MaxConsecutiveFailures (3)
	// One extra scripted utterance beyond attempts: the loop below drives
	// exactly `attempts` utterances, and a further utterance afterward
	// proves degradation sticks past that point too.
	script := make([]asr.Transcript, attempts+1)
	for i := range script {
		script[i] = asr.Transcript{Text: fmt.Sprintf("utterance %d", i), Language: "hi", IsFinal: true, Confidence: 0.99}
	}
	asrRec := &scriptedRecognizer{scripts: [][]asr.Transcript{script, nil}}

	// translate.MockTranslator's real, built-in "unsupported pair" error
	// path (a plain, non-FatalError) drives the non-fatal-failure case,
	// so this scenario needs no injected/forced error at all - only the
	// real mock translator, deliberately configured for a pair that
	// doesn't include hi->en.
	realTranslator := translate.NewMockTranslator([2]translate.Language{"xx", "yy"})
	translator := &countingTranslator{inner: realTranslator}

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asrRec,
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	for i := 0; i < attempts; i++ {
		frame := asr.AudioFrame{PCM: []byte{byte(i + 1), byte(i + 1), byte(i + 1)}, SampleRate: 8000}
		if err := sess.PushCallerAudio(frame); err != nil {
			t.Fatalf("PushCallerAudio #%d: %v", i, err)
		}
		chunks := drainUntilFinal(t, sess.AgentHearsAudio(), 2*time.Second)
		if !containsPCM(chunks, frame.PCM) {
			t.Fatalf("attempt %d: expected original audio among passthrough chunks (real mock translator's unsupported-pair error must fall back, not drop)", i)
		}
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("expected the caller leg to be permanently degraded after 5 consecutive unsupported-pair failures (default MaxConsecutiveFailures is 3)")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("the agent leg must be unaffected by the caller leg's failures")
	}
	// Calls 1-3 must reach the real Translator; once degraded (after the
	// 3rd), later attempts must be short-circuited straight to passthrough
	// without calling Translate again.
	if got := translator.callCount(); got != 3 {
		t.Fatalf("translator called %d times, want exactly 3 (degrade after the 3rd failure, no further calls for attempts 4-5)", got)
	}

	// One more utterance after the loop, purely to prove degradation
	// sticks and nothing panics/hangs well past the threshold.
	frame := asr.AudioFrame{PCM: []byte{42, 42, 42}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio (post-degrade): %v", err)
	}
	chunks := drainUntilFinal(t, sess.AgentHearsAudio(), 2*time.Second)
	if !containsPCM(chunks, frame.PCM) {
		t.Fatal("expected passthrough to keep working well after permanent degradation")
	}
	if got := translator.callCount(); got != 3 {
		t.Fatalf("translator called %d times after the post-degrade utterance, want still 3", got)
	}
	if !sess.CallerLegDegraded() {
		t.Fatal("leg must still be degraded (must never un-degrade)")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
