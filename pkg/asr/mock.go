package asr

import (
	"context"
	"fmt"
	"sync"
)

// mockPhrases holds a canned, deterministic transcript per supported
// language. These strings are part of the mock's observable contract: other
// workstreams (QA in particular) may assert on them directly, so treat
// changing them as a breaking change.
var mockPhrases = map[Language]string{
	"en": "hello, this is a test call",
	"hi": "नमस्ते, यह एक परीक्षण कॉल है",
}

// mockFlushBytes is the number of accumulated PCM bytes that triggers an
// automatic (non-final) transcript emission. At 8kHz/16-bit mono this is
// roughly 500ms of audio (8000 samples/sec * 2 bytes/sample * 0.5s = 8000).
const mockFlushBytes = 8000

// MockRecognizer is a deterministic, in-memory implementation of Recognizer.
// It performs no real speech recognition: it accumulates pushed audio and,
// once enough bytes have arrived (or the session is closed), emits a canned
// Transcript for the session's language. It exists so the rest of the
// pipeline (orchestrator, MT, TTS, tests) can be built and exercised before
// any real ASR vendor is wired in.
type MockRecognizer struct {
	langs []Language
}

// NewMockRecognizer returns a MockRecognizer supporting the given languages.
// If no languages are provided, it defaults to "en" and "hi".
func NewMockRecognizer(langs ...Language) *MockRecognizer {
	if len(langs) == 0 {
		langs = []Language{"en", "hi"}
	}
	cp := make([]Language, len(langs))
	copy(cp, langs)
	return &MockRecognizer{langs: cp}
}

// Name implements Recognizer.
func (m *MockRecognizer) Name() string { return "mock" }

// SupportedLanguages implements Recognizer.
func (m *MockRecognizer) SupportedLanguages() []Language {
	out := make([]Language, len(m.langs))
	copy(out, m.langs)
	return out
}

// StartStream implements Recognizer. languageHint selects which canned
// phrase is emitted; an empty hint (auto-detect) defaults to "en".
func (m *MockRecognizer) StartStream(ctx context.Context, languageHint Language) (StreamSession, error) {
	lang := languageHint
	if lang == "" {
		lang = "en"
	}
	if !m.supports(lang) {
		return nil, fmt.Errorf("asr/mock: unsupported language %q", lang)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	s := &mockStreamSession{
		lang: lang,
		// Buffered generously: PushAudio emits synchronously into this
		// channel and Close() waits for in-flight sends to drain before
		// closing it, so a reader that only drains after the stream ends
		// (a common pattern in short-lived tests) must not deadlock Close.
		out:       make(chan Transcript, 64),
		ctx:       sessCtx,
		cancel:    cancel,
		startedMS: 0,
	}
	return s, nil
}

func (m *MockRecognizer) supports(lang Language) bool {
	for _, l := range m.langs {
		if l == lang {
			return true
		}
	}
	return false
}

// mockStreamSession implements StreamSession for MockRecognizer. It is safe
// for one concurrent writer (PushAudio) and one concurrent reader
// (Transcripts), plus a concurrent Close call, guarded by mu.
type mockStreamSession struct {
	mu       sync.Mutex
	lang     Language
	buffered int64 // accumulated PCM bytes since last flush
	totalMS  int64 // total ms of audio pushed so far, for StartMS/EndMS
	seq      int   // number of transcripts emitted so far

	out    chan Transcript
	ctx    context.Context
	cancel context.CancelFunc

	closed    bool
	startedMS int64

	// sendWG tracks sends started by PushAudio that are still in flight.
	// Close() must wait on it after marking the session closed (so no new
	// sends can start) and before closing the channel, otherwise a
	// PushAudio-triggered send could race with close(s.out) and panic.
	sendWG sync.WaitGroup
}

// PushAudio implements StreamSession.
func (s *mockStreamSession) PushAudio(ctx context.Context, frame AudioFrame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("asr/mock: session closed")
	}
	s.buffered += int64(len(frame.PCM))
	// Estimate duration of this frame in ms from PCM length assuming
	// 16-bit mono at frame.SampleRate; guard against div-by-zero.
	frameMS := int64(0)
	if frame.SampleRate > 0 {
		samples := len(frame.PCM) / 2
		frameMS = int64(samples) * 1000 / int64(frame.SampleRate)
	}
	s.totalMS += frameMS

	var toEmit *Transcript
	if s.buffered >= mockFlushBytes {
		s.buffered = 0
		s.seq++
		toEmit = s.buildTranscript(false)
		s.sendWG.Add(1)
	}
	s.mu.Unlock()

	if toEmit != nil {
		s.send(*toEmit)
		s.sendWG.Done()
	}
	return nil
}

// buildTranscript must be called with s.mu held.
func (s *mockStreamSession) buildTranscript(final bool) *Transcript {
	phrase, ok := mockPhrases[s.lang]
	if !ok {
		phrase = mockPhrases["en"]
	}
	start := s.startedMS
	end := s.totalMS
	s.startedMS = s.totalMS
	return &Transcript{
		Text:       phrase,
		Language:   s.lang,
		IsFinal:    final,
		Confidence: 0.99,
		StartMS:    start,
		EndMS:      end,
	}
}

// send delivers t on the output channel without holding s.mu, so PushAudio
// never blocks other callers while waiting for a slow reader. It respects
// context cancellation so it cannot leak a goroutine forever.
func (s *mockStreamSession) send(t Transcript) {
	select {
	case s.out <- t:
	case <-s.ctx.Done():
	}
}

// Transcripts implements StreamSession.
func (s *mockStreamSession) Transcripts() <-chan Transcript {
	return s.out
}

// Close implements StreamSession. It flushes any buffered audio as a final
// transcript (if there is anything left to flush, or nothing has been
// emitted yet), then closes the output channel exactly once. Safe to call
// concurrently with PushAudio and multiple times (idempotent).
func (s *mockStreamSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// Marking closed under the lock guarantees any PushAudio call that
	// acquires the lock afterwards sees it and returns early instead of
	// registering a new send, so sendWG can only still be counting sends
	// that started strictly before this point.
	s.closed = true

	var toEmit *Transcript
	if s.buffered > 0 || s.seq == 0 {
		s.buffered = 0
		s.seq++
		toEmit = s.buildTranscript(true)
	}
	s.mu.Unlock()

	// Wait for any PushAudio-triggered send that was already in flight
	// when we flipped s.closed to finish before we touch the channel.
	s.sendWG.Wait()

	if toEmit != nil {
		s.send(*toEmit)
	}

	s.cancel()
	close(s.out)
	return nil
}

var _ Recognizer = (*MockRecognizer)(nil)
var _ StreamSession = (*mockStreamSession)(nil)
