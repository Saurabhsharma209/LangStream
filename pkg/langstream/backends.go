// Backend registry: lets callers (the CLI, tests, or future orchestration
// code) select an asr.Recognizer / translate.Translator / tts.Synthesizer
// implementation *by name* at runtime instead of importing a concrete
// vendor package directly. This is the seam Week 2's "--backend" CLI flag
// and its LANGSTREAM_{ASR,MT,TTS}_BACKEND env-var equivalents are built on.
//
// Why a registry instead of a switch statement: the real vendor backends
// (Deepgram/Sarvam for ASR, GPT-4o for MT, Cartesia for TTS) are being
// built concurrently in pkg/asr, pkg/translate, and pkg/tts by a different
// workstream, landing under constructor names that are not yet fixed. This
// package must not import those constructors before they exist (it would
// not compile), so instead each backend registers itself against a
// well-known string name, and this package only depends on the *interface*
// types (asr.Recognizer, translate.Translator, tts.Synthesizer), which are
// already stable. Once the vendor constructors land, wiring them in is a
// one-line RegisterXBackend call (see doc comments below) -- no changes
// needed here or in cmd/langstream/main.go's selection logic.
//
// Only "mock" is registered today, wired to the existing
// asr.NewMockRecognizer / translate.NewMockTranslator / tts.NewMockSynthesizer
// backends, and it works fully end-to-end.
package langstream

import (
	"fmt"
	"sort"
	"sync"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// BackendMock is the name of the deterministic, no-API-key-required
// backend registered for all three legs (ASR, MT, TTS) by default. It is
// also the default backend name the CLI falls back to when no
// --backend flag or LANGSTREAM_*_BACKEND env var is set (see
// cmd/langstream/main.go).
const BackendMock = "mock"

// ASRBackendFactory constructs a fresh asr.Recognizer. Factories are called
// once per selection (e.g. once at CLI startup), not once per session, so
// they're free to do setup work (reading API keys from the environment,
// opening vendor SDK clients, etc.) that would be wasteful per-call.
type ASRBackendFactory func() (asr.Recognizer, error)

// TranslatorBackendFactory constructs a fresh translate.Translator.
type TranslatorBackendFactory func() (translate.Translator, error)

// TTSBackendFactory constructs a fresh tts.Synthesizer.
type TTSBackendFactory func() (tts.Synthesizer, error)

// backendRegistry holds the three independent name->factory maps (one per
// pipeline leg) behind a single mutex. The three legs are selected and
// registered independently (e.g. a "mock" ASR can be paired with a real
// "gpt4o" translator) so this is three maps, not one.
type backendRegistry struct {
	mu        sync.RWMutex
	asrFac    map[string]ASRBackendFactory
	translFac map[string]TranslatorBackendFactory
	ttsFac    map[string]TTSBackendFactory
}

// defaultRegistry is the process-wide registry used by the package-level
// Register*/New* functions below. It is a package var (not an exported
// type) so callers don't need to thread a registry instance through their
// code just to select a backend by name; tests that need isolation
// construct their own backendRegistry via newBackendRegistry instead of
// mutating this shared one.
var defaultRegistry = newBackendRegistry()

// newBackendRegistry returns a registry pre-populated with the "mock"
// backend for all three legs, wired to the existing mock implementations
// in pkg/asr, pkg/translate, and pkg/tts.
func newBackendRegistry() *backendRegistry {
	r := &backendRegistry{
		asrFac:    make(map[string]ASRBackendFactory),
		translFac: make(map[string]TranslatorBackendFactory),
		ttsFac:    make(map[string]TTSBackendFactory),
	}
	r.registerASR(BackendMock, func() (asr.Recognizer, error) {
		return asr.NewMockRecognizer(), nil
	})
	r.registerTranslator(BackendMock, func() (translate.Translator, error) {
		return translate.NewMockTranslator(), nil
	})
	r.registerTTS(BackendMock, func() (tts.Synthesizer, error) {
		return tts.NewMockSynthesizer(), nil
	})
	return r
}

// --- Extension points ---
//
// Once PE's real vendor constructors exist, wiring them in looks like
// (typically from cmd/langstream/main.go's init or main, or from an
// integration package that imports both pkg/langstream and the vendor
// package):
//
//	langstream.RegisterASRBackend("deepgram", func() (asr.Recognizer, error) {
//		return asr.NewDeepgramRecognizer(os.Getenv("DEEPGRAM_API_KEY"))
//	})
//	langstream.RegisterASRBackend("sarvam", func() (asr.Recognizer, error) {
//		return asr.NewSarvamRecognizer(os.Getenv("SARVAM_API_KEY"))
//	})
//	langstream.RegisterTranslatorBackend("gpt4o", func() (translate.Translator, error) {
//		return translate.NewGPT4oTranslator(os.Getenv("OPENAI_API_KEY"))
//	})
//	langstream.RegisterTTSBackend("cartesia", func() (tts.Synthesizer, error) {
//		return tts.NewCartesiaSynthesizer(os.Getenv("CARTESIA_API_KEY"))
//	})
//
// After that, selecting them at runtime is just naming them via
// --backend/env var (e.g. LANGSTREAM_ASR_BACKEND=deepgram); no further
// code changes are required in this package.

// RegisterASRBackend registers factory under name in the default registry,
// making it selectable via NewASRBackend(name) / the CLI's --backend flag
// and LANGSTREAM_ASR_BACKEND env var. Registering under an already-used
// name overwrites the previous registration (last write wins), which is
// convenient for tests that want to substitute a fake backend temporarily.
func RegisterASRBackend(name string, factory ASRBackendFactory) {
	defaultRegistry.registerASR(name, factory)
}

// RegisterTranslatorBackend registers factory under name in the default
// registry. See RegisterASRBackend for semantics.
func RegisterTranslatorBackend(name string, factory TranslatorBackendFactory) {
	defaultRegistry.registerTranslator(name, factory)
}

// RegisterTTSBackend registers factory under name in the default registry.
// See RegisterASRBackend for semantics.
func RegisterTTSBackend(name string, factory TTSBackendFactory) {
	defaultRegistry.registerTTS(name, factory)
}

// NewASRBackend constructs the asr.Recognizer registered under name, or
// returns an error naming the unknown backend (and listing what *is*
// registered, to make CLI misconfiguration easy to diagnose) if none was
// registered under that name.
func NewASRBackend(name string) (asr.Recognizer, error) {
	return defaultRegistry.newASR(name)
}

// NewTranslatorBackend constructs the translate.Translator registered
// under name. See NewASRBackend for error semantics.
func NewTranslatorBackend(name string) (translate.Translator, error) {
	return defaultRegistry.newTranslator(name)
}

// NewTTSBackend constructs the tts.Synthesizer registered under name. See
// NewASRBackend for error semantics.
func NewTTSBackend(name string) (tts.Synthesizer, error) {
	return defaultRegistry.newTTS(name)
}

// AvailableASRBackends returns the sorted names of all currently
// registered ASR backends, e.g. for CLI --help output or diagnostics.
func AvailableASRBackends() []string {
	return defaultRegistry.namesASR()
}

// AvailableTranslatorBackends returns the sorted names of all currently
// registered translator backends.
func AvailableTranslatorBackends() []string {
	return defaultRegistry.namesTranslator()
}

// AvailableTTSBackends returns the sorted names of all currently
// registered TTS backends.
func AvailableTTSBackends() []string {
	return defaultRegistry.namesTTS()
}

// --- backendRegistry methods ---

func (r *backendRegistry) registerASR(name string, factory ASRBackendFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asrFac[name] = factory
}

func (r *backendRegistry) registerTranslator(name string, factory TranslatorBackendFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.translFac[name] = factory
}

func (r *backendRegistry) registerTTS(name string, factory TTSBackendFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ttsFac[name] = factory
}

func (r *backendRegistry) newASR(name string) (asr.Recognizer, error) {
	r.mu.RLock()
	factory, ok := r.asrFac[name]
	available := sortedKeys(r.asrFac)
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("langstream: unknown ASR backend %q (available: %v)", name, available)
	}
	return factory()
}

func (r *backendRegistry) newTranslator(name string) (translate.Translator, error) {
	r.mu.RLock()
	factory, ok := r.translFac[name]
	available := sortedKeys(r.translFac)
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("langstream: unknown translator backend %q (available: %v)", name, available)
	}
	return factory()
}

func (r *backendRegistry) newTTS(name string) (tts.Synthesizer, error) {
	r.mu.RLock()
	factory, ok := r.ttsFac[name]
	available := sortedKeys(r.ttsFac)
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("langstream: unknown TTS backend %q (available: %v)", name, available)
	}
	return factory()
}

func (r *backendRegistry) namesASR() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.asrFac)
}

func (r *backendRegistry) namesTranslator() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.translFac)
}

func (r *backendRegistry) namesTTS() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.ttsFac)
}

// sortedKeys returns the sorted keys of any string-keyed map, used to
// produce deterministic "available backends" lists for error messages and
// diagnostics (map iteration order is randomized in Go, which would
// otherwise make error messages flaky across runs).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
