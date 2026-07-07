// Package tts defines the streaming speech-synthesis abstraction. TTS
// backends must support incremental synthesis: the first audio chunk
// should be returned well before the full text has been synthesized,
// since this sits on the critical latency path of a live call.
package tts

import "context"

type Language string

// AudioChunk is one piece of synthesized PCM audio (16-bit LE, mono).
type AudioChunk struct {
	PCM        []byte
	SampleRate int
	IsFinal    bool // true on the last chunk for this synthesis request
}

// Persona describes the target voice for a language, so a caller
// consistently hears "the same agent" regardless of which language
// they're speaking.
type Persona struct {
	VoiceID  string
	Language Language
	Gender   string // "male", "female", "neutral" - informational only
	// ClonedFrom, if set, references a voice sample used for voice
	// cloning so the translated voice can approximate the original
	// speaker's timbre (Phase 2+; not required for MVP).
	ClonedFrom string
}

// Synthesizer is the interface every TTS backend (Cartesia, ElevenLabs,
// Sarvam TTS, ...) implements.
type Synthesizer interface {
	Name() string

	SupportedLanguages() []Language

	// SynthesizeStream synthesizes text into a stream of audio chunks,
	// starting to emit before the full text is necessarily consumed
	// (implementations should chunk on natural boundaries: clauses,
	// punctuation). The returned channel is closed after the final chunk
	// or on error.
	SynthesizeStream(ctx context.Context, text string, persona Persona) (<-chan AudioChunk, error)
}
