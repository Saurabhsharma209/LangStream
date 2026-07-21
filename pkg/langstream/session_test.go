package langstream

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// --- Inline fakes: deliberately not the PE workstream's pkg/asr/mock.go,
// pkg/translate/mock.go, pkg/tts/mock.go (concurrently under development
// by another agent), just enough of each interface to drive the
// orchestrator's behavior under test. ---

// fakeStreamSession implements asr.StreamSession. Each PushAudio call pops
// and emits the next entry (if any) from a fixed script, so tests can
// control exactly which transcripts appear and in what order.
type fakeStreamSession struct {
	mu     sync.Mutex
	out    chan asr.Transcript
	script []asr.Transcript
	pos    int
	closed bool
}

func newFakeStreamSession(script []asr.Transcript) *fakeStreamSession {
	return &fakeStreamSession{out: make(chan asr.Transcript, 8), script: script}
}

func (s *fakeStreamSession) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("fake: session closed")
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

func (s *fakeStreamSession) Transcripts() <-chan asr.Transcript { return s.out }

func (s *fakeStreamSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.out)
	return nil
}

// fakeRecognizer implements asr.Recognizer. It records every languageHint
// passed to StartStream and hands out a fakeStreamSession per call, in
// order, driven by an optional list of scripts (scripts[i] for the i-th
// StartStream call).
type fakeRecognizer struct {
	mu       sync.Mutex
	scripts  [][]asr.Transcript
	hints    []asr.Language
	sessions []*fakeStreamSession
}

func (r *fakeRecognizer) Name() string                       { return "fake" }
func (r *fakeRecognizer) SupportedLanguages() []asr.Language { return []asr.Language{"en", "hi"} }

func (r *fakeRecognizer) StartStream(ctx context.Context, languageHint asr.Language) (asr.StreamSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hints = append(r.hints, languageHint)
	var script []asr.Transcript
	idx := len(r.sessions)
	if idx < len(r.scripts) {
		script = r.scripts[idx]
	}
	s := newFakeStreamSession(script)
	r.sessions = append(r.sessions, s)
	return s, nil
}

func (r *fakeRecognizer) hintsSnapshot() []asr.Language {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]asr.Language, len(r.hints))
	copy(out, r.hints)
	return out
}

// fakeTranslator implements translate.Translator, recording every call it
// receives and optionally returning a fixed error instead of a Chunk.
type fakeTranslator struct {
	mu    sync.Mutex
	calls []fakeTranslateCall
	err   error
}

type fakeTranslateCall struct {
	text    string
	source  translate.Language
	target  translate.Language
	isFinal bool
}

func (t *fakeTranslator) Name() string                            { return "fake" }
func (t *fakeTranslator) SupportedPairs() [][2]translate.Language { return nil }

func (t *fakeTranslator) Translate(ctx context.Context, text string, source, target translate.Language, isFinal bool) (translate.Chunk, error) {
	t.mu.Lock()
	t.calls = append(t.calls, fakeTranslateCall{text: text, source: source, target: target, isFinal: isFinal})
	err := t.err
	t.mu.Unlock()
	if err != nil {
		return translate.Chunk{}, err
	}
	return translate.Chunk{Text: "T:" + text, SourceLang: source, TargetLang: target, IsFinal: isFinal}, nil
}

func (t *fakeTranslator) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// fakeSynthesizer implements tts.Synthesizer, synthesizing a single
// AudioChunk whose PCM is just the input text bytes, so tests can assert
// on it directly without decoding real audio.
type fakeSynthesizer struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeSynthesizer) Name() string                       { return "fake" }
func (f *fakeSynthesizer) SupportedLanguages() []tts.Language { return []tts.Language{"en", "hi"} }

func (f *fakeSynthesizer) SynthesizeStream(ctx context.Context, text string, persona tts.Persona) (<-chan tts.AudioChunk, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := make(chan tts.AudioChunk, 1)
	out <- tts.AudioChunk{PCM: []byte(text), SampleRate: 8000, IsFinal: true}
	close(out)
	return out, nil
}

func (f *fakeSynthesizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// slowTranslator ignores the text entirely and blocks until either ctx is
// done or delay elapses, so tests can exercise FallbackConfig.TranslateTimeout
// deterministically without a real network dependency.
type slowTranslator struct {
	delay time.Duration
}

func (t *slowTranslator) Name() string                            { return "slow" }
func (t *slowTranslator) SupportedPairs() [][2]translate.Language { return nil }

func (t *slowTranslator) Translate(ctx context.Context, text string, source, target translate.Language, isFinal bool) (translate.Chunk, error) {
	select {
	case <-time.After(t.delay):
		return translate.Chunk{Text: "T:" + text, SourceLang: source, TargetLang: target, IsFinal: isFinal}, nil
	case <-ctx.Done():
		return translate.Chunk{}, ctx.Err()
	}
}

// stallingSynthesizer returns a channel that never delivers a chunk (until
// its context is cancelled), so tests can exercise
// FallbackConfig.SynthesizeTimeout's "backend never starts responding"
// path deterministically.
type stallingSynthesizer struct{}

func (s *stallingSynthesizer) Name() string                       { return "stalling" }
func (s *stallingSynthesizer) SupportedLanguages() []tts.Language { return []tts.Language{"en", "hi"} }

func (s *stallingSynthesizer) SynthesizeStream(ctx context.Context, text string, persona tts.Persona) (<-chan tts.AudioChunk, error) {
	out := make(chan tts.AudioChunk)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

func validConfig() SessionConfig {
	return SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            &fakeRecognizer{},
		Translator:     &fakeTranslator{},
		TTS:            &fakeSynthesizer{},
	}
}

func TestNewSessionValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*SessionConfig)
		wantErr bool
	}{
		{"valid", func(c *SessionConfig) {}, false},
		{"missing caller language", func(c *SessionConfig) { c.CallerLanguage = "" }, true},
		{"missing agent language", func(c *SessionConfig) { c.AgentLanguage = "" }, true},
		{"nil ASR", func(c *SessionConfig) { c.ASR = nil }, true},
		{"nil Translator", func(c *SessionConfig) { c.Translator = nil }, true},
		{"nil TTS", func(c *SessionConfig) { c.TTS = nil }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)

			sess, err := NewSession(context.Background(), cfg)
			if tc.wantErr {
				if err == nil {
					if sess != nil {
						sess.Close()
					}
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer sess.Close()
		})
	}
}

func TestSessionCodeSwitchingPassesEmptyHints(t *testing.T) {
	rec := &fakeRecognizer{}
	cfg := validConfig()
	cfg.ASR = rec
	cfg.CodeSwitching = true

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	hints := rec.hintsSnapshot()
	if len(hints) != 2 {
		t.Fatalf("expected 2 StartStream calls, got %d", len(hints))
	}
	for i, h := range hints {
		if h != "" {
			t.Errorf("hint %d = %q, want empty (auto-detect) under CodeSwitching", i, h)
		}
	}
}

func TestSessionWithoutCodeSwitchingPassesConfiguredLanguages(t *testing.T) {
	rec := &fakeRecognizer{}
	cfg := validConfig()
	cfg.ASR = rec

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	hints := rec.hintsSnapshot()
	if len(hints) != 2 || hints[0] != "hi" || hints[1] != "en" {
		t.Fatalf("got hints %v, want [hi en]", hints)
	}
}

func TestSessionDuplexFlowSkipsPartialsAndTranslatesFinals(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{ // caller leg (StartStream call #1)
				{Text: "partial caller text", Language: "hi", IsFinal: false},
				{Text: "final caller text", Language: "hi", IsFinal: true},
			},
			{ // agent leg (StartStream call #2)
				{Text: "final agent text", Language: "en", IsFinal: true},
			},
		},
	}
	translator := &fakeTranslator{}
	synth := &fakeSynthesizer{}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator
	cfg.TTS = synth

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}

	// Push twice on the caller leg: first drains the partial (should be
	// skipped, not translated), second drains the final (should be
	// translated and synthesized for the agent to hear).
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio #1: %v", err)
	}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio #2: %v", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			t.Fatalf("AgentHearsAudio closed unexpectedly")
		}
		if string(chunk.PCM) != "T:final caller text" {
			t.Fatalf("agent heard %q, want %q", string(chunk.PCM), "T:final caller text")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent audio")
	}

	if got := translator.callCount(); got != 1 {
		t.Fatalf("translator called %d times, want 1 (partial transcript must be skipped)", got)
	}

	// Now drive the agent leg and confirm audio reaches the caller.
	if err := sess.PushAgentAudio(frame); err != nil {
		t.Fatalf("PushAgentAudio: %v", err)
	}

	select {
	case chunk, ok := <-sess.CallerHearsAudio():
		if !ok {
			t.Fatalf("CallerHearsAudio closed unexpectedly")
		}
		if string(chunk.PCM) != "T:final agent text" {
			t.Fatalf("caller heard %q, want %q", string(chunk.PCM), "T:final agent text")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for caller audio")
	}

	if got := synth.callCount(); got != 2 {
		t.Fatalf("synthesizer called %d times, want 2", got)
	}
}

// TestSessionTranslationErrorFallsBackToPassthrough replaces Week 1's
// "drop the utterance on translation error" behavior (see session.go's
// runLeg doc comment history) with Week 3's graceful degradation: instead
// of silently dropping the caller's audio, the agent must still hear
// *something* -- the original, untranslated audio -- rather than nothing
// and rather than a mistranslation.
func TestSessionTranslationErrorFallsBackToPassthrough(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "will fail", Language: "hi", IsFinal: true}},
			{},
		},
	}
	translator := &fakeTranslator{err: errors.New("boom")}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{1, 2, 3, 4}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	var gotOriginal bool
	deadline := time.After(2 * time.Second)
	for !gotOriginal {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatalf("AgentHearsAudio closed unexpectedly before original audio arrived")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				gotOriginal = true
			}
			if chunk.IsFinal && !gotOriginal {
				t.Fatalf("saw final passthrough chunk without the original audio anywhere in the stream")
			}
		case <-deadline:
			t.Fatal("timed out waiting for passthrough audio after translation error")
		}
	}

	if got := translator.callCount(); got != 1 {
		t.Fatalf("translator called %d times, want 1", got)
	}
	if sess.CallerLegDegraded() {
		t.Fatal("a single translate error must not permanently degrade the leg (default MaxConsecutiveFailures is 3)")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// TestSessionLowConfidenceFallsBackToPassthrough exercises the
// ConfidenceThreshold trigger: a final transcript with confidence below
// the threshold must never reach the Translator at all, and the listening
// party must still hear the original audio.
func TestSessionLowConfidenceFallsBackToPassthrough(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "mumble mumble", Language: "hi", IsFinal: true, Confidence: 0.1}},
			{},
		},
	}
	translator := &fakeTranslator{}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{9, 9, 9, 9}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	var sawOriginal bool
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatalf("AgentHearsAudio closed unexpectedly")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			if chunk.IsFinal {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for low-confidence passthrough audio")
		}
	}
done:
	if !sawOriginal {
		t.Fatal("expected the original audio to be forwarded on low confidence")
	}
	if got := translator.callCount(); got != 0 {
		t.Fatalf("translator called %d times, want 0 (low-confidence transcripts must never reach Translate)", got)
	}
}

// TestSessionLegDegradesAfterConsecutiveFailuresAndStaysDegraded exercises
// ROADMAP.md's "a leg drops ... backend returns a fatal, non-retryable
// error" case: repeated MT failures must permanently degrade the leg
// (stop even attempting translation) rather than retrying forever or
// hanging, and every subsequent utterance on that leg must still produce
// passthrough audio, not silence.
func TestSessionLegDegradesAfterConsecutiveFailuresAndStaysDegraded(t *testing.T) {
	const attempts = 5 // > default MaxConsecutiveFailures (3)
	scripts := make([]asr.Transcript, attempts)
	for i := range scripts {
		scripts[i] = asr.Transcript{Text: "will fail", Language: "hi", IsFinal: true}
	}
	rec := &fakeRecognizer{scripts: [][]asr.Transcript{scripts, {}}}
	translator := &fakeTranslator{err: errors.New("permanent boom")}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	for i := 0; i < attempts; i++ {
		frame := asr.AudioFrame{PCM: []byte{byte(i), byte(i), byte(i)}, SampleRate: 8000}
		if err := sess.PushCallerAudio(frame); err != nil {
			t.Fatalf("PushCallerAudio #%d: %v", i, err)
		}

		var sawFinal bool
		deadline := time.After(2 * time.Second)
		for !sawFinal {
			select {
			case chunk, ok := <-sess.AgentHearsAudio():
				if !ok {
					t.Fatalf("AgentHearsAudio closed unexpectedly on attempt %d", i)
				}
				sawFinal = chunk.IsFinal
			case <-deadline:
				t.Fatalf("timed out waiting for passthrough audio on attempt %d", i)
			}
		}
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("expected caller leg to be permanently degraded after repeated translate failures")
	}
	// The default MaxConsecutiveFailures is 3, so calls 1-3 reach the
	// translator; once degraded, later attempts must be short-circuited
	// straight to passthrough without calling Translate again.
	if got := translator.callCount(); got != 3 {
		t.Fatalf("translator called %d times, want exactly 3 (degrade after the 3rd failure, no further calls)", got)
	}
}

// TestSessionTranslateTimeoutFallsBackToPassthrough exercises
// ROADMAP.md's "translation lags ... exceeds a bounded timeout" trigger
// for the MT leg: a Translator that never returns within
// FallbackConfig.TranslateTimeout must not hang the session -- it must
// degrade to passthrough, same as an outright Translate error.
func TestSessionTranslateTimeoutFallsBackToPassthrough(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "slow", Language: "hi", IsFinal: true}},
			{},
		},
	}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = &slowTranslator{delay: 500 * time.Millisecond}
	cfg.Fallback = FallbackConfig{TranslateTimeout: 50 * time.Millisecond}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{7, 7, 7}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var sawOriginal bool
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			if chunk.IsFinal {
				if !sawOriginal {
					t.Fatal("expected original audio somewhere in the passthrough stream")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for passthrough after a translate timeout (session may be hanging)")
		}
	}
}

// TestSessionTTSStallFallsBackToPassthrough exercises the TTS-side
// counterpart: a Synthesizer that never produces a first chunk within
// FallbackConfig.SynthesizeTimeout must degrade to passthrough instead of
// hanging the leg forever.
func TestSessionTTSStallFallsBackToPassthrough(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "will stall", Language: "hi", IsFinal: true}},
			{},
		},
	}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = &fakeTranslator{}
	cfg.TTS = &stallingSynthesizer{}
	cfg.Fallback = FallbackConfig{SynthesizeTimeout: 50 * time.Millisecond}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{8, 8, 8}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var sawOriginal bool
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			if chunk.IsFinal {
				if !sawOriginal {
					t.Fatal("expected original audio somewhere in the passthrough stream")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for passthrough after a TTS stall (session may be hanging)")
		}
	}
}

// TestSessionASRStreamPermanentClosureDegradesLegAndForwardsBufferedAudio
// exercises the gap fixed in runLeg's `tr, ok := <-transcripts` case: a
// leg's ASR StreamSession's Transcripts() channel closing on its own,
// mid-call (e.g. Deepgram's failAndClose after exhausting its own
// reconnect/retry budget, see pkg/asr/backoff.go) -- as opposed to
// Session.Close() deliberately closing it -- is a permanent failure that
// must be visible (CallerLegDegraded/AgentLegDegraded, and a dashboard
// event tagged reasonASRStreamClosed) and must not silently drop whatever
// raw audio was still buffered for the in-flight utterance.
func TestSessionASRStreamPermanentClosureDegradesLegAndForwardsBufferedAudio(t *testing.T) {
	rec := &fakeRecognizer{
		// Neither leg's fakeStreamSession ever emits a scripted
		// transcript: this test drives the caller leg's permanent-failure
		// path purely by closing its Transcripts() channel directly,
		// simulating a backend that has exhausted its own internal
		// reconnect/retry budget mid-utterance.
		scripts: [][]asr.Transcript{{}, {}},
	}
	metrics := observability.NewLatencyRecorder()

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Fallback = FallbackConfig{Metrics: metrics}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{11, 22, 33, 44}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	if len(rec.sessions) == 0 {
		t.Fatal("expected fakeRecognizer to have started at least one stream")
	}
	callerStream := rec.sessions[0]

	// Simulate the ASR backend permanently failing mid-call: its
	// Transcripts() channel closes on its own, independent of
	// Session.Close ever being called.
	if err := callerStream.Close(); err != nil {
		t.Fatalf("closing fake caller ASR stream: %v", err)
	}

	// The buffered audio for the in-flight utterance must still reach the
	// agent as a final passthrough chunk, instead of being silently
	// dropped.
	var sawOriginal, sawFinal bool
	deadline := time.After(2 * time.Second)
	for !sawFinal {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			sawFinal = chunk.IsFinal
		case <-deadline:
			t.Fatal("timed out waiting for passthrough audio after the ASR stream closed")
		}
	}
	if !sawOriginal {
		t.Fatal("expected the buffered original audio to be forwarded as passthrough after the ASR stream closed")
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("expected the caller leg to be marked permanently degraded once its ASR stream closed")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("the agent leg's ASR stream never closed; it must not be affected")
	}

	if got := metrics.ReasonCount(stageLegDegraded, "caller", reasonASRStreamClosed); got != 1 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed) = %d, want 1", got)
	}

	// Close must still shut the session down cleanly afterward: the
	// caller leg's goroutine has already exited on its own (proven
	// above), and the agent leg's goroutine must still exit normally once
	// Close closes its (still-live) ASR stream -- proving no goroutine
	// leak, no panic, and no hang.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, ok := <-sess.AgentHearsAudio(); ok {
		t.Fatal("AgentHearsAudio not closed after Close")
	}
	if _, ok := <-sess.CallerHearsAudio(); ok {
		t.Fatal("CallerHearsAudio not closed after Close")
	}
}

// TestSessionCloseDoesNotTriggerASRStreamClosedFallback exercises the
// other side of the same change: Session.Close() itself closes both ASR
// streams as part of normal shutdown (see Close's doc comment), and that
// must NOT be misread as a permanent ASR failure -- no leg should be
// marked degraded, and no reasonASRStreamClosed event should be recorded,
// purely because the call ended normally.
func TestSessionCloseDoesNotTriggerASRStreamClosedFallback(t *testing.T) {
	metrics := observability.NewLatencyRecorder()

	cfg := validConfig()
	cfg.Fallback = FallbackConfig{Metrics: metrics}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if sess.CallerLegDegraded() {
		t.Fatal("a normal Close() must not mark the caller leg permanently degraded")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("a normal Close() must not mark the agent leg permanently degraded")
	}
	if got := metrics.ReasonCount(stageLegDegraded, "caller", reasonASRStreamClosed); got != 0 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed) = %d, want 0 after a normal Close()", got)
	}
	if got := metrics.ReasonCount(stageLegDegraded, "agent", reasonASRStreamClosed); got != 0 {
		t.Fatalf("ReasonCount(leg_degraded, agent, asr_stream_closed) = %d, want 0 after a normal Close()", got)
	}
}

func TestSessionCloseClosesOutboundChannels(t *testing.T) {
	cfg := validConfig()
	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both channels must be closed (reads return the zero value with
	// ok == false), proving the leg goroutines exited before Close
	// returned (Close only closes them after wg.Wait()).
	if _, ok := <-sess.AgentHearsAudio(); ok {
		t.Fatal("AgentHearsAudio channel not closed after Close")
	}
	if _, ok := <-sess.CallerHearsAudio(); ok {
		t.Fatal("CallerHearsAudio channel not closed after Close")
	}

	// Close must be idempotent and safe to call again.
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

func TestSessionCloseDoesNotLeakGoroutines(t *testing.T) {
	// Let any goroutines from prior subtests settle before sampling the
	// baseline.
	settleGoroutines(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 25; i++ {
		cfg := validConfig()
		sess, err := NewSession(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio: %v", err)
		}
		if err := sess.PushAgentAudio(asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}); err != nil {
			t.Fatalf("PushAgentAudio: %v", err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	settleGoroutines(t)
	after := runtime.NumGoroutine()

	// Allow a small amount of slack for the Go runtime/test harness's own
	// housekeeping goroutines, but 25 sessions' worth of leaked leg
	// goroutines (50 goroutines) would blow well past this.
	if after > before+4 {
		t.Fatalf("possible goroutine leak: before=%d after=%d", before, after)
	}
}

// settleGoroutines gives background goroutines a chance to actually exit
// (Close's wg.Wait() only guarantees the goroutine functions have
// returned; it doesn't force an immediate runtime.NumGoroutine() update)
// before the test samples the goroutine count.
func settleGoroutines(t *testing.T) {
	t.Helper()
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionVoicePersonaOverride(t *testing.T) {
	cfg := validConfig()
	cfg.VoicePersona = &tts.Persona{VoiceID: "agent-voice-1", Language: "en", Gender: "female"}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	got := sess.Personas().Get("en")
	if got.VoiceID != "agent-voice-1" || got.Gender != "female" {
		t.Fatalf("Personas().Get(en) = %+v, want VoicePersona override", got)
	}

	// A language with no override still gets a sensible fallback.
	fallback := sess.Personas().Get("hi")
	if fallback.VoiceID == "" {
		t.Fatalf("expected non-empty fallback VoiceID for unassigned language")
	}
}

// TestSessionDrainDeadLegForwardsAudioPushedAfterASRStreamCloses covers
// today's fix (session.go's drainDeadLeg, spawned by runLeg's
// `tr, ok := <-transcripts` branch once a leg's ASR StreamSession has
// permanently died): audio pushed to a leg *after* its ASR stream closes
// must not sit forgotten in leg.audio until Session.Close() -- it must
// still reach the listening party as passthrough, polled on
// FallbackConfig.DeadLegDrainInterval's cadence rather than dropped.
//
// Uses a short (200ms) DeadLegDrainInterval override so the test doesn't
// need to sleep for the 300ms production default, let alone anything
// longer. The 20ms/200ms (10x) margin used for the "not delivered yet"
// assertion below is deliberately generous: this only needs to prove the
// forward doesn't happen synchronously/immediately on push, not pin down
// the exact tick boundary.
func TestSessionDrainDeadLegForwardsAudioPushedAfterASRStreamCloses(t *testing.T) {
	rec := &fakeRecognizer{scripts: [][]asr.Transcript{{}, {}}}
	metrics := observability.NewLatencyRecorder()

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Fallback = FallbackConfig{Metrics: metrics, DeadLegDrainInterval: 200 * time.Millisecond}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	firstFrame := asr.AudioFrame{PCM: []byte{11, 22, 33}, SampleRate: 8000}
	if err := sess.PushCallerAudio(firstFrame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}
	if len(rec.sessions) == 0 {
		t.Fatal("expected fakeRecognizer to have started at least one stream")
	}
	callerStream := rec.sessions[0]

	// Kill the caller leg's ASR stream permanently, mid-call.
	if err := callerStream.Close(); err != nil {
		t.Fatalf("closing fake caller ASR stream: %v", err)
	}

	// Drain the one-time death-drain passthrough of firstFrame (runLeg's
	// own synchronous flush, unchanged by today's fix) before pushing
	// more audio, so it can't be confused with what's under test here.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if chunk.IsFinal {
				goto drained
			}
		case <-deadline:
			t.Fatal("timed out waiting for the initial death-drain passthrough chunk")
		}
	}
drained:

	if !sess.CallerLegDegraded() {
		t.Fatal("expected the caller leg to be permanently degraded once its ASR stream closed")
	}

	// Push audio *after* the leg has already died. fakeStreamSession's
	// PushAudio errors once its stream is closed (unlike some real
	// backends, which might accept the call and simply do nothing) --
	// but Session.PushCallerAudio buffers the frame into leg.audio
	// *unconditionally*, before ever calling into the ASR backend (see
	// PushCallerAudio's doc comment), so that buffering happens
	// regardless of this expected error. The error return itself is not
	// under test here.
	secondFrame := asr.AudioFrame{PCM: []byte{99, 98, 97, 96, 95}, SampleRate: 8000}
	_ = sess.PushCallerAudio(secondFrame)

	// It must not be forwarded immediately: drainDeadLeg only polls every
	// DeadLegDrainInterval (200ms here).
	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		t.Fatalf("expected no passthrough chunk yet (drain cadence not elapsed), got ok=%v chunk=%+v", ok, chunk)
	case <-time.After(20 * time.Millisecond):
	}

	// It must eventually arrive, forwarded as passthrough by drainDeadLeg.
	var sawSecond, sawFinal bool
	deadline = time.After(2 * time.Second)
	for !sawFinal {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if string(chunk.PCM) == string(secondFrame.PCM) {
				sawSecond = true
			}
			sawFinal = chunk.IsFinal
		case <-deadline:
			t.Fatal("timed out waiting for post-death audio to be forwarded as passthrough")
		}
	}
	if !sawSecond {
		t.Fatal("expected the audio pushed after the ASR stream died to be forwarded as passthrough, but it never appeared")
	}

	if got := metrics.ReasonCount(stageLegDegraded, "caller", reasonASRStreamClosedPassthrough); got < 1 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed_passthrough) = %d, want >= 1", got)
	}
	// reasonASRStreamClosed (singular, instant-of-death) must still have
	// fired exactly once, distinct from the ongoing-passthrough reason
	// checked above.
	if got := metrics.ReasonCount(stageLegDegraded, "caller", reasonASRStreamClosed); got != 1 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed) = %d, want 1", got)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSessionDrainDeadLegStopsCleanlyOnCloseNoGoroutineLeak drives the
// same permanent-ASR-failure trigger repeatedly, each time also pushing
// audio *after* the leg dies (so drainDeadLeg actually has something to
// forward, exercising it beyond just spinning on an empty buffer), and
// confirms Session.Close() always shuts the resulting drainDeadLeg
// goroutine down cleanly with no leak -- following the same
// repeated-iteration goroutine-count-check shape as
// asr_permanent_failure_integration_test.go's
// TestASRPermanentFailure_NoGoroutineLeak and this file's own
// TestSessionCloseDoesNotLeakGoroutines.
func TestSessionDrainDeadLegStopsCleanlyOnCloseNoGoroutineLeak(t *testing.T) {
	settleGoroutines(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 15; i++ {
		rec := &fakeRecognizer{scripts: [][]asr.Transcript{{}, {}}}
		cfg := validConfig()
		cfg.ASR = rec
		cfg.Fallback = FallbackConfig{DeadLegDrainInterval: 20 * time.Millisecond}

		sess, err := NewSession(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: []byte{1, 2, 3}, SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio: %v", err)
		}
		if err := rec.sessions[0].Close(); err != nil {
			t.Fatalf("closing fake caller ASR stream: %v", err)
		}

		// Drain until the death-drain final chunk, then push more audio
		// so drainDeadLeg's poll loop actually has work to do at least
		// once before Close() tears everything down.
		for {
			chunk, ok := <-sess.AgentHearsAudio()
			if !ok || chunk.IsFinal {
				break
			}
		}
		// See TestSessionDrainDeadLegForwardsAudioPushedAfterASRStreamCloses's
		// comment on why this expected post-death error is not checked.
		_ = sess.PushCallerAudio(asr.AudioFrame{PCM: []byte{4, 5, 6}, SampleRate: 8000})
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
		t.Fatalf("possible goroutine leak from drainDeadLeg after repeated ASR-permanent-failure cycles: before=%d after=%d", before, after)
	}
}

// TestSessionNormalCloseDoesNotSpawnDeadLegDrainer proves the new
// drainDeadLeg machinery is scoped strictly to the permanent-ASR-failure
// path: an ordinary Session.Close() (no leg ever died on its own) must
// behave exactly as it did before this change -- no
// reasonASRStreamClosedPassthrough (or reasonASRStreamClosed) event ever
// recorded, and Close() must return quickly, well under one
// DeadLegDrainInterval. That second check matters because if a
// drainDeadLeg goroutine were (incorrectly) spawned on a normal shutdown
// too, Close() would have to wait for it to notice s.closing on its next
// tick (see drainDeadLeg's doc comment on why it can't exit purely via
// s.ctx.Done() before that), which would make this test's deliberately
// long 5s DeadLegDrainInterval blow the tight deadline below.
func TestSessionNormalCloseDoesNotSpawnDeadLegDrainer(t *testing.T) {
	metrics := observability.NewLatencyRecorder()

	cfg := validConfig()
	cfg.Fallback = FallbackConfig{Metrics: metrics, DeadLegDrainInterval: 5 * time.Second}

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	start := time.Now()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("normal Close() took %v, want well under one DeadLegDrainInterval (5s) -- suggests a drainDeadLeg goroutine was unexpectedly spawned", elapsed)
	}

	if sess.CallerLegDegraded() || sess.AgentLegDegraded() {
		t.Fatal("a normal Close() must not mark either leg permanently degraded")
	}
	if got := metrics.ReasonCount(stageLegDegraded, "caller", reasonASRStreamClosedPassthrough); got != 0 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed_passthrough) = %d, want 0 after a normal Close()", got)
	}
	if got := metrics.ReasonCount(stageLegDegraded, "agent", reasonASRStreamClosedPassthrough); got != 0 {
		t.Fatalf("ReasonCount(leg_degraded, agent, asr_stream_closed_passthrough) = %d, want 0 after a normal Close()", got)
	}
}
