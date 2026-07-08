package tts

// This file maps LangStream's persona concept (see
// pkg/langstream/personas.go) onto concrete Cartesia voice IDs.
//
// pkg/langstream.PersonaManager currently only tracks one Persona per
// Language (Week 1 scaffolding; no per-call overrides yet), and its Get()
// falls back to a synthesized default persona of the form:
//
//	tts.Persona{VoiceID: "default-" + string(lang), Language: lang, Gender: "neutral"}
//
// So the mapping below is keyed first by Language, then by
// Persona.VoiceID, with "default-en" / "default-hi" as the entries that
// PersonaManager's fallback resolves to. Callers that *do* assign a named
// persona (e.g. via PersonaManager.Set with a custom VoiceID) can add more
// entries here without touching SynthesizeStream.

// Language constants for the two languages this backend supports today.
// These intentionally match the literal strings pkg/tts/mock.go's default
// languages use ("en", "hi") so a Persona built against either backend is
// interchangeable.
const (
	LanguageEnglish Language = "en"
	LanguageHindi   Language = "hi"
)

// cartesiaVoices maps Language -> (persona VoiceID -> Cartesia voice ID).
//
// Voice IDs marked "confirmed" come directly from Cartesia's published
// docs/examples (docs.cartesia.ai) as of this writing. Voice IDs marked
// "placeholder" are illustrative slots for named personas an operator
// might configure via PersonaManager.Set; they should be replaced with
// real IDs picked from https://play.cartesia.ai/voices (Cartesia's voice
// library UI, which is not scrapable as plain API docs) before relying on
// them in production. Using a placeholder never breaks the pipeline: any
// VoiceID not found in this map falls back to the language's "default-*"
// entry (see voiceFor), and worst case that is also a confirmed ID.
var cartesiaVoices = map[Language]map[string]string{
	LanguageEnglish: {
		// "Newsman" -- a neutral English narrator voice used in Cartesia's
		// own quick-start docs (docs.cartesia.ai/get-started/make-an-api-request).
		// Safe, confirmed default for English.
		"default-en": "a0e99841-438c-4a64-b679-ae501e7d6091",
		// Placeholder persona slots for a two-persona pilot roster
		// (e.g. one agent voice, one "read back the customer" voice).
		// Replace with real IDs from the voice library before pilot go-live.
		"agent-male-en":   "63ff761f-c1e8-414b-b969-d1833d1c870c",
		"agent-female-en": "bf0a246a-8642-498a-9950-80c35e9276b5",
	},
	LanguageHindi: {
		// Cartesia's multilingual voices are selected by voice_id + the
		// `language` field on the generation request (see
		// docs.cartesia.ai/build-with-cartesia/capability-guides/multilingual-voices);
		// a single voice can often speak multiple locales. No specific
		// Hindi voice ID is published in Cartesia's crawlable docs as of
		// this writing, so this is a placeholder: replace with a verified
		// Hindi-capable voice ID from https://play.cartesia.ai/voices
		// before enabling Hindi in a real call.
		"default-hi":      "839495c2-1cd1-49a1-9a5c-2b1a3f6c9d21",
		"agent-male-hi":   "a3f1e2c4-9b7d-4a6e-8c1f-5d2e7b9a4c60",
		"agent-female-hi": "f4c2a1e9-6d3b-4c8a-9e7f-1a2b3c4d5e6f",
	},
}

// cartesiaDefaultVoiceKey returns the persona VoiceID PersonaManager's
// zero-value fallback uses for lang, so voiceFor can recognize it as "no
// persona specified" and resolve it to a sane default voice.
func cartesiaDefaultVoiceKey(lang Language) string {
	return "default-" + string(lang)
}

// voiceFor resolves a Persona to a concrete Cartesia voice ID:
//  1. If persona.Language has a known voice table and persona.VoiceID is a
//     recognized key in it, use that mapped voice.
//  2. Otherwise fall back to the default voice for persona.Language.
//  3. If persona.Language itself isn't one this backend has a voice table
//     for, fall back to the English table entirely (voiceFor is only
//     reached after SupportedLanguages()/supports() has already validated
//     the language in normal use, so this is a last-resort guard against
//     being called out of band rather than the common path).
func (c *CartesiaSynthesizer) voiceFor(persona Persona) string {
	lang := persona.Language
	if lang == "" {
		lang = LanguageEnglish
	}

	byLang, ok := cartesiaVoices[lang]
	if !ok {
		byLang = cartesiaVoices[LanguageEnglish]
		lang = LanguageEnglish
	}

	if persona.VoiceID != "" {
		if id, ok := byLang[persona.VoiceID]; ok {
			return id
		}
	}

	if id, ok := byLang[cartesiaDefaultVoiceKey(lang)]; ok {
		return id
	}

	// Absolute fallback: never send an empty voice id to Cartesia.
	return cartesiaVoices[LanguageEnglish][cartesiaDefaultVoiceKey(LanguageEnglish)]
}
