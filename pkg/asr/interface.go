// Package asr defines the streaming speech-recognition abstraction used by
// LangStream. Every ASR backend (Deepgram, Sarvam, Whisper, ...) implements
// Recognizer so the session orchestrator never depends on a specific vendor.
package asr

import "context"

// Language is a BCP-47-ish tag, e.g. "hi", "ta", "en", "ar".
type Language string

// Transcript is one chunk of recognized speech. Streaming recognizers emit
// a sequence of Transcripts per utterance: zero or more IsFinal=false
// partials followed by exactly one IsFinal=true result.
type Transcript struct {
	Text       string
	Language   Language // detected or configured language of this chunk
	IsFinal    bool
	Confidence float64
	// StartMS/EndMS are offsets (ms) into the session's audio stream,
	// used for latency measurement and for aligning MT/TTS downstream.
	StartMS int64
	EndMS   int64
}

// AudioFrame is a single chunk of PCM audio (16-bit LE, mono) pushed into
// a streaming recognition session. SampleRate is typically 8000 (telephony)
// or 16000 (post ClearStream upsampling).
type AudioFrame struct {
	PCM         []byte
	SampleRate  int
	TimestampMS int64
}

// StreamSession is a single duplex recognition session bound to one leg
// of a call (caller or agent). Implementations must be safe for one
// concurrent writer (PushAudio) and one concurrent reader (Transcripts).
type StreamSession interface {
	// PushAudio feeds one frame of audio into the recognizer. Must not
	// block longer than ~20ms under normal conditions.
	PushAudio(ctx context.Context, frame AudioFrame) error

	// Transcripts returns a channel of recognized chunks. Closed when
	// the session ends or the context is cancelled.
	Transcripts() <-chan Transcript

	// Close flushes any buffered audio and releases resources.
	Close() error
}

// Recognizer is the factory every ASR backend implements.
type Recognizer interface {
	// Name identifies the backend for logging/metrics, e.g. "deepgram", "sarvam".
	Name() string

	// SupportedLanguages lists languages this backend can recognize.
	SupportedLanguages() []Language

	// StartStream opens a new streaming recognition session for the given
	// language hint. Pass "" to enable auto language detection / code-switching
	// if the backend supports it (Sarvam does; most others don't).
	StartStream(ctx context.Context, languageHint Language) (StreamSession, error)
}
