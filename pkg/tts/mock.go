package tts

import (
	"context"
	"fmt"
	"time"
)

// mockSampleRate is the sample rate used for all synthesized mock audio.
// 8kHz matches telephony PCM as documented in pkg/asr.
const mockSampleRate = 8000

// mockFrameBytes is the size of one synthesized PCM chunk: 20ms of 16-bit
// mono audio at mockSampleRate (8000 samples/sec * 0.02s * 2 bytes/sample
// = 320 bytes), matching a typical RTP packetization interval.
const mockFrameBytes = 320

// mockChunkDelay is the artificial delay between emitted chunks. It is
// small enough to keep `go test` fast but non-zero so downstream latency
// tests exercise real inter-chunk timing rather than a busy loop.
const mockChunkDelay = 2 * time.Millisecond

// mockWordsPerChunk controls how synthesis "paces" through the input text:
// roughly one chunk gets emitted per this many whitespace-delimited words
// (with a minimum of one chunk for the whole utterance), so longer text
// produces more chunks, keeping the mock's shape plausible without being
// a real TTS engine.
const mockWordsPerChunk = 3

// MockSynthesizer is a deterministic, in-memory implementation of
// Synthesizer. It does not produce real speech: each chunk's PCM payload is
// a fixed, deterministic byte pattern derived from the chunk index, so
// tests can assert on chunk count/sizes/ordering without depending on any
// audio codec. It exists so the rest of the pipeline can be built and
// tested before a real TTS vendor (Cartesia, ElevenLabs, Sarvam TTS) is
// wired in.
type MockSynthesizer struct {
	langs []Language
}

// NewMockSynthesizer returns a MockSynthesizer supporting the given
// languages. If none are given, it defaults to "en" and "hi".
func NewMockSynthesizer(langs ...Language) *MockSynthesizer {
	if len(langs) == 0 {
		langs = []Language{"en", "hi"}
	}
	cp := make([]Language, len(langs))
	copy(cp, langs)
	return &MockSynthesizer{langs: cp}
}

// Name implements Synthesizer.
func (m *MockSynthesizer) Name() string { return "mock" }

// SupportedLanguages implements Synthesizer.
func (m *MockSynthesizer) SupportedLanguages() []Language {
	out := make([]Language, len(m.langs))
	copy(out, m.langs)
	return out
}

// SynthesizeStream implements Synthesizer. It returns a channel that emits
// a handful of fake PCM chunks (320 bytes each, i.e. 20ms @ 8kHz/16-bit
// mono) with a small artificial delay between them, ending with exactly
// one IsFinal=true chunk. The channel is always closed, whether synthesis
// "succeeds" or the context is cancelled midway.
func (m *MockSynthesizer) SynthesizeStream(ctx context.Context, text string, persona Persona) (<-chan AudioChunk, error) {
	if text == "" {
		return nil, fmt.Errorf("tts/mock: empty text")
	}
	if !m.supports(persona.Language) {
		return nil, fmt.Errorf("tts/mock: unsupported language %q", persona.Language)
	}

	numChunks := wordCount(text)/mockWordsPerChunk + 1
	if numChunks < 1 {
		numChunks = 1
	}

	out := make(chan AudioChunk, numChunks)

	go func() {
		defer close(out)
		for i := 0; i < numChunks; i++ {
			chunk := AudioChunk{
				PCM:        fakePCM(i),
				SampleRate: mockSampleRate,
				IsFinal:    i == numChunks-1,
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}

			if i < numChunks-1 {
				select {
				case <-time.After(mockChunkDelay):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

func (m *MockSynthesizer) supports(lang Language) bool {
	for _, l := range m.langs {
		if l == lang {
			return true
		}
	}
	return false
}

// fakePCM builds a deterministic mockFrameBytes-sized "PCM" payload for
// chunk index i, so tests can assert both size and content without a real
// codec. The byte pattern simply encodes the chunk index repeated across
// the frame.
func fakePCM(i int) []byte {
	b := make([]byte, mockFrameBytes)
	for j := range b {
		b[j] = byte((i + j) % 256)
	}
	return b
}

// wordCount returns the number of whitespace-delimited words in s.
func wordCount(s string) int {
	count := 0
	inWord := false
	for _, r := range s {
		isSpace := r == ' ' || r == '\t' || r == '\n' || r == '\r'
		if isSpace {
			inWord = false
			continue
		}
		if !inWord {
			count++
			inWord = true
		}
	}
	return count
}

var _ Synthesizer = (*MockSynthesizer)(nil)
