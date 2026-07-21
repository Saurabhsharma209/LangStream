// Package langstream_test - QA's Sprint 2026-07-21 integration coverage
// for Tech's dead-leg-drain fix (pkg/langstream/session.go's
// drainDeadLeg, spawned by runLeg's `tr, ok := <-transcripts` branch the
// moment a leg's ASR StreamSession permanently dies mid-call, plus
// pkg/langstream/fallback.go's FallbackConfig.DeadLegDrainInterval and
// reasonASRStreamClosedPassthrough).
//
// Before today's fix (see asr_permanent_failure_integration_test.go's
// package doc comment for last sprint's fix, and DEVLOG.md's 2026-07-20
// entry documenting the gap it deliberately left open), a leg whose ASR
// stream died permanently mid-call got exactly one last drain-and-forward
// of whatever was already buffered, then went dark for good: any audio
// pushed to that leg afterward sat forgotten in leg.audio's ring buffer
// until Session.Close() finally tore the whole session down. Tech's own
// unit tests (pkg/langstream/session_test.go's
// TestSessionDrainDeadLegForwardsAudioPushedAfterASRStreamCloses/
// TestSessionDrainDeadLegStopsCleanlyOnCloseNoGoroutineLeak/
// TestSessionNormalCloseDoesNotSpawnDeadLegDrainer) already prove the
// fix against Tech's own hand-rolled, unexported, package-internal fakes,
// and already cover ONE subsequent push landing as passthrough.
//
// This file re-drives the same trigger through the real, exported
// langstream.Session API from an external test package (same convention
// as asr_permanent_failure_integration_test.go and
// langstream_integration_test.go's package doc comments), reusing that
// file's permaFailRecognizer/permaFailStream/permaFailSession test
// doubles (same package, permaFailStream.PushAudio never errors so every
// PushCallerAudio call here succeeds regardless of the leg's state - see
// PushCallerAudio's own doc comment for why the raw-audio buffering
// happens unconditionally before the backend is ever consulted). What it
// adds beyond Tech's own tests: (1) MULTIPLE separate
// Push{Caller,Agent}Audio calls arriving over time after the leg has
// already died - not just the single follow-up push Tech's own test
// exercises - each asserted to individually reach the listening party as
// passthrough, simulating a caller who keeps talking well after their leg
// silently died; and (2) confirming Session.Close() still terminates
// promptly and cleanly afterward, with no goroutine leak, across many
// repeated cycles.
package langstream_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// newPermaFailSessionWithFallback is newPermaFailSession's (see
// asr_permanent_failure_integration_test.go) sibling: it additionally
// takes a FallbackConfig so this file's tests can set a short
// DeadLegDrainInterval (rather than the 300ms production default) and
// wire in their own *observability.LatencyRecorder to assert on
// reasonASRStreamClosedPassthrough counts, the same way
// pkg/langstream/session_test.go's own drainDeadLeg tests do.
func newPermaFailSessionWithFallback(t *testing.T, ctx context.Context, rec *permaFailRecognizer, fallback langstream.FallbackConfig) *langstream.Session {
	t.Helper()
	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
		Fallback:       fallback,
	}
	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}

// drainUntilPayloadAndFinal drains ch until it has seen a chunk whose PCM
// exactly matches want, and then continues draining until (and including)
// the next IsFinal chunk, so the caller can be sure the entire flush that
// carried want has been fully consumed before doing anything else (e.g.
// pushing more audio, which this file's tests rely on to force each
// subsequent push through its own, separately-observable drainDeadLeg
// poll cycle rather than potentially coalescing with a later one).
func drainUntilPayloadAndFinal(t *testing.T, ch <-chan tts.AudioChunk, want []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	sawPayload := false
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("audio channel closed unexpectedly before the expected passthrough payload arrived")
			}
			if string(chunk.PCM) == string(want) {
				sawPayload = true
			}
			if chunk.IsFinal {
				if !sawPayload {
					t.Fatalf("saw a final passthrough chunk but never the expected payload %v", want)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for passthrough payload %v (and its terminating final chunk)", want)
		}
	}
}

// TestDeadLegDrain_MultipleSubsequentPushesOverTimeAllForwardedAsPassthrough
// drives a real *langstream.Session through a permanent caller-leg ASR
// failure, then simulates a caller who keeps talking well after their leg
// silently died: three separate PushCallerAudio calls, each made only
// after the previous one's passthrough has been fully observed on
// AgentHearsAudio(), so each necessarily crosses its own
// DeadLegDrainInterval poll boundary rather than all landing in one
// flush. Every single one must still reach the agent, not just the first
// - the exact gap beyond Tech's own unit test that this file exists to
// close - and CallerLegDegraded()/AgentLegDegraded() must reflect only
// the caller leg throughout.
func TestDeadLegDrain_MultipleSubsequentPushesOverTimeAllForwardedAsPassthrough(t *testing.T) {
	rec := &permaFailRecognizer{}
	metrics := observability.NewLatencyRecorder()
	fallback := langstream.FallbackConfig{
		Metrics:              metrics,
		DeadLegDrainInterval: 40 * time.Millisecond,
	}
	sess := newPermaFailSessionWithFallback(t, context.Background(), rec, fallback)
	defer sess.Close()

	firstFrame := asr.AudioFrame{PCM: []byte{9, 8, 7, 6, 5}, SampleRate: 8000}
	if err := sess.PushCallerAudio(firstFrame); err != nil {
		t.Fatalf("PushCallerAudio (initial, pre-death): %v", err)
	}
	if rec.streamCount() < 1 {
		t.Fatal("expected permaFailRecognizer to have started at least the caller stream")
	}
	callerStream := rec.stream(0)

	// Kill the caller leg's ASR stream permanently, mid-call, independent
	// of Session.Close (never called before the end of this test).
	callerStream.simulateBackendDeath()

	// Drain the one-time, synchronous death-drain flush of firstFrame
	// (runLeg's own flush, unchanged by today's fix) before moving on to
	// what's actually under test here, so it can't be confused with the
	// drainDeadLeg-forwarded pushes that follow.
	drainUntilPayloadAndFinal(t, sess.AgentHearsAudio(), firstFrame.PCM, 3*time.Second)

	if !sess.CallerLegDegraded() {
		t.Fatal("expected CallerLegDegraded() to be true once the caller leg's ASR stream closed unprompted")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("the agent leg's ASR stream never closed; AgentLegDegraded() must remain false")
	}

	// Now simulate the caller continuing to talk after their leg silently
	// died: three separate, distinctly-payloaded pushes, each fully
	// drained (including its own IsFinal passthrough chunk) before the
	// next is pushed, so each one necessarily forces its own
	// drainDeadLeg poll cycle rather than all coalescing into a single
	// flush.
	followUpFrames := [][]byte{
		{101, 102, 103},
		{201, 202, 203, 204},
		{55, 66, 77, 88, 99, 111},
	}
	for i, pcm := range followUpFrames {
		frame := asr.AudioFrame{PCM: pcm, SampleRate: 8000}
		if err := sess.PushCallerAudio(frame); err != nil {
			t.Fatalf("PushCallerAudio (post-death follow-up #%d): %v", i, err)
		}
		drainUntilPayloadAndFinal(t, sess.AgentHearsAudio(), pcm, 3*time.Second)
	}

	// Every one of the three follow-up pushes above must have produced
	// its own recorded reasonASRStreamClosedPassthrough event - if
	// drainDeadLeg silently stopped polling after the first cycle (a
	// regression this integration test exists specifically to catch,
	// since Tech's own unit test only ever pushes one follow-up frame),
	// this count would be short.
	if got := metrics.ReasonCount("leg_degraded", "caller", "asr_stream_closed_passthrough"); int(got) < len(followUpFrames) {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed_passthrough) = %d, want >= %d (one per post-death follow-up push)", got, len(followUpFrames))
	}
	// The one-time reasonASRStreamClosed event must still have fired
	// exactly once, distinct from the ongoing per-push passthrough
	// events checked above.
	if got := metrics.ReasonCount("leg_degraded", "caller", "asr_stream_closed"); got != 1 {
		t.Fatalf("ReasonCount(leg_degraded, caller, asr_stream_closed) = %d, want 1", got)
	}

	if !sess.CallerLegDegraded() {
		t.Fatal("CallerLegDegraded() must remain true after the follow-up pushes")
	}
	if sess.AgentLegDegraded() {
		t.Fatal("AgentLegDegraded() must remain false - only the caller leg's ASR stream ever died")
	}

	// Close must still shut the session down cleanly (and promptly - well
	// under the 3s finalFlushTimeout backstop drainDeadLeg's doc comment
	// warns a shutdown-ordering bug would force every such Close() to
	// eat): the caller leg's original runLeg goroutine already exited on
	// its own, its replacement drainDeadLeg goroutine must notice
	// s.closing/s.ctx and exit too, and the agent leg (whose ASR stream
	// never died) must still shut down normally.
	closeStart := time.Now()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
	if elapsed := time.Since(closeStart); elapsed > 1*time.Second {
		t.Fatalf("Close() took %v after a dead-leg drain cycle, want well under the 3s finalFlushTimeout backstop - suggests the shutdown-ordering fix (s.closing check in drainDeadLeg) has regressed", elapsed)
	}

	if _, ok := <-sess.AgentHearsAudio(); ok {
		t.Fatal("AgentHearsAudio not closed after Close")
	}
	if _, ok := <-sess.CallerHearsAudio(); ok {
		t.Fatal("CallerHearsAudio not closed after Close")
	}
}

// TestDeadLegDrain_AgentLegSymmetricMultiplePushes is the caller-leg
// test's mirror: the agent leg's ASR stream dies instead, and multiple
// PushAgentAudio calls arrive afterward. Proves the multi-push drain
// behavior is symmetric across legs (session.go's runLeg/drainDeadLeg are
// parameterized identically for both) and that it is CallerHearsAudio()
// specifically that carries the resulting passthrough.
func TestDeadLegDrain_AgentLegSymmetricMultiplePushes(t *testing.T) {
	rec := &permaFailRecognizer{}
	fallback := langstream.FallbackConfig{DeadLegDrainInterval: 40 * time.Millisecond}
	sess := newPermaFailSessionWithFallback(t, context.Background(), rec, fallback)
	defer sess.Close()

	firstFrame := asr.AudioFrame{PCM: []byte{1, 2, 3, 4}, SampleRate: 8000}
	if err := sess.PushAgentAudio(firstFrame); err != nil {
		t.Fatalf("PushAgentAudio (initial, pre-death): %v", err)
	}
	if rec.streamCount() < 2 {
		t.Fatal("expected permaFailRecognizer to have started both caller and agent streams")
	}
	agentStream := rec.stream(1)
	agentStream.simulateBackendDeath()

	drainUntilPayloadAndFinal(t, sess.CallerHearsAudio(), firstFrame.PCM, 3*time.Second)

	if !sess.AgentLegDegraded() {
		t.Fatal("expected AgentLegDegraded() to be true once the agent leg's ASR stream closed unprompted")
	}
	if sess.CallerLegDegraded() {
		t.Fatal("the caller leg's ASR stream never closed; CallerLegDegraded() must remain false")
	}

	followUpFrames := [][]byte{
		{11, 22, 33},
		{44, 55, 66, 77},
	}
	for i, pcm := range followUpFrames {
		frame := asr.AudioFrame{PCM: pcm, SampleRate: 8000}
		if err := sess.PushAgentAudio(frame); err != nil {
			t.Fatalf("PushAgentAudio (post-death follow-up #%d): %v", i, err)
		}
		drainUntilPayloadAndFinal(t, sess.CallerHearsAudio(), pcm, 3*time.Second)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
}

// TestDeadLegDrain_RepeatedMultiPushCyclesNoGoroutineLeak drives the same
// permanent-ASR-failure-plus-multiple-subsequent-pushes scenario
// repeatedly and confirms Session.Close() always shuts every resulting
// drainDeadLeg goroutine down cleanly with no leak, following the same
// repeated-iteration goroutine-count-check shape as
// asr_permanent_failure_integration_test.go's
// TestASRPermanentFailure_NoGoroutineLeak and
// pkg/langstream/session_test.go's
// TestSessionDrainDeadLegStopsCleanlyOnCloseNoGoroutineLeak - but, unlike
// either of those, pushing multiple (not just zero or one) follow-up
// frames per iteration, so any leak specific to drainDeadLeg surviving
// multiple poll cycles (rather than just one) would show up here.
func TestDeadLegDrain_RepeatedMultiPushCyclesNoGoroutineLeak(t *testing.T) {
	settleGoroutines(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 15; i++ {
		rec := &permaFailRecognizer{}
		fallback := langstream.FallbackConfig{DeadLegDrainInterval: 15 * time.Millisecond}
		sess := newPermaFailSessionWithFallback(t, context.Background(), rec, fallback)

		if err := sess.PushCallerAudio(asr.AudioFrame{PCM: []byte{1, 2, 3}, SampleRate: 8000}); err != nil {
			t.Fatalf("PushCallerAudio: %v", err)
		}
		rec.stream(0).simulateBackendDeath()
		drainUntilPayloadAndFinal(t, sess.AgentHearsAudio(), []byte{1, 2, 3}, 3*time.Second)

		for j, pcm := range [][]byte{{4, 5, 6}, {7, 8, 9}, {10, 11, 12}} {
			frame := asr.AudioFrame{PCM: pcm, SampleRate: 8000}
			if err := sess.PushCallerAudio(frame); err != nil {
				t.Fatalf("iteration %d, follow-up push %d: PushCallerAudio: %v", i, j, err)
			}
			drainUntilPayloadAndFinal(t, sess.AgentHearsAudio(), pcm, 3*time.Second)
		}

		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	settleGoroutines(t)
	after := runtime.NumGoroutine()

	if after > before+4 {
		t.Fatalf("possible goroutine leak from drainDeadLeg after repeated multi-push ASR-permanent-failure cycles: before=%d after=%d", before, after)
	}
}
