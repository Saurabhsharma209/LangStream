// Command backend_selection demonstrates LangStream's backend registry
// (pkg/langstream/backends.go): it selects the "mock" ASR/MT/TTS backend
// explicitly via the LANGSTREAM_ASR_BACKEND / LANGSTREAM_MT_BACKEND /
// LANGSTREAM_TTS_BACKEND environment variables (falling back to the
// registry's own "mock" default if they're unset, so `go run` with no
// environment still works), then runs a short simulated duplex call
// through a real langstream.Session to prove the registry-selected
// backends work end to end - not just that they construct without error.
//
// This mirrors what `langstream demo --backend mock` does from the CLI
// (cmd/langstream/main.go); this example exists to show the same registry
// used directly from Go code, e.g. for an integration embedding LangStream
// as a library rather than shelling out to the CLI.
//
// Run it with:
//
//	go run ./examples/backend_selection
//
// or, to see the env-var override path explicitly:
//
//	LANGSTREAM_ASR_BACKEND=mock LANGSTREAM_MT_BACKEND=mock LANGSTREAM_TTS_BACKEND=mock \
//		go run ./examples/backend_selection
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
)

// backendFromEnv reads name from the environment, defaulting to
// langstream.BackendMock if unset. It's the same precedence rule
// cmd/langstream/main.go's resolveBackend applies for its env-var path
// (minus the --backend flag override, which is CLI-specific).
func backendFromEnv(envVar string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return langstream.BackendMock
}

func main() {
	asrName := backendFromEnv("LANGSTREAM_ASR_BACKEND")
	mtName := backendFromEnv("LANGSTREAM_MT_BACKEND")
	ttsName := backendFromEnv("LANGSTREAM_TTS_BACKEND")

	fmt.Printf("selecting backends via registry: asr=%s mt=%s tts=%s\n", asrName, mtName, ttsName)
	fmt.Printf("registered backends: asr=%v mt=%v tts=%v\n",
		langstream.AvailableASRBackends(),
		langstream.AvailableTranslatorBackends(),
		langstream.AvailableTTSBackends())

	rec, err := langstream.NewASRBackend(asrName)
	if err != nil {
		log.Fatalf("selecting ASR backend: %v", err)
	}
	tr, err := langstream.NewTranslatorBackend(mtName)
	if err != nil {
		log.Fatalf("selecting MT backend: %v", err)
	}
	syn, err := langstream.NewTTSBackend(ttsName)
	if err != nil {
		log.Fatalf("selecting TTS backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	})
	if err != nil {
		log.Fatalf("creating session: %v", err)
	}

	// Simulate one short utterance on each leg of the call: the caller
	// speaks (heard, translated, and synthesized for the agent) and the
	// agent speaks back (heard, translated, and synthesized for the
	// caller). Frames are sized under the mock ASR's internal flush
	// threshold, so they're only emitted as final transcripts once the
	// session is closed (see pkg/asr/mock.go and Session.Close's doc
	// comment on why final-on-close matters for real streaming ASR too).
	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}

	if err := sess.PushCallerAudio(frame); err != nil {
		log.Fatalf("pushing caller audio: %v", err)
	}
	if err := sess.PushAgentAudio(frame); err != nil {
		log.Fatalf("pushing agent audio: %v", err)
	}

	if err := sess.Close(); err != nil {
		log.Fatalf("closing session: %v", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			log.Fatal("agent audio channel closed with no output")
		}
		fmt.Printf("agent hears %d bytes of synthesized audio @ %dHz\n", len(chunk.PCM), chunk.SampleRate)
	default:
		log.Fatal("no audio produced for the agent leg")
	}

	select {
	case chunk, ok := <-sess.CallerHearsAudio():
		if !ok {
			log.Fatal("caller audio channel closed with no output")
		}
		fmt.Printf("caller hears %d bytes of synthesized audio @ %dHz\n", len(chunk.PCM), chunk.SampleRate)
	default:
		log.Fatal("no audio produced for the caller leg")
	}

	fmt.Println("backend registry demo complete: mock ASR -> MT -> TTS worked end to end in both directions")
}
