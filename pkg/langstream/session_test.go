package langstream

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
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

func TestSessionTranslationErrorDropsUtteranceWithoutHanging(t *testing.T) {
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

	if err := sess.PushCallerAudio(asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		t.Fatalf("expected no audio after translation error, got chunk=%v ok=%v", chunk, ok)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing arrives
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
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
