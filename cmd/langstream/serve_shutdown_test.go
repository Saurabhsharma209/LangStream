// serve_shutdown_test.go covers the shutdown-ordering bug fixed in
// runServe/buildServeSession/runServeWithContext (main.go): the exact same
// bug class already found and fixed in pkg/rtp/duplex.go's
// buildDuplexSession on 2026-07-13 (see DEVLOG.md's 2026-07-13 entry,
// "Bugs found/fixed" item 3). Before the fix, `serve` constructed its
// langstream.Session against the same context signal.NotifyContext
// cancels on SIGINT/SIGTERM, so the instant that signal arrived, the
// Session's internal translate/synthesize goroutines (guarded throughout
// by a select on that same context, see session.go's runLeg) would
// abandon an in-flight final-utterance flush before the deferred
// sess.Close() call ever got a chance to drain it -- silently dropping
// the last thing said on the call.
//
// TestRunServeWithContext_GracefulShutdownFlushesInFlightUtterance below
// reproduces exactly that scenario end to end through the real,
// currently-shipped code path (buildServeSession + runServeWithContext,
// the same construction/lifecycle split `runServe` itself uses): it
// builds a real Session, pushes one buffered (not yet flushed -- see
// asr.MockRecognizer's doc comment, and runDemo's own use of the same
// pattern) caller-audio frame, starts runServeWithContext exactly as
// `serve` would, cancels ctx mid-utterance (simulating SIGINT/SIGTERM),
// and asserts the flushed, translated, synthesized chunk actually arrives
// on AgentHearsAudio() -- i.e. that shutdown drained it -- rather than the
// channel closing with nothing delivered.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
)

// TestRunServeWithContext_GracefulShutdownFlushesInFlightUtterance is
// `serve`'s own lifecycle test, mirroring
// TestRunDuplexWithContext_RealLoopbackEndToEnd in duplex_test.go one
// layer down (there is no RTP transport in `serve` to push real packets
// through -- see runServe's doc comment -- so audio is pushed directly
// into the Session instead, same as runDemo does).
func TestRunServeWithContext_GracefulShutdownFlushesInFlightUtterance(t *testing.T) {
	addr := freeAddr(t)

	sess, srv, err := buildServeSession(langstream.BackendMock, langstream.BackendMock, langstream.BackendMock, addr)
	if err != nil {
		t.Fatalf("buildServeSession: %v", err)
	}

	// A small frame that pkg/asr's MockRecognizer only buffers rather than
	// immediately emitting a transcript for (see runDemo's doc comment on
	// the same pattern) -- so this utterance is genuinely "in flight" and
	// only gets flushed as a final transcript once something closes the
	// ASR stream, exactly the "caller hangs up mid-utterance" case this
	// bug silently dropped.
	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() { runDone <- runServeWithContext(ctx, sess, srv) }()

	// Wait for the dashboard to come up as proof runServeWithContext (and
	// its internal serveDashboard goroutine) has actually started, mirroring
	// duplex_test.go's own use of waitForServer for the same purpose.
	waitForServer(t, "http://"+addr+"/metrics")

	// Simulate SIGINT/SIGTERM arriving mid-utterance: this is the exact
	// moment the pre-fix code would have already cancelled the Session's
	// own internal context (because it was constructed against ctx
	// directly), abandoning the flush below before sess.Close() ever ran.
	cancel()

	// If the bug were still present, AgentHearsAudio() would either
	// deliver nothing before closing, or close outright without ever
	// producing the flushed chunk. With the fix (Session built against
	// context.Background() in buildServeSession, only ever cancelled by
	// runServeWithContext's own explicit, later sess.Close() call), the
	// final transcript's translated+synthesized audio must still arrive.
	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			t.Fatal("AgentHearsAudio() closed without ever delivering the flushed final-utterance chunk - utterance was dropped on shutdown")
		}
		if len(chunk.PCM) == 0 {
			t.Error("flushed chunk has 0 bytes of PCM; expected synthesized audio")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the flushed final-utterance chunk on AgentHearsAudio() - utterance was dropped on shutdown")
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runServeWithContext returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServeWithContext did not return within 10s of context cancellation")
	}
}

// TestBuildServeSession_UnknownBackendFails confirms buildServeSession
// still surfaces backend-resolution errors (e.g. an unregistered ASR
// backend name) the same way newSession itself does, now that
// construction has been split out of runServe.
func TestBuildServeSession_UnknownBackendFails(t *testing.T) {
	if _, _, err := buildServeSession("does-not-exist", langstream.BackendMock, langstream.BackendMock, freeAddr(t)); err == nil {
		t.Fatal("expected an error for an unregistered ASR backend")
	}
}

// TestRunServeWithContext_NoActivity_ShutsDownCleanly confirms the normal,
// no-in-flight-utterance shutdown path (already covered at the process
// level by TestServeCommand_RealBinary_EndToEnd in
// serve_integration_test.go) also returns nil promptly at this layer,
// i.e. the fix doesn't introduce a hang when there is nothing to flush.
func TestRunServeWithContext_NoActivity_ShutsDownCleanly(t *testing.T) {
	sess, srv, err := buildServeSession(langstream.BackendMock, langstream.BackendMock, langstream.BackendMock, freeAddr(t))
	if err != nil {
		t.Fatalf("buildServeSession: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runServeWithContext(ctx, sess, srv) }()

	// Give the dashboard goroutine a moment to actually start listening
	// before triggering shutdown.
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runServeWithContext returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServeWithContext did not return within 10s of context cancellation")
	}
}
