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
//
// Week 3 (Sprint 4): a `serve` subcommand starts a long-running Session
// (same backend-selection path as `demo`, see newSession) with
// pkg/observability's dashboard HTTP server (SRE-owned, see
// pkg/observability/dashboard.go) mounted in front of that Session's
// metrics recorder. It does not attach any real telephony transport -
// there is no RTP/SIP socket behind it yet, see
// examples/vsip_example for the integration contract a future transport
// (Exotel vSIP) would fill in, and DEVLOG.md's 2026-07-08/09 entries for
// why duplex RTP itself is still blocked on a ClearStream decision. `serve`
// exists so the dashboard endpoints are reachable against a live (if idle)
// Session today, ahead of that transport being wired in.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

const version = "langstream v0.1.0 (pilot)"

// Environment variable names used to select a backend for one pipeline leg
// independently. The --backend flag to `langstream demo` (and `serve`) sets
// all three at once (the common case: run entirely on mocks, or entirely
// on real vendors); these env vars let a leg be overridden individually
// (e.g. a real ASR backend paired with still-mock MT/TTS) without a
// --backend value for every permutation. Precedence is flag > env var >
// "mock", resolved per leg by resolveBackend.
const (
	envASRBackend = "LANGSTREAM_ASR_BACKEND"
	envMTBackend  = "LANGSTREAM_MT_BACKEND"
	envTTSBackend = "LANGSTREAM_TTS_BACKEND"
)

// defaultDashboardAddr is the default listen address for `langstream
// serve`'s observability dashboard. It matches the port already reserved
// for it in the Dockerfile and docker-compose.yml.
const defaultDashboardAddr = ":8080"

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
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve failed:", err)
			os.Exit(1)
		}
	case "duplex":
		if err := runDuplex(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "duplex failed:", err)
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
	fmt.Fprintln(os.Stderr, "usage: langstream <version|demo|serve|duplex|help>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "demo [--backend NAME]")
	fmt.Fprintln(os.Stderr, "    Run a one-shot duplex-session demo against the named backend for")
	fmt.Fprintln(os.Stderr, "    ASR, MT, and TTS alike (default \"mock\"). Override per leg with the")
	fmt.Fprintf(os.Stderr, "    %s / %s / %s env vars.\n", envASRBackend, envMTBackend, envTTSBackend)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "serve [--backend NAME] [--addr ADDR]")
	fmt.Fprintln(os.Stderr, "    Start a long-running duplex Session (same backend selection as demo)")
	fmt.Fprintf(os.Stderr, "    with the observability dashboard (%s, %s, %s) served on ADDR\n", "/", "/dashboard.json", "/metrics")
	fmt.Fprintf(os.Stderr, "    (default %q), until SIGINT/SIGTERM. No real telephony transport is\n", defaultDashboardAddr)
	fmt.Fprintln(os.Stderr, "    attached; see examples/vsip_example for the integration contract a")
	fmt.Fprintln(os.Stderr, "    future transport would fill in.")
	fmt.Fprintf(os.Stderr, "    available backends: asr=%v mt=%v tts=%v\n",
		langstream.AvailableASRBackends(),
		langstream.AvailableTranslatorBackends(),
		langstream.AvailableTTSBackends())
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "duplex --caller-listen ADDR --caller-forward ADDR --agent-listen ADDR --agent-forward ADDR [--backend NAME]")
	fmt.Fprintln(os.Stderr, "    Start a real, long-running duplex Session (same backend selection as demo/")
	fmt.Fprintln(os.Stderr, "    serve) bridged to two real ClearStream RTP legs (see pkg/rtp.DuplexSession):")
	fmt.Fprintln(os.Stderr, "    caller-facing and agent-facing UDP sockets, each with its own listen and")
	fmt.Fprintln(os.Stderr, "    forward address, until SIGINT/SIGTERM. Also mounts the observability")
	fmt.Fprintln(os.Stderr, "    dashboard on --addr (default matches serve's; pass --addr \"\" to disable it).")
	fmt.Fprintln(os.Stderr, "    Other flags: --caller-lang/--agent-lang (default hi/en), --caller-payload-type/")
	fmt.Fprintln(os.Stderr, "    --agent-payload-type, --caller-jitter-depth/--agent-jitter-depth, and")
	fmt.Fprintf(os.Stderr, "    --suppressor (ClearStream noise-suppressor backend, default %q).\n", defaultSuppressorBackend)
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

// newSession resolves the named ASR/MT/TTS backends through pkg/langstream's
// backend registry and constructs a langstream.Session configured for
// callerLang/agentLang. It is the shared backend-resolution +
// session-construction path used by both runDemo (a one-shot smoke test)
// and runServe (a long-running Session with the observability dashboard
// mounted in front of it), so the two subcommands don't duplicate the
// registry lookups or SessionConfig wiring.
func newSession(ctx context.Context, asrName, mtName, ttsName string, callerLang, agentLang langstream.Language) (*langstream.Session, error) {
	rec, err := langstream.NewASRBackend(asrName)
	if err != nil {
		return nil, fmt.Errorf("selecting ASR backend: %w", err)
	}
	tr, err := langstream.NewTranslatorBackend(mtName)
	if err != nil {
		return nil, fmt.Errorf("selecting MT backend: %w", err)
	}
	syn, err := langstream.NewTTSBackend(ttsName)
	if err != nil {
		return nil, fmt.Errorf("selecting TTS backend: %w", err)
	}

	cfg := langstream.SessionConfig{
		CallerLanguage: callerLang,
		AgentLanguage:  agentLang,
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}
	return sess, nil
}

// runDemo wires a langstream.Session up with backends selected by name via
// the --backend flag / LANGSTREAM_*_BACKEND env vars (see resolveBackend
// and newSession), looked up in pkg/langstream's backend registry, pushes
// one frame of "caller audio", and prints the synthesized audio the agent
// leg produces in response - a minimal, dependency-free proof that both
// the duplex orchestrator wiring and the backend-selection registry work
// end to end.
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newSession(ctx, asrName, mtName, ttsName, "hi", "en")
	if err != nil {
		return err
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

// runServe starts a long-running langstream.Session (backends selected the
// same way runDemo does, see newSession) and mounts
// observability.NewDashboardServer in front of that Session's metrics
// recorder (Session.Metrics), listening on --addr (default
// defaultDashboardAddr) until SIGINT/SIGTERM, then shuts both the HTTP
// server and the Session down gracefully.
//
// No real telephony transport is attached to the Session here: nothing
// calls PushCallerAudio/PushAgentAudio, so the dashboard will report an
// idle Session until a real transport (or a test/demo harness) pushes
// audio into it. See examples/vsip_example for the shape a future Exotel
// vSIP integration would use to do that, and DEVLOG.md's 2026-07-08/09
// entries for why that transport itself is still blocked on a ClearStream
// decision, unrelated to this subcommand.
//
// Construction (buildServeSession) and lifecycle (runServeWithContext) are
// split the same way duplex.go splits buildDuplexSession/
// runDuplexWithContext, both for testability (see serve_shutdown_test.go)
// and because it's what makes the shutdown-ordering fix below possible to
// express clearly.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	backend := fs.String("backend", "", `backend name for ASR, MT, and TTS alike (default "mock")`)
	addr := fs.String("addr", defaultDashboardAddr, "address for the observability dashboard HTTP server to listen on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	asrName := resolveBackend(*backend, envASRBackend)
	mtName := resolveBackend(*backend, envMTBackend)
	ttsName := resolveBackend(*backend, envTTSBackend)

	// signal.NotifyContext gives us a context that's cancelled the moment
	// SIGINT/SIGTERM arrives. Unlike an earlier version of this function,
	// this ctx is used ONLY to decide *when* to start shutting down
	// (serveDashboard's own wait, and the <-ctx.Done() in
	// runServeWithContext below) -- it is deliberately not propagated into
	// the Session itself. See buildServeSession's doc comment for why.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, srv, err := buildServeSession(asrName, mtName, ttsName, *addr)
	if err != nil {
		return err
	}

	fmt.Printf("langstream serve: backends asr=%s mt=%s tts=%s\n", asrName, mtName, ttsName)
	fmt.Printf("langstream serve: dashboard listening on %s (/, /dashboard.json, /metrics)\n", *addr)
	fmt.Println("langstream serve: press Ctrl+C to stop")

	return runServeWithContext(ctx, sess, srv)
}

// buildServeSession resolves the named ASR/MT/TTS backends (see newSession)
// and constructs both the langstream.Session and the
// observability.NewDashboardServer mounted in front of its metrics
// recorder, ready for runServeWithContext to run.
//
// The Session is deliberately constructed against context.Background(),
// NOT any SIGINT/SIGTERM-cancelling context a caller might otherwise have
// on hand -- this is the exact fix for the bug class already found and
// fixed in pkg/rtp/duplex.go's buildDuplexSession (see that function's own
// doc comment, and DEVLOG.md's 2026-07-13 entry, "Bugs found/fixed" item
// 3, for the original writeup). langstream.NewSession derives the
// Session's *entire* internal lifecycle context from whatever ctx it's
// given, and every blocking operation inside Session -- including the
// translate/synthesize work Session.Close() waits to drain during its
// final-utterance flush (see session.go's runLeg, guarded throughout by a
// select on that same context being Done) -- reacts to it immediately.
// Constructing the Session against the shutdown-signal context directly
// would mean the instant SIGINT/SIGTERM arrives, Session's internal
// goroutines would already see their context cancelled and abandon any
// in-flight flush before runServeWithContext's later, explicit sess.Close()
// call ever gets a chance to drain it -- silently defeating graceful
// shutdown, exactly like the duplex.go bug did before its fix. Session's
// actual lifecycle is instead governed entirely by the explicit Close()
// call in runServeWithContext.
func buildServeSession(asrName, mtName, ttsName, addr string) (*langstream.Session, *http.Server, error) {
	sess, err := newSession(context.Background(), asrName, mtName, ttsName, "hi", "en")
	if err != nil {
		return nil, nil, err
	}
	srv := observability.NewDashboardServer(addr, sess.Metrics())
	return sess, srv, nil
}

// runServeWithContext blocks until ctx is cancelled, then shuts sess and
// srv down: it closes sess first (flushing any in-flight final utterance
// through the still-live translate/synthesize pipeline -- safe to do
// because buildServeSession built sess against context.Background(), not
// ctx -- bounded by Session's own finalFlushTimeout, see session.go's
// Close doc comment) while srv's own graceful shutdown (serveDashboard,
// bounded to 5s) runs concurrently in its own goroutine, since the two
// components don't interact: nothing in `serve` calls
// PushCallerAudio/PushAgentAudio or reads AgentHearsAudio()/
// CallerHearsAudio() (see runServe's doc comment -- no real telephony
// transport is attached here), so unlike `duplex` (see duplex.go's
// duplexFinalDrainGrace, needed there because a flushed chunk must still
// be pulled through the TTS-pacing buffer and injected over a live RTP leg
// *after* sess.Close() returns) there is no separate external consumer
// that additionally needs wall-clock time to drain a channel after
// Session.Close() has already returned -- Close() itself only returns
// once the flush has actually completed (or timed out), so no extra fixed
// grace-period sleep is added here; it would add dead time, not
// correctness.
//
// Split out from runServe (mirroring runDuplexWithContext) so it can be
// exercised directly in tests against a real Session that's had audio
// pushed into it, without going through flag parsing, backend resolution,
// or OS signal delivery.
func runServeWithContext(ctx context.Context, sess *langstream.Session, srv *http.Server) error {
	dashboardErr := make(chan error, 1)
	go func() {
		dashboardErr <- serveDashboard(ctx, srv)
	}()

	<-ctx.Done()

	var firstErr error
	if err := sess.Close(); err != nil {
		firstErr = fmt.Errorf("closing session: %w", err)
	}

	if err := <-dashboardErr; err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

// serveDashboard runs srv (via ListenAndServe) until ctx is cancelled, then
// shuts it down gracefully (bounded by a fixed 5-second grace period) and
// returns. It returns a non-nil error only if ListenAndServe failed to
// start listening at all (e.g. the address is already in use) or if
// Shutdown itself failed to complete within its grace period; a normal
// shutdown triggered by ctx cancellation returns nil.
//
// Split out from runServe so it can be exercised directly in tests without
// going through flag parsing, backend resolution, or OS signal delivery.
func serveDashboard(ctx context.Context, srv *http.Server) error {
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		// Fall through to the graceful-shutdown path below.
	case err := <-serveErr:
		// The server stopped on its own (most likely a listen error)
		// before ctx was ever cancelled.
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down dashboard server: %w", err)
	}

	return <-serveErr
}
