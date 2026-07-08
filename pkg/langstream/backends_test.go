package langstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// TestMockBackendsRegisteredByDefault checks that the "mock" name is
// pre-registered for all three legs against the default (package-level)
// registry, since that is what makes `langstream demo --backend mock` (and
// its default, no-flag behavior) work out of the box.
func TestMockBackendsRegisteredByDefault(t *testing.T) {
	rec, err := NewASRBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewASRBackend(%q): %v", BackendMock, err)
	}
	if rec == nil {
		t.Fatal("NewASRBackend returned nil Recognizer with no error")
	}
	if rec.Name() != "mock" {
		t.Errorf("recognizer.Name() = %q, want %q", rec.Name(), "mock")
	}

	tr, err := NewTranslatorBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewTranslatorBackend(%q): %v", BackendMock, err)
	}
	if tr == nil {
		t.Fatal("NewTranslatorBackend returned nil Translator with no error")
	}
	if tr.Name() != "mock" {
		t.Errorf("translator.Name() = %q, want %q", tr.Name(), "mock")
	}

	syn, err := NewTTSBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewTTSBackend(%q): %v", BackendMock, err)
	}
	if syn == nil {
		t.Fatal("NewTTSBackend returned nil Synthesizer with no error")
	}
	if syn.Name() != "mock" {
		t.Errorf("synthesizer.Name() = %q, want %q", syn.Name(), "mock")
	}
}

// TestMockBackendsEndToEnd proves the "mock" registry path is not just a
// stub: backends constructed via the registry must be usable to build a
// real Session and carry a frame of audio through ASR -> MT -> TTS end to
// end, exactly like the CLI demo does.
//
// It pushes a single frame *below* asr.MockRecognizer's internal flush
// threshold so no transcript is emitted on PushAudio; the resulting
// transcript is instead flushed as IsFinal=true by Session.Close(), which
// deterministically waits (bounded, internally) for both leg goroutines to
// drain that flush before closing the outbound audio channels. This
// exercises the same "hang up mid-utterance" flush path documented on
// Session.Close, using the real registered mock backends end to end.
func TestMockBackendsEndToEnd(t *testing.T) {
	rec, err := NewASRBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewASRBackend: %v", err)
	}
	tr, err := NewTranslatorBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewTranslatorBackend: %v", err)
	}
	syn, err := NewTTSBackend(BackendMock)
	if err != nil {
		t.Fatalf("NewTTSBackend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := NewSession(ctx, SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Well under mockFlushBytes (8000): PushAudio buffers it without
	// emitting anything, so the only transcript this session ever
	// produces is the IsFinal=true flush Close() triggers below.
	frame := asr.AudioFrame{
		PCM:        make([]byte, 320),
		SampleRate: 8000,
	}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	// Close flushes the buffered audio as a final transcript and blocks
	// (bounded by its own internal timeout) until the caller leg has
	// translated and synthesized it, so the chunk is guaranteed to be
	// sitting in the buffered agentOut channel by the time Close returns.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			t.Fatal("AgentHearsAudio channel closed with no buffered chunk")
		}
		if len(chunk.PCM) == 0 {
			t.Error("expected non-empty synthesized PCM")
		}
	default:
		t.Fatal("expected a buffered chunk on AgentHearsAudio after Close")
	}
}

// TestUnknownBackendNameErrors checks that requesting an unregistered
// backend name fails clearly (rather than panicking or returning a nil
// interface with a nil error, which would surface downstream as a
// confusing nil-pointer panic instead of a clean startup error).
func TestUnknownBackendNameErrors(t *testing.T) {
	if _, err := NewASRBackend("does-not-exist"); err == nil {
		t.Error("NewASRBackend(unknown): expected error, got nil")
	} else if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not mention the unknown backend name", err)
	}

	if _, err := NewTranslatorBackend("does-not-exist"); err == nil {
		t.Error("NewTranslatorBackend(unknown): expected error, got nil")
	}

	if _, err := NewTTSBackend("does-not-exist"); err == nil {
		t.Error("NewTTSBackend(unknown): expected error, got nil")
	}
}

// TestRegisterASRBackendExtensionPoint exercises the exact extension-point
// pattern documented in backends.go for wiring in a real vendor
// constructor later: RegisterASRBackend under a new name, then select it
// via NewASRBackend. It uses a local fake recognizer (not a real vendor)
// so this test has no external dependency, but the registration mechanics
// are identical to what PE's real Deepgram/Sarvam constructors will use.
func TestRegisterASRBackendExtensionPoint(t *testing.T) {
	const name = "fake-vendor-for-test"
	sentinel := errors.New("boom")

	RegisterASRBackend(name, func() (asr.Recognizer, error) {
		return nil, sentinel
	})

	_, err := NewASRBackend(name)
	if !errors.Is(err, sentinel) {
		t.Fatalf("NewASRBackend(%q) error = %v, want %v", name, err, sentinel)
	}

	names := AvailableASRBackends()
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("AvailableASRBackends() = %v, want it to contain %q", names, name)
	}
}

// TestRegisterTranslatorAndTTSBackendOverwrite checks that re-registering
// a name overwrites the previous factory (documented "last write wins"
// behavior), which is what lets tests substitute a fake backend under a
// scratch name temporarily.
func TestRegisterTranslatorAndTTSBackendOverwrite(t *testing.T) {
	const name = "overwrite-test"

	RegisterTranslatorBackend(name, func() (translate.Translator, error) {
		return translate.NewMockTranslator(), nil
	})
	first, err := NewTranslatorBackend(name)
	if err != nil {
		t.Fatalf("NewTranslatorBackend: %v", err)
	}
	if first.Name() != "mock" {
		t.Fatalf("unexpected translator: %v", first.Name())
	}

	sentinel := errors.New("replaced")
	RegisterTranslatorBackend(name, func() (translate.Translator, error) {
		return nil, sentinel
	})
	if _, err := NewTranslatorBackend(name); !errors.Is(err, sentinel) {
		t.Fatalf("expected overwritten factory to be used, got err=%v", err)
	}

	RegisterTTSBackend(name, func() (tts.Synthesizer, error) {
		return tts.NewMockSynthesizer(), nil
	})
	syn, err := NewTTSBackend(name)
	if err != nil {
		t.Fatalf("NewTTSBackend: %v", err)
	}
	if syn.Name() != "mock" {
		t.Fatalf("unexpected synthesizer: %v", syn.Name())
	}
}

// TestAvailableBackendsIncludesMock checks the Available*Backends
// diagnostics helpers report "mock" as registered from process start,
// independent of any test registering extra names.
func TestAvailableBackendsIncludesMock(t *testing.T) {
	for _, names := range [][]string{
		AvailableASRBackends(),
		AvailableTranslatorBackends(),
		AvailableTTSBackends(),
	} {
		found := false
		for _, n := range names {
			if n == BackendMock {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q in %v", BackendMock, names)
		}
	}
}

// TestIsolatedRegistryStartsEmptyExceptMock checks that a freshly
// constructed backendRegistry (as used internally, and as a template for
// any future per-test isolation needs) only has "mock" registered, not
// whatever extra names other tests may have registered against the
// package-level defaultRegistry.
func TestIsolatedRegistryStartsEmptyExceptMock(t *testing.T) {
	r := newBackendRegistry()
	if got := r.namesASR(); len(got) != 1 || got[0] != BackendMock {
		t.Errorf("fresh registry ASR names = %v, want [%q]", got, BackendMock)
	}
	if got := r.namesTranslator(); len(got) != 1 || got[0] != BackendMock {
		t.Errorf("fresh registry translator names = %v, want [%q]", got, BackendMock)
	}
	if got := r.namesTTS(); len(got) != 1 || got[0] != BackendMock {
		t.Errorf("fresh registry TTS names = %v, want [%q]", got, BackendMock)
	}
}
