package langstream

import (
	"sync"

	"github.com/exotel/langstream/pkg/tts"
)

// PersonaManager assigns a default tts.Persona to each language, so a
// caller consistently hears "the same agent" regardless of which language
// they're speaking (and vice versa). It is Week 1 scaffolding: it only
// tracks one persona per language with no per-call overrides, blending, or
// voice cloning. That richer behavior is out of scope for this month per
// ROADMAP.md ("Voice cloning / preserving the original speaker's timbre").
//
// PersonaManager is safe for concurrent use: a Session's two legs may both
// call Get concurrently (each looking up the persona for the language it
// synthesizes into), and callers may adjust assignments mid-call via Set.
type PersonaManager struct {
	mu       sync.RWMutex
	personas map[Language]tts.Persona
}

// NewPersonaManager returns an empty PersonaManager. Get on a language with
// no explicit assignment returns a sensible zero-value persona for that
// language (see Get).
func NewPersonaManager() *PersonaManager {
	return &PersonaManager{personas: make(map[Language]tts.Persona)}
}

// Get returns the persona assigned to lang. If none was assigned via Set,
// it falls back to a deterministic default persona: a neutral-gender voice
// named after the language itself, so downstream TTS backends always
// receive a usable, stable persona rather than a bare zero value.
func (pm *PersonaManager) Get(lang Language) tts.Persona {
	pm.mu.RLock()
	p, ok := pm.personas[lang]
	pm.mu.RUnlock()
	if ok {
		return p
	}
	return tts.Persona{
		VoiceID:  "default-" + string(lang),
		Language: tts.Language(lang),
		Gender:   "neutral",
	}
}

// Set assigns the default persona to use for lang. Passing a zero
// tts.Persona is valid and simply reverts lookups for lang to the
// synthesized default (Get keys purely on presence in the internal map,
// but a persona explicitly Set to the zero value still round-trips as
// that same zero value, distinct from the language-named fallback).
func (pm *PersonaManager) Set(lang Language, p tts.Persona) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.personas[lang] = p
}
