// Task 1 (2026-07-12 Tech-workstream sprint): wiring real per-stage
// latency instrumentation into Session.runLeg (see pkg/observability's
// package doc comment for the four stage names: "asr_first_chunk", "mt",
// "tts_first_chunk", "total"). These tests assert the wiring actually
// produces real samples for a live Session, not just mock/hand-populated
// LatencyRecorder data (the gap flagged in DEVLOG.md's 2026-07-10 entry).
package langstream

import (
	"context"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
)

// TestSessionRecordsRealLatencyMetrics drives a full, successful round
// trip (final ASR transcript -> Translate -> SynthesizeStream -> forwarded
// audio) and asserts every stage gets a real, non-zero sample -- including
// one ("mt") deliberately delayed by an artificial 30ms so the test can
// confirm the recorded value reflects a real measurement, not a
// hardcoded/zero placeholder.
func TestSessionRecordsRealLatencyMetrics(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "hello", Language: "hi", IsFinal: true}},
			{},
		},
	}
	translator := &slowTranslator{delay: 30 * time.Millisecond}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{1, 2, 3, 4}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	select {
	case chunk, ok := <-sess.AgentHearsAudio():
		if !ok {
			t.Fatal("AgentHearsAudio closed unexpectedly")
		}
		if !chunk.IsFinal {
			t.Fatal("expected a single final chunk from fakeSynthesizer, got IsFinal=false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for translated audio")
	}

	m := sess.Metrics()
	if got := m.Count("asr_first_chunk"); got == 0 {
		t.Error("Count(asr_first_chunk) = 0, want > 0 for a successful round trip")
	}
	if got := m.Count("mt"); got == 0 {
		t.Error("Count(mt) = 0, want > 0 for a successful round trip")
	}
	if got := m.Count("tts_first_chunk"); got == 0 {
		t.Error("Count(tts_first_chunk) = 0, want > 0 for a successful round trip")
	}

	// "total" is recorded by runLeg's `if completed` branch only after
	// forwardAudio's send to AgentHearsAudio() fully completes -- the
	// sending goroutine still has to return from forwardAudio and call
	// recordTotalIfStarted after this test's receive on AgentHearsAudio()
	// above already unblocked, so checking m.Count immediately is a real
	// (if narrow) race, not a synchronization point. Poll briefly instead
	// of asserting instantaneously -- same race class discovered during
	// 2026-07-14's integration verification for the passthrough variant
	// of this test, below.
	deadlineTotal := time.After(time.Second)
	for {
		if m.Count("total") > 0 {
			break
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-deadlineTotal:
			t.Fatal("Count(total) = 0 after 1s, want > 0 for a successful round trip")
		}
	}

	// The mt sample should reflect the artificial 30ms delay, not be a
	// near-zero placeholder -- proving this is a real measurement of the
	// Translate call rather than a stub value.
	if got := m.Percentile("mt", 50); got < 20 {
		t.Errorf("mt p50 = %.2fms, want at least ~20ms given the 30ms artificial translator delay", got)
	}
	// total spans the whole utterance including that delay, so it must be
	// at least as large.
	if got := m.Percentile("total", 50); got < 20 {
		t.Errorf("total p50 = %.2fms, want at least ~20ms given the 30ms artificial translator delay", got)
	}
}

// TestSessionPassthroughSkipsUnattemptedStagesButRecordsTotal exercises the
// other half of Task 1's contract: a forced-passthrough utterance (here,
// low ASR confidence) never reaches Translate/SynthesizeStream, so it must
// not record "asr_first_chunk"/"mt"/"tts_first_chunk" samples for stages it
// never attempted -- but glass-to-glass latency still matters for a
// degraded call, so "total" must still be recorded.
func TestSessionPassthroughSkipsUnattemptedStagesButRecordsTotal(t *testing.T) {
	rec := &fakeRecognizer{
		scripts: [][]asr.Transcript{
			{{Text: "mumble", Language: "hi", IsFinal: true, Confidence: 0.1}},
			{},
		},
	}
	translator := &fakeTranslator{}

	cfg := validConfig()
	cfg.ASR = rec
	cfg.Translator = translator

	sess, err := NewSession(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{9, 9, 9, 9}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly")
			}
			if chunk.IsFinal {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for passthrough audio")
		}
	}
done:

	if got := translator.callCount(); got != 0 {
		t.Fatalf("translator called %d times, want 0 (low-confidence transcripts must never reach Translate)", got)
	}

	m := sess.Metrics()
	if got := m.Count("asr_first_chunk"); got != 0 {
		t.Errorf("Count(asr_first_chunk) = %d, want 0 for a passthrough utterance that never reached the asr_first_chunk measurement point", got)
	}
	if got := m.Count("mt"); got != 0 {
		t.Errorf("Count(mt) = %d, want 0 for a passthrough utterance that never called Translate", got)
	}
	if got := m.Count("tts_first_chunk"); got != 0 {
		t.Errorf("Count(tts_first_chunk) = %d, want 0 for a passthrough utterance that never called SynthesizeStream", got)
	}

	// emitPassthroughTimed records "total" only after the passthrough send
	// to AgentHearsAudio() fully completes (see session.go) -- the sending
	// goroutine still has to return from forwardAudio/emitPassthrough and
	// call recordTotalIfStarted after this test's receive on
	// AgentHearsAudio() above already unblocked, so checking m.Count
	// immediately is a real (if narrow) race, not a synchronization point.
	// Poll briefly instead of asserting instantaneously -- this is a race
	// discovered during 2026-07-14's integration verification
	// (go test -race -count=3 failed roughly 1 run in 3).
	deadlineTotal := time.After(time.Second)
	for {
		if m.Count("total") > 0 {
			break
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-deadlineTotal:
			t.Fatal("Count(total) = 0 after 1s, want > 0 -- glass-to-glass latency must still be recorded for degraded/passthrough calls")
		}
	}
}
