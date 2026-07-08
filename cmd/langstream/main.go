// Command langstream is LangStream's CLI entrypoint.
//
// Week 1 shipped a version subcommand and a demo subcommand that exercised
// the duplex Session orchestrator end-to-end against local, in-process stub
// implementations of the asr.Recognizer, translate.Translator, and
// tts.Synthesizer interfaces (so the demo didn't depend on the PE
// workstream's pkg/asr/mock.go, pkg/translate/mock.go, pkg/tts/mock.go
// while those were still under active development).
//
// Week 2: those mock backends are stable, and the demo now selects its
// ASR/MT/TTS backends by name through pkg/langstream's backend registry
// (see pkg/langstream/backends.go) via the --backend flag or the
// LANGSTREAM_ASR_BACKEND / LANGSTREAM_MT_BACKEND / LANGSTREAM_TTS_BACKEND
// environment variables, defaulting to "mock" for all three. This is the
// seam real vendor backends (Deepgram/Sarvam, GPT-4o, Cartesia) will be
// selected through once they're registered - no code changes needed here,
// just `langstream demo --backend deepgram` (once that name is registered).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

const version = "langstream v0.1.0 (pilot)"

// Environment variable names used to select a backend for one pipeline leg
// independently. The --backend flag to `langstream demo` sets all three at
// once (the common case: run entirely on mocks, or entirely on real
// vendors); these env vars let a leg be overridden individually (e.g. a
// real ASR backend paired with still-mock MT/TTS) without a --backend
// value for every permutation. Precedence is flag > env var > "mock",
// resolved per leg by resolveBackend.
const (
	envASRBackend = "LANGSTREAM_ASR_BACKEND"
	envMTBackend  = "LANGSTREAM_MT_BACKEND"
	envTTSBackend = "LANGSTREAM_TTS_BACKEND"
)

// init registers the real vendor backends (Deepgram/Sarvam for ASR, GPT-4o
// for MT, Cartesia for TTS) alongside the always-available "mock" backend
// registered by pkg/langstream itself. Registration is unconditional -- it
// does not check whether the corresponding API key env var is set -- so
// `--backend deepgram` always appears in `langstream help`'s available-backends
// list; the constructor itself returns a clear "DEEPGRAM_API_KEY is not set"
// error at selection time if the key is missing, which is a better failure
// mode than silently hiding the option. No live vendor API keys exist in
// this environment yet (see ROADMAP.md Week 2 decision, 2026-07-07), so
// these paths are exercised in CI only via fake local servers (see each
// vendor package's _test.go), not against the real vendor endpoints.
func init() {
	langstream.RegisterASRBackend("deepgram", func() (asr.Recognizer, error) {
		return asr.NewDeepgramRecognizer()
	})
	langstream.RegisterASRBackend("sarvam", func() (asr.Recognizer, error) {
		return asr.NewSarvamRecognizer()
	})
	langstream.RegisterTranslatorBackend("gpt4o", func() (translate.Translator, error) {
		return translate.NewGPT4oTranslator()
	})
	langstream.RegisterTTSBackend("cartesia", func() (tts.Synthesizer, error) {
		return tts.NewCartesiaSynthesizer()
	})
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "demo":
		if err := runDemo(os.Args[2:]); err != nil {
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
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "demo [--backend NAME]")
	fmt.Fprintln(os.Stderr, "    Run a one-shot duplex-session demo against the named backend for")
	fmt.Fprintln(os.Stderr, "    ASR, MT, and TTS alike (default \"mock\"). Override per leg with the")
	fmt.Fprintf(os.Stderr, "    %s / %s / %s env vars.\n", envASRBackend, envMTBackend, envTTSBackend)
	fmt.Fprintf(os.Stderr, "    available backends: asr=%v mt=%v tts=%v\n",
		langstream.AvailableASRBackends(),
		langstream.AvailableTranslatorBackends(),
		langstream.AvailableTTSBackends())
}

// resolveBackend resolves the backend name for one pipeline leg.
// Precedence: flagValue (if non-empty, i.e. --backend was passed) > the
// named environment variable (if set) > langstream.BackendMock.
func resolveBackend(flagValue, envVar string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return langstream.BackendMock
}

// runDemo wires a langstream.Session up with backends selected by name via
// the --backend flag / LANGSTREAM_*_BACKEND env vars (see resolveBackend),
// looked up in pkg/langstream's backend registry, pushes one frame of
// "caller audio", and prints the synthesized audio the agent leg produces
// in response - a minimal, dependency-free proof that both the duplex
// orchestrator wiring and the backend-selection registry work end to end.
//
// The pushed frame is deliberately small enough that the mock ASR backend
// only buffers it rather than emitting a transcript immediately (see
// asr.MockRecognizer); Close() is what flushes it as a final transcript
// (exactly the "caller hangs up mid-utterance" path Session.Close is
// documented against), so the demo closes the session before reading the
// resulting synthesized audio off the now-closed-but-still-buffered
// channel.
func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	backend := fs.String("backend", "", `backend name for ASR, MT, and TTS alike (default "mock")`)
	if err := fs.Parse(args); err != nil {
		return err
	}

	asrName := resolveBackend(*backend, envASRBackend)
	mtName := resolveBackend(*backend, envMTBackend)
	ttsName := resolveBackend(*backend, envTTSBackend)

	fmt.Printf("langstream demo: backends asr=%s mt=%s tts=%s\n", asrName, mtName, ttsName)

	rec, err := langstream.NewASRBackend(asrName)
	if err != nil {
		return fmt.Errorf("selecting ASR backend: %w", err)
	}
	tr, err := langstream.NewTranslatorBackend(mtName)
	if err != nil {
		return fmt.Errorf("selecting MT backend: %w", err)
	}
	syn, err := langstream.NewTTSBackend(ttsName)
	if err != nil {
		return fmt.Errorf("selecting TTS backend: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	frame := asr.AudioFrame{
		PCM:         make([]byte, 320), // 20ms @ 8kHz, 16-bit mono, silence-shaped
		SampleRate:  8000,
		TimestampMS: 0,
	}
	if err := sess.PushCallerAudio(frame); err != nil {
		_ = sess.Close()
		return fmt.Errorf("pushing caller audio: %w", err)
	}

	// Close flushes the buffered frame as a final transcript and blocks
	// until the caller leg has translated and synthesized it (bounded by
	// Session's own internal flush timeout), so the resulting chunk is
	// guaranteed to already be sitting in the buffered AgentHearsAudio
	// channel by the time Close returns.
	if err := sess.Close(); err != nil {
		return fmt.Errorf("closing session: %w", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			return fmt.Errorf("agent audio channel closed before producing output")
		}
		fmt.Printf("agent hears %d bytes of synthesized audio @ %dHz (final=%v)\n",
			len(chunk.PCM), chunk.SampleRate, chunk.IsFinal)
	default:
		return fmt.Errorf("no synthesized audio was produced")
	}

	return nil
}
