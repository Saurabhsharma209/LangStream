package tts

// This file maps LangStream's persona concept (see
// pkg/langstream/personas.go) onto concrete ElevenLabs voice IDs, mirroring
// cartesia_voices.go's structure exactly. See that file's doc comment for
// how pkg/langstream.PersonaManager's zero-value fallback
// (tts.Persona{VoiceID: "default-" + string(lang), Language: lang, Gender:
// "neutral"}) drives the "default-en" / "default-hi" keys below.
//
// Unlike Cartesia (where per-language voice IDs are more separated),
// ElevenLabs' eleven_multilingual_v2 model can speak both Hindi and
// English through either voice, confirmed live against the real API on
// 2026-07-14 (see elevenlabs.go's package doc comment). So this backend
// deliberately assigns two audibly distinct voices across the two
// languages -- George (British male) for English, Sarah (American female)
// for Hindi -- rather than one voice per language pair sharing a timbre;
// that makes a live two-user Hindi<->English call easy to tell apart by
// ear. Note this intentionally differs from Cartesia's convention of
// English/Hindi voices being independently chosen without regard to
// "sounding different from each other".
//
// LanguageEnglish and LanguageHindi are declared in cartesia_voices.go and
// reused here rather than redeclared.

// elevenlabsVoices maps Language -> (persona VoiceID -> ElevenLabs voice
// ID).
//
// Both voice IDs below were confirmed to belong to the real ElevenLabs
// account behind the API key used for verification on 2026-07-14, via
// GET /v1/voices -- these are real, working, premade voices, not
// placeholders:
//
//   - JBFqnCBsd6RMkjVDRZzb ("George"): British male, warm storyteller;
//     also the voice ElevenLabs' own official docs examples use.
//   - EXAVITQu4vr4xnSDxMaL ("Sarah"): American female, mature/confident.
var elevenlabsVoices = map[Language]map[string]string{
	LanguageEnglish: {
		// George -- confirmed real voice ID, used as the English default
		// so a two-user Hindi<->English test call has two distinct voices
		// (see this file's doc comment).
		"default-en": "JBFqnCBsd6RMkjVDRZzb",
		// Named persona slots for a two-persona pilot roster, following
		// cartesia_voices.go's "agent-<gender>-<lang>" naming convention.
		// Both map to the same two confirmed voice IDs above (this
		// backend only has two real voice IDs verified so far); replace
		// with additional confirmed IDs from the ElevenLabs voice library
		// before relying on more than two distinct voices in production.
		"agent-male-en":   "JBFqnCBsd6RMkjVDRZzb",
		"agent-female-en": "EXAVITQu4vr4xnSDxMaL",
	},
	LanguageHindi: {
		// Sarah -- confirmed real voice ID, used as the Hindi default
		// (paired opposite George on the English side; see this file's
		// doc comment). eleven_multilingual_v2 speaks Hindi through this
		// voice just as well as English.
		"default-hi":      "EXAVITQu4vr4xnSDxMaL",
		"agent-male-hi":   "JBFqnCBsd6RMkjVDRZzb",
		"agent-female-hi": "EXAVITQu4vr4xnSDxMaL",
	},
}

// elevenlabsDefaultVoiceKey returns the persona VoiceID PersonaManager's
// zero-value fallback uses for lang, so voiceFor can recognize it as "no
// persona specified" and resolve it to a sane default voice.
func elevenlabsDefaultVoiceKey(lang Language) string {
	return "default-" + string(lang)
}

// voiceFor resolves a Persona to a concrete ElevenLabs voice ID:
//  1. If persona.Language has a known voice table and persona.VoiceID is a
//     recognized key in it, use that mapped voice.
//  2. Otherwise fall back to the default voice for persona.Language.
//  3. If persona.Language itself isn't one this backend has a voice table
//     for, fall back to the English table entirely (voiceFor is only
//     reached after SupportedLanguages()/supports() has already validated
//     the language in normal use, so this is a last-resort guard against
//     being called out of band rather than the common path).
func (e *ElevenLabsSynthesizer) voiceFor(persona Persona) string {
	lang := persona.Language
	if lang == "" {
		lang = LanguageEnglish
	}

	byLang, ok := elevenlabsVoices[lang]
	if !ok {
		byLang = elevenlabsVoices[LanguageEnglish]
		lang = LanguageEnglish
	}

	if persona.VoiceID != "" {
		if id, ok := byLang[persona.VoiceID]; ok {
			return id
		}
	}

	if id, ok := byLang[elevenlabsDefaultVoiceKey(lang)]; ok {
		return id
	}

	// Absolute fallback: never send an empty voice id to ElevenLabs.
	return elevenlabsVoices[LanguageEnglish][elevenlabsDefaultVoiceKey(LanguageEnglish)]
}
