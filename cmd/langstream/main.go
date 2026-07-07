// Command langstream is LangStream's CLI entrypoint.
//
// Week 1 scope is intentionally small: a version subcommand, and a demo
// subcommand that exercises the duplex Session orchestrator end-to-end
// against minimal, local, in-process implementations of the asr.Recognizer,
// translate.Translator, and tts.Synthesizer interfaces. The demo does not
// depend on pkg/asr/mock.go, pkg/translate/mock.go, or pkg/tts/mock.go
// (owned by the PE workstream) so this command builds and runs regardless
// of the state of those files.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

const version = "langstream v0.1.0 (pilot)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "demo":
		if err := runDemo(); err != nil {
			fmt.Fprintln(os.Stderr, "demo failed:", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: langstream <version|demo|help>")
}

// runDemo wires a langstream.Session up with local stub ASR/Translator/TTS
// implementations, pushes one frame of "caller audio", and prints the
// synthesized audio the agent leg produces in response - a minimal,
// dependency-free proof that the duplex orchestrator wiring works.
func runDemo() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            newStubRecognizer(),
		Translator:     newStubTranslator(),
		TTS:            newStubSynthesizer(),
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	defer sess.Close()

	// One frame is enough: stubRecognizer emits a final transcript on
	// every PushAudio call.
	frame := asr.AudioFrame{
		PCM:         make([]byte, 320), // 20ms @ 8kHz, 16-bit mono, silence-shaped
		SampleRate:  8000,
		TimestampMS: 0,
	}
	if err := sess.PushCallerAudio(frame); err != nil {
		return fmt.Errorf("pushing caller audio: %w", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			return fmt.Errorf("agent audio channel closed before producing output")
		}
		fmt.Printf("agent hears %d bytes of synthesized audio @ %dHz (final=%v)\n",
			len(chunk.PCM), chunk.SampleRate, chunk.IsFinal)
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for translated audio: %w", ctx.Err())
	}

	return nil
}

// --- Minimal local stub implementations, demo-only. ---
//
// These exist solely so `langstream demo` has something concrete to wire
// up without depending on pkg/asr/mock.go, pkg/translate/mock.go, or
// pkg/tts/mock.go (owned by the PE workstream, which may still be under
// active development). They are intentionally tiny: one canned phrase in,
// one bracketed "translation" out, one tone-shaped audio chunk synthesized.
// Real mock backends belong in pkg/asr, pkg/translate, and pkg/tts, not
// here.

type stubRecognizer struct{}

func newStubRecognizer() *stubRecognizer { return &stubRecognizer{} }

func (s *stubRecognizer) Name() string { return "cli-stub" }

func (s *stubRecognizer) SupportedLanguages() []asr.Language {
	return []asr.Language{"en", "hi"}
}

func (s *stubRecognizer) StartStream(ctx context.Context, languageHint asr.Language) (asr.StreamSession, error) {
	lang := languageHint
	if lang == "" {
		lang = "en"
	}
	return &stubStream{lang: lang, out: make(chan asr.Transcript, 4)}, nil
}

// stubStream implements asr.StreamSession. Every PushAudio call
// synchronously emits one final transcript, so the demo doesn't need to
// wait for a flush threshold.
type stubStream struct {
	mu     sync.Mutex
	lang   asr.Language
	out    chan asr.Transcript
	closed bool
	seq    int64
}

func (s *stubStream) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("cli-stub: stream closed")
	}
	s.seq++
	text := "hello, this is a test call"
	if s.lang == "hi" {
		text = "नमस्ते, यह एक परीक्षण कॉल है"
	}
	select {
	case s.out <- asr.Transcript{
		Text:       text,
		Language:   s.lang,
		IsFinal:    true,
		Confidence: 0.95,
		StartMS:    (s.seq - 1) * 20,
		EndMS:      s.seq * 20,
	}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (s *stubStream) Transcripts() <-chan asr.Transcript { return s.out }

func (s *stubStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.out)
	return nil
}

type stubTranslator struct{}

func newStubTranslator() *stubTranslator { return &stubTranslator{} }

func (t *stubTranslator) Name() string { return "cli-stub" }

func (t *stubTranslator) SupportedPairs() [][2]translate.Language {
	return [][2]translate.Language{{"hi", "en"}, {"en", "hi"}}
}

func (t *stubTranslator) Translate(ctx context.Context, text string, source, target translate.Language, isFinal bool) (translate.Chunk, error) {
	select {
	case <-ctx.Done():
		return translate.Chunk{}, ctx.Err()
	default:
	}
	return translate.Chunk{
		Text:       fmt.Sprintf("[%s] %s", target, text),
		SourceLang: source,
		TargetLang: target,
		IsFinal:    isFinal,
	}, nil
}

type stubSynthesizer struct{}

func newStubSynthesizer() *stubSynthesizer { return &stubSynthesizer{} }

func (s *stubSynthesizer) Name() string { return "cli-stub" }

func (s *stubSynthesizer) SupportedLanguages() []tts.Language {
	return []tts.Language{"en", "hi"}
}

func (s *stubSynthesizer) SynthesizeStream(ctx context.Context, text string, persona tts.Persona) (<-chan tts.AudioChunk, error) {
	out := make(chan tts.AudioChunk, 1)
	// One deterministic chunk, sized off the text so different input
	// produces observably different output; this is not real audio.
	pcm := make([]byte, len(text)*2)
	for i := range text {
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(text[i])*137)
	}
	select {
	case out <- tts.AudioChunk{PCM: pcm, SampleRate: 8000, IsFinal: true}:
	case <-ctx.Done():
		close(out)
		return out, ctx.Err()
	}
	close(out)
	return out, nil
}
