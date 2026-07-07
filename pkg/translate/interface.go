// Package translate defines the streaming machine-translation abstraction.
// Translators consume text chunks (from asr.Transcript) and emit translated
// text chunks as soon as they are ready, so downstream TTS can start before
// a whole sentence is translated.
package translate

import "context"

type Language string

// Chunk is one unit of translated text.
type Chunk struct {
	Text       string
	SourceLang Language
	TargetLang Language
	IsFinal    bool // true once this maps to a final (non-partial) ASR transcript
}

// Translator is the interface every MT backend (GPT-4o, DeepL, NLLB-200, ...)
// implements.
type Translator interface {
	Name() string

	// SupportedPairs returns the (source, target) language pairs this
	// backend can translate. Empty source means "any" (e.g. NLLB-200).
	SupportedPairs() [][2]Language

	// Translate converts one chunk of source text. Implementations should
	// be safe to call back-to-back on partial ASR output (cheap/cached where
	// possible) as well as on final output (full quality pass).
	Translate(ctx context.Context, text string, source, target Language, isFinal bool) (Chunk, error)
}
