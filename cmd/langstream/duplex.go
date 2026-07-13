// duplex.go implements the `langstream duplex` subcommand: the concrete
// next step called out as "Tomorrow" in DEVLOG.md's 2026-07-12 entry --
// wiring pkg/rtp.DuplexSession (a working, tested bridge between two
// ClearStream rtp.Session legs and a langstream.Session, see
// pkg/rtp/duplex.go) into this CLI's flag/lifecycle plumbing, so a real
// (or loopback, for local testing) UDP RTP transport can actually drive a
// live Session instead of only being exercised by pkg/rtp's own tests and
// examples/vsip_example.
//
// `duplex` deliberately reuses `demo`/`serve`'s existing backend-selection
// path (resolveBackend/newSession) unchanged: which ASR/MT/TTS backend is
// behind the Session is orthogonal to how RTP gets in and out of it, and
// the "mock" default keeps this runnable without any vendor credentials
// (see ROADMAP.md's Week 2 decision).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/rtp"
)

// defaultSuppressorBackend is the model.SuppressorConfig.Backend `duplex`
// uses unless --suppressor overrides it. "passthrough" (no-op, no CGo/ONNX
// dependency) is the safe default for a CLI/example entrypoint that may
// run in environments without RNNoise's native library or a DeepFilterNet
// ONNX model available; a real deployment would typically override this
// with "rnnoise" (ClearStream's default when Backend is left empty) once
// that dependency is known to be present, e.g. `--suppressor rnnoise`.
const defaultSuppressorBackend = "passthrough"

// duplexShutdownGrace bounds how long `duplex` waits, after ctx is
// cancelled (SIGINT/SIGTERM), for DuplexSession.Stop and the dashboard
// server's graceful shutdown to both complete before returning -- mirrors
// duplexStopTimeout's own "bounded wait, don't hang forever on shutdown"
// reasoning inside pkg/rtp itself (see duplex.go), applied one layer up.
const duplexShutdownGrace = 5 * time.Second

// duplexFinalDrainGrace bounds how long runDuplexWithContext waits, after
// sess.Close() returns but before calling duplex.Stop(), for any utterance
// sess.Close() just flushed (translate+synthesize completed,
// AgentHearsAudio()/CallerHearsAudio() closed -- see
// langstream.Session.Close's doc comment) to actually drain through the
// per-leg TTS-pacing buffer and reach InjectBotAudio.
//
// This ordering (Session closed and given a moment to drain *before* the
// RTP legs are stopped) matters: DuplexSession.Stop cancels the context
// its bridging goroutines (including the TTS pacer's runTTSPacer) select
// on, so calling it too early would abandon a final flushed chunk
// mid-flight even though runTTSPacer is deliberately written not to exit
// merely because its feed channel closed (see duplex.go's runTTSPacer doc
// comment) -- that protection only helps if Stop() is called after enough
// time has passed for the chunk to actually have been pulled and
// injected. 250ms is comfortably above the default TTS-pacing tick
// (rtp.DefaultTTSPacingInterval, 20ms) and ClearStream's own downstream
// playback-loop tick (also 20ms), leaving slack for scheduling jitter.
const duplexFinalDrainGrace = 250 * time.Millisecond

// duplexConfig holds `duplex`'s fully-parsed/resolved flag values, split
// out from flag.FlagSet parsing itself so runDuplexWithContext can be
// exercised directly in tests (see duplex_test.go) without going through
// os.Args or OS signal delivery, the same pattern serveDashboard already
// uses relative to runServe.
type duplexConfig struct {
	asrBackend, mtBackend, ttsBackend string
	callerLang, agentLang             string

	callerListen, callerForward string
	agentListen, agentForward   string
	callerPayloadType           uint
	agentPayloadType            uint
	callerJitterDepth           int
	agentJitterDepth            int

	suppressorBackend string

	dashboardAddr string // "" disables the dashboard entirely
}

// parseDuplexFlags parses args into a duplexConfig, resolving the
// --backend/env-var-driven ASR/MT/TTS selection the same way runDemo and
// runServe do (see resolveBackend), and validates that the four RTP
// addresses `duplex` cannot function without are actually set.
func parseDuplexFlags(args []string) (duplexConfig, error) {
	fs := flag.NewFlagSet("duplex", flag.ContinueOnError)
	backend := fs.String("backend", "", `backend name for ASR, MT, and TTS alike (default "mock")`)
	callerLang := fs.String("caller-lang", "hi", "language the caller leg speaks/hears")
	agentLang := fs.String("agent-lang", "en", "language the agent leg speaks/hears")

	callerListen := fs.String("caller-listen", "", `UDP address the caller leg listens for inbound RTP on (e.g. "0.0.0.0:5004")  [required]`)
	callerForward := fs.String("caller-forward", "", "UDP address the caller leg forwards denoised/injected RTP to [required]")
	agentListen := fs.String("agent-listen", "", `UDP address the agent leg listens for inbound RTP on (e.g. "0.0.0.0:5006")  [required]`)
	agentForward := fs.String("agent-forward", "", "UDP address the agent leg forwards denoised/injected RTP to [required]")

	callerPayloadType := fs.Uint("caller-payload-type", 0, "RTP payload type the caller leg's telephony side uses (e.g. 0 for PCMU)")
	agentPayloadType := fs.Uint("agent-payload-type", 0, "RTP payload type the agent leg's telephony side uses (e.g. 0 for PCMU)")
	callerJitterDepth := fs.Int("caller-jitter-depth", 0, "ClearStream's own inbound jitter buffer depth for the caller leg (0 = ClearStream default)")
	agentJitterDepth := fs.Int("agent-jitter-depth", 0, "ClearStream's own inbound jitter buffer depth for the agent leg (0 = ClearStream default)")

	suppressor := fs.String("suppressor", defaultSuppressorBackend, `ClearStream noise-suppressor backend for both legs ("rnnoise", "deepfilter", or "passthrough")`)

	addr := fs.String("addr", defaultDashboardAddr, `address for the observability dashboard HTTP server to listen on ("" disables it)`)

	if err := fs.Parse(args); err != nil {
		return duplexConfig{}, err
	}

	var missing []string
	for name, v := range map[string]string{
		"--caller-listen":  *callerListen,
		"--caller-forward": *callerForward,
		"--agent-listen":   *agentListen,
		"--agent-forward":  *agentForward,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return duplexConfig{}, fmt.Errorf("duplex: missing required flag(s): %v", missing)
	}

	return duplexConfig{
		asrBackend: resolveBackend(*backend, envASRBackend),
		mtBackend:  resolveBackend(*backend, envMTBackend),
		ttsBackend: resolveBackend(*backend, envTTSBackend),
		callerLang: *callerLang,
		agentLang:  *agentLang,

		callerListen:  *callerListen,
		callerForward: *callerForward,
		agentListen:   *agentListen,
		agentForward:  *agentForward,

		callerPayloadType: *callerPayloadType,
		agentPayloadType:  *agentPayloadType,
		callerJitterDepth: *callerJitterDepth,
		agentJitterDepth:  *agentJitterDepth,

		suppressorBackend: *suppressor,

		dashboardAddr: *addr,
	}, nil
}

// buildDuplexSession resolves cfg's backends/suppressors and constructs
// (but does not Start) both the langstream.Session and the
// *rtp.DuplexSession bridging it to two real ClearStream RTP legs, per
// cfg's addresses. Split out from runDuplexWithContext so tests can
// inspect/drive construction failures (e.g. an invalid address) without
// needing a full lifecycle run.
//
// The langstream.Session is deliberately constructed against
// context.Background(), not the ctx parameter (which, in runDuplex, is
// the SIGINT/SIGTERM-cancelled context that also gates when
// runDuplexWithContext decides to shut down): langstream.NewSession
// derives the Session's *entire* internal lifecycle context from whatever
// ctx it's given (see session.go's sessCtx), and every blocking operation
// inside Session -- including the translate/synthesize work
// Session.Close() waits to drain during its final-utterance flush -- is
// guarded by a select on that same context being done. Constructing the
// Session against the shutdown-signal ctx directly would mean the instant
// SIGINT/SIGTERM arrives, Session's internal goroutines would already see
// their context cancelled and abandon any in-flight flush before
// runDuplexWithContext's later, explicit sess.Close() call ever gets a
// chance to gracefully drain it -- silently defeating the very shutdown
// ordering duplexFinalDrainGrace exists for. ctx is still accepted here
// (kept symmetric with runDuplexWithContext's signature, and available for
// a future construction-time deadline if one is ever needed) but is not
// propagated into the Session itself; the Session's actual lifecycle is
// governed entirely by explicit Close() calls instead.
func buildDuplexSession(ctx context.Context, cfg duplexConfig) (*rtp.DuplexSession, *langstream.Session, error) {
	_ = ctx // see doc comment above: intentionally not passed to newSession.
	sess, err := newSession(context.Background(), cfg.asrBackend, cfg.mtBackend, cfg.ttsBackend, langstream.Language(cfg.callerLang), langstream.Language(cfg.agentLang))
	if err != nil {
		return nil, nil, fmt.Errorf("duplex: %w", err)
	}

	callerSuppressor, err := model.NewSuppressor(model.SuppressorConfig{Backend: cfg.suppressorBackend})
	if err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("duplex: constructing caller leg suppressor (backend %q): %w", cfg.suppressorBackend, err)
	}
	agentSuppressor, err := model.NewSuppressor(model.SuppressorConfig{Backend: cfg.suppressorBackend})
	if err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("duplex: constructing agent leg suppressor (backend %q): %w", cfg.suppressorBackend, err)
	}

	logger, err := zap.NewProduction()
	if err != nil {
		logger = zap.NewNop()
	}

	duplex, err := rtp.NewDuplexSession(rtp.DuplexConfig{
		CallerLeg: rtp.LegConfig{
			ListenAddr:  cfg.callerListen,
			ForwardAddr: cfg.callerForward,
			PayloadType: uint8(cfg.callerPayloadType),
			JitterDepth: cfg.callerJitterDepth,
			Suppressor:  callerSuppressor,
			Logger:      logger.Named("rtp.caller"),
		},
		AgentLeg: rtp.LegConfig{
			ListenAddr:  cfg.agentListen,
			ForwardAddr: cfg.agentForward,
			PayloadType: uint8(cfg.agentPayloadType),
			JitterDepth: cfg.agentJitterDepth,
			Suppressor:  agentSuppressor,
			Logger:      logger.Named("rtp.agent"),
		},
		Session: sess,
		Logger:  logger.Named("rtp.duplex"),
	})
	if err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("duplex: constructing DuplexSession: %w", err)
	}

	return duplex, sess, nil
}

// runDuplexWithContext builds a real DuplexSession per cfg, starts it (and,
// unless cfg.dashboardAddr is empty, the observability dashboard mounted
// in front of the underlying Session's metrics recorder, exactly as
// `serve` does), and blocks until ctx is cancelled, then stops everything
// in order: the Session first (flushing any in-flight utterance and
// giving it duplexFinalDrainGrace to drain through InjectBotAudio while
// the RTP legs are still up -- see that constant's doc comment), then the
// DuplexSession/RTP legs, then the dashboard server.
//
// It returns the first error encountered during either construction or
// shutdown; a normal run that's stopped cleanly via ctx cancellation
// returns nil.
func runDuplexWithContext(ctx context.Context, cfg duplexConfig) error {
	duplex, sess, err := buildDuplexSession(ctx, cfg)
	if err != nil {
		return err
	}

	var dashboardErr chan error
	var dashboardSrv = observability.NewDashboardServer(cfg.dashboardAddr, sess.Metrics())
	if cfg.dashboardAddr != "" {
		dashboardErr = make(chan error, 1)
		go func() {
			dashboardErr <- serveDashboard(ctx, dashboardSrv)
		}()
		fmt.Printf("langstream duplex: dashboard listening on %s (/, /dashboard.json, /metrics)\n", cfg.dashboardAddr)
	}

	duplex.Start()
	fmt.Printf("langstream duplex: caller leg %s <-> %s, agent leg %s <-> %s\n",
		cfg.callerListen, cfg.callerForward, cfg.agentListen, cfg.agentForward)
	fmt.Println("langstream duplex: running -- press Ctrl+C to stop")

	<-ctx.Done()

	// Close the Session first (flushing any in-flight utterance and
	// giving it duplexFinalDrainGrace to actually reach InjectBotAudio,
	// see that constant's doc comment) and only then stop the RTP legs --
	// the reverse order would cancel the bridging goroutines' context
	// before a final flushed chunk could be paced and injected.
	var firstErr error
	if err := sess.Close(); err != nil {
		firstErr = fmt.Errorf("closing session: %w", err)
	}
	time.Sleep(duplexFinalDrainGrace)
	if err := duplex.Stop(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("stopping DuplexSession: %w", err)
	}
	if dashboardErr != nil {
		select {
		case err := <-dashboardErr:
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("shutting down dashboard server: %w", err)
			}
		case <-time.After(duplexShutdownGrace):
			if firstErr == nil {
				firstErr = fmt.Errorf("dashboard server did not shut down within %s", duplexShutdownGrace)
			}
		}
	}

	return firstErr
}

// runDuplex is the `langstream duplex` subcommand entrypoint: it parses
// args, derives a context cancelled on SIGINT/SIGTERM (matching
// runServe's own signal-handling pattern), and delegates the actual
// construct/run/shutdown lifecycle to runDuplexWithContext.
func runDuplex(args []string) error {
	cfg, err := parseDuplexFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runDuplexWithContext(ctx, cfg)
}
