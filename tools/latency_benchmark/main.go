// Command latency_benchmark is QA's standalone harness for measuring
// LangStream's glass-to-glass translation latency. It wires a real
// langstream.Session up to PE's real mock backends (pkg/asr, pkg/translate,
// pkg/tts) and pushes synthetic caller-leg audio through it N times,
// recording latency with pkg/observability.LatencyRecorder and printing
// p50/p95/p99 at the end.
//
// IMPORTANT - two caveats, read both before trusting any number this
// prints:
//
//  1. Every number below is measured against instant, in-memory mocks, not
//     a real ASR/MT/TTS vendor. They say nothing about real-world latency.
//     The entire point of this tool existing is that the harness (CLI
//     flags, Session wiring, LatencyRecorder plumbing, percentile
//     reporting) is built and working *today*, so Week 2 can point
//     --caller-lang/--agent-lang at real backend-backed
//     asr.Recognizer/translate.Translator/tts.Synthesizer implementations
//     and get real numbers on day one instead of writing this file then.
//
//  2. As of Week 1, the "glass-to-glass" stage below (PushCallerAudio ->
//     AgentHearsAudio) almost never fires against asr.MockRecognizer: QA
//     found and reported a bug in langstream.Session.Close() where the
//     final transcript an ASR backend flushes at stream-close time (which
//     is exactly when asr.MockRecognizer, and presumably a real streaming
//     vendor, delivers a call's last utterance) is dropped due to a
//     cancel-before-close ordering bug. See
//     langstream_integration_test.go's TestSessionClose_DropsFinalUtteranceOnHangup
//     for the full writeup and repro. Until that's fixed, expect
//     "glass_to_glass_ms" below to report 0 samples; "session_setup_ms"
//     and "session_close_ms" are unaffected by that bug and do report
//     real (if mock-cheap) numbers today, which at least proves the
//     LatencyRecorder/percentile machinery works end-to-end.
//
// -vendor-fake (added Week 2, additive - the default/mock path above is
// unchanged): swaps the Week 1 mocks for the real Sarvam/GPT-4o/Cartesia
// vendor client code (pkg/asr, pkg/translate, pkg/tts) pointed at
// in-process fake servers (see vendor_fake.go), so the numbers this tool
// prints become a fake-server round-trip latency proxy - real client-side
// marshaling, WebSocket/HTTP framing, and goroutine plumbing, just no
// actual network hop to a vendor - instead of a pure in-memory-mock
// number. This is strictly additive: without the flag, behavior and
// output are identical to before.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

func main() {
	iterations := flag.Int("iterations", 100, "number of simulated call sessions to run")
	pcmBytes := flag.Int("pcm-bytes", 3000, "size in bytes of the single caller-leg audio frame pushed per iteration; kept below the mock ASR's 8000-byte auto-flush threshold (see pkg/asr/mock.go) so the frame is only finalized when the session closes, mirroring a short one-utterance call")
	iterationTimeout := flag.Duration("iteration-timeout", 50*time.Millisecond, "how long to wait per iteration for a translated audio chunk to arrive on AgentHearsAudio() before giving up on that iteration; this is a safety valve so a stuck pipeline can't hang the benchmark forever, not a real SLA")
	callerLang := flag.String("caller-lang", "hi", "caller leg language (must be supported by both the ASR and translate mocks; hi/en is the pilot's only supported pair per ROADMAP.md)")
	agentLang := flag.String("agent-lang", "en", "agent leg language")
	verbose := flag.Bool("verbose", false, "print a line per iteration instead of just the final summary")
	vendorFake := flag.Bool("vendor-fake", false, "measure against the real Sarvam/GPT-4o/Cartesia vendor client code pointed at in-process fake servers, instead of the default in-memory mocks (see vendor_fake.go); requires --caller-lang=hi --agent-lang=en (the only pair the fake servers below script a response for)")
	flag.Parse()

	rec := observability.NewLatencyRecorder()

	var fakeServers *fakeVendorServers
	var vendorASR asr.Recognizer
	var vendorMT translate.Translator
	var vendorTTS tts.Synthesizer
	if *vendorFake {
		if langstream.Language(*callerLang) != "hi" || langstream.Language(*agentLang) != "en" {
			fmt.Fprintln(os.Stderr, "-vendor-fake requires --caller-lang=hi --agent-lang=en")
			os.Exit(1)
		}
		setFakeVendorAPIKeys()
		fakeServers = startFakeVendorServers()
		defer fakeServers.Close()

		var err error
		vendorASR, err = asr.NewSarvamRecognizer(asr.WithSarvamBaseURL(fakeServers.SarvamWSURL))
		if err != nil {
			fmt.Fprintln(os.Stderr, "vendor-fake: building Sarvam recognizer:", err)
			os.Exit(1)
		}
		vendorMT, err = translate.NewGPT4oTranslator(translate.WithBaseURL(fakeServers.GPT4oHTTPURL), translate.WithAPIKey("fake-benchmark-key"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "vendor-fake: building GPT-4o translator:", err)
			os.Exit(1)
		}
		vendorTTS, err = tts.NewCartesiaSynthesizer(tts.WithBaseURL(fakeServers.CartesiaWSURL))
		if err != nil {
			fmt.Fprintln(os.Stderr, "vendor-fake: building Cartesia synthesizer:", err)
			os.Exit(1)
		}
		fmt.Println("latency_benchmark: -vendor-fake enabled - measuring real Sarvam/GPT-4o/Cartesia client code against in-process fake servers, not the Week 1 mocks")
	}

	var hits, misses, setupErrs int
	for i := 0; i < *iterations; i++ {
		var outcome bool
		var err error
		if *vendorFake {
			outcome, err = runIterationWithBackends(rec, vendorASR, vendorMT, vendorTTS, langstream.Language(*callerLang), langstream.Language(*agentLang), *pcmBytes, *iterationTimeout)
		} else {
			outcome, err = runIteration(rec, langstream.Language(*callerLang), langstream.Language(*agentLang), *pcmBytes, *iterationTimeout)
		}
		switch {
		case err != nil:
			setupErrs++
			if *verbose {
				fmt.Fprintf(os.Stderr, "iteration %d: error: %v\n", i, err)
			}
		case outcome:
			hits++
			if *verbose {
				fmt.Printf("iteration %d: hit\n", i)
			}
		default:
			misses++
			if *verbose {
				fmt.Printf("iteration %d: miss (no audio within %s)\n", i, *iterationTimeout)
			}
		}
	}

	printReport(rec, *iterations, hits, misses, setupErrs)
}

// runIterationWithBackends mirrors runIteration exactly, except the
// asr.Recognizer/translate.Translator/tts.Synthesizer are passed in
// (shared across iterations) rather than constructed fresh from PE's
// mocks each time - used by -vendor-fake mode, where each backend already
// exists (pointed at a long-lived fake server) and a real Recognizer's
// StartStream is what opens a fresh connection per iteration, not the
// Recognizer construction itself.
func runIterationWithBackends(rec *observability.LatencyRecorder, asrBackend asr.Recognizer, mt translate.Translator, synth tts.Synthesizer, callerLang, agentLang langstream.Language, pcmBytes int, timeout time.Duration) (hit bool, err error) {
	ctx := context.Background()

	cfg := langstream.SessionConfig{
		CallerLanguage: callerLang,
		AgentLanguage:  agentLang,
		ASR:            asrBackend,
		Translator:     mt,
		TTS:            synth,
	}

	setupStart := time.Now()
	sess, err := langstream.NewSession(ctx, cfg)
	rec.Record("session_setup_ms", msSince(setupStart))
	if err != nil {
		return false, fmt.Errorf("NewSession: %w", err)
	}
	defer func() {
		closeStart := time.Now()
		_ = sess.Close()
		rec.Record("session_close_ms", msSince(closeStart))
	}()

	frame := asr.AudioFrame{PCM: make([]byte, pcmBytes), SampleRate: 16000}
	pushStart := time.Now()
	if err := sess.PushCallerAudio(frame); err != nil {
		return false, fmt.Errorf("PushCallerAudio: %w", err)
	}

	select {
	case _, ok := <-sess.AgentHearsAudio():
		if !ok {
			return false, nil
		}
		rec.Record("glass_to_glass_ms", msSince(pushStart))
		return true, nil
	case <-time.After(timeout):
		return false, nil
	}
}

// runIteration simulates one short call: build a Session, push one
// caller-leg audio frame, wait up to timeout for the resulting translated
// audio to reach AgentHearsAudio(), then close the session. It returns
// true if a chunk arrived in time ("hit"), false if the wait timed out
// ("miss"), and a non-nil error only for genuine setup/push failures.
func runIteration(rec *observability.LatencyRecorder, callerLang, agentLang langstream.Language, pcmBytes int, timeout time.Duration) (hit bool, err error) {
	ctx := context.Background()

	cfg := langstream.SessionConfig{
		CallerLanguage: callerLang,
		AgentLanguage:  agentLang,
		ASR:            asr.NewMockRecognizer(asr.Language(callerLang), asr.Language(agentLang)),
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer(tts.Language(callerLang), tts.Language(agentLang)),
	}

	setupStart := time.Now()
	sess, err := langstream.NewSession(ctx, cfg)
	rec.Record("session_setup_ms", msSince(setupStart))
	if err != nil {
		return false, fmt.Errorf("NewSession: %w", err)
	}
	defer func() {
		closeStart := time.Now()
		_ = sess.Close()
		rec.Record("session_close_ms", msSince(closeStart))
	}()

	frame := asr.AudioFrame{PCM: make([]byte, pcmBytes), SampleRate: 8000}
	pushStart := time.Now()
	if err := sess.PushCallerAudio(frame); err != nil {
		return false, fmt.Errorf("PushCallerAudio: %w", err)
	}

	select {
	case _, ok := <-sess.AgentHearsAudio():
		if !ok {
			// Channel closed with nothing delivered - treat like a miss,
			// not an error: this is the documented current behavior (see
			// the package doc comment above), not a crash.
			return false, nil
		}
		rec.Record("glass_to_glass_ms", msSince(pushStart))
		return true, nil
	case <-time.After(timeout):
		return false, nil
	}
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

func printReport(rec *observability.LatencyRecorder, iterations, hits, misses, setupErrs int) {
	fmt.Println()
	fmt.Println("=== LangStream latency_benchmark report ===")
	fmt.Printf("iterations: %d   hits: %d   misses: %d   errors: %d\n", iterations, hits, misses, setupErrs)
	fmt.Println()

	for _, stage := range []string{"glass_to_glass_ms", "session_setup_ms", "session_close_ms"} {
		n := rec.Count(stage)
		if n == 0 {
			fmt.Printf("%-20s  no samples collected\n", stage)
			continue
		}
		p50 := rec.Percentile(stage, 50)
		p95 := rec.Percentile(stage, 95)
		p99 := rec.Percentile(stage, 99)
		fmt.Printf("%-20s  n=%-5d p50=%8.4fms  p95=%8.4fms  p99=%8.4fms\n", stage, n, p50, p95, p99)
	}

	fmt.Println()
	fmt.Println("NOTE: these numbers are measured against in-memory mocks (pkg/asr,")
	fmt.Println("pkg/translate, pkg/tts MockRecognizer/MockTranslator/MockSynthesizer),")
	fmt.Println("not real vendor APIs. They are meaningless for real latency planning;")
	fmt.Println("this tool exists so Week 2 can swap in real backends and get real")
	fmt.Println("numbers immediately instead of building this harness from scratch.")

	if rec.Count("glass_to_glass_ms") == 0 {
		fmt.Println()
		fmt.Println("NOTE: glass_to_glass_ms has zero samples because of a known bug in")
		fmt.Println("langstream.Session.Close() that drops the final ASR transcript flushed")
		fmt.Println("at stream-close time (see langstream_integration_test.go's")
		fmt.Println("TestSessionClose_DropsFinalUtteranceOnHangup for the full repro/root")
		fmt.Println("cause). session_setup_ms/session_close_ms above are unaffected by that")
		fmt.Println("bug and confirm the recorder/percentile machinery itself works.")
	}
}
