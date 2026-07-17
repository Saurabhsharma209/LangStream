package langstream

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// --- FallbackConfig defaults ---

func TestDefaultFallbackConfigValues(t *testing.T) {
	d := DefaultFallbackConfig()
	if d.ConfidenceThreshold != 0.55 {
		t.Errorf("ConfidenceThreshold = %v, want 0.55", d.ConfidenceThreshold)
	}
	if !d.DegradeToneEnabled {
		t.Error("DegradeToneEnabled = false, want true")
	}
	if d.TranslateTimeout != 2*time.Second {
		t.Errorf("TranslateTimeout = %v, want 2s", d.TranslateTimeout)
	}
	if d.SynthesizeTimeout != 3*time.Second {
		t.Errorf("SynthesizeTimeout = %v, want 3s", d.SynthesizeTimeout)
	}
	if d.MaxConsecutiveFailures != 3 {
		t.Errorf("MaxConsecutiveFailures = %v, want 3", d.MaxConsecutiveFailures)
	}
}

func TestFallbackConfigWithDefaultsFillsOnlyZeroFields(t *testing.T) {
	cfg := FallbackConfig{ConfidenceThreshold: 0.9}
	got := cfg.withDefaults()

	if got.ConfidenceThreshold != 0.9 {
		t.Errorf("ConfidenceThreshold = %v, want explicit 0.9 preserved", got.ConfidenceThreshold)
	}
	if got.TranslateTimeout != 2*time.Second {
		t.Errorf("TranslateTimeout = %v, want defaulted 2s", got.TranslateTimeout)
	}
	if got.SynthesizeTimeout != 3*time.Second {
		t.Errorf("SynthesizeTimeout = %v, want defaulted 3s", got.SynthesizeTimeout)
	}
	if got.MaxConsecutiveFailures != 3 {
		t.Errorf("MaxConsecutiveFailures = %v, want defaulted 3", got.MaxConsecutiveFailures)
	}
	// DegradeToneEnabled is explicitly NOT defaulted by withDefaults - see
	// its doc comment. It should stay exactly as given (false, the zero
	// value here, since the test didn't set it).
	if got.DegradeToneEnabled {
		t.Error("DegradeToneEnabled should be left as explicitly given (false), not defaulted, once any field is set")
	}
}

// --- FatalError / isFatal ---

type fatalErr struct{ fatal bool }

func (e fatalErr) Error() string { return fmt.Sprintf("fatalErr{fatal:%v}", e.fatal) }
func (e fatalErr) Fatal() bool   { return e.fatal }

func TestIsFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"plain error", errors.New("boom"), false},
		{"FatalError reporting true", fatalErr{fatal: true}, true},
		{"FatalError reporting false", fatalErr{fatal: false}, false},
		{"wrapped FatalError", fmt.Errorf("wrap: %w", fatalErr{fatal: true}), true},
		{"context deadline exceeded (not fatal)", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFatal(tc.err); got != tc.want {
				t.Errorf("isFatal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- audioRingBuffer ---

func TestAudioRingBufferPushAndDrain(t *testing.T) {
	b := newAudioRingBuffer(10)

	b.push([]byte{1, 2}, 8000)
	b.push([]byte{3, 4}, 8000)

	frames := b.drain()
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if string(frames[0].pcm) != "\x01\x02" || string(frames[1].pcm) != "\x03\x04" {
		t.Fatalf("unexpected frame contents: %+v", frames)
	}

	// drain empties the buffer.
	if got := b.drain(); len(got) != 0 {
		t.Fatalf("second drain returned %d frames, want 0", len(got))
	}
}

func TestAudioRingBufferPushCopiesInput(t *testing.T) {
	b := newAudioRingBuffer(10)
	pcm := []byte{1, 2, 3}
	b.push(pcm, 8000)
	pcm[0] = 99 // mutate the caller's slice after push

	frames := b.drain()
	if frames[0].pcm[0] != 1 {
		t.Fatalf("audioRingBuffer.push must copy its input; got %v, want first byte 1", frames[0].pcm)
	}
}

func TestAudioRingBufferDropsOldestBeyondCapacity(t *testing.T) {
	b := newAudioRingBuffer(3)
	for i := 0; i < 5; i++ {
		b.push([]byte{byte(i)}, 8000)
	}
	frames := b.drain()
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want capacity-bounded 3", len(frames))
	}
	// Oldest (0, 1) should have been dropped; only 2, 3, 4 remain.
	want := []byte{2, 3, 4}
	for i, f := range frames {
		if f.pcm[0] != want[i] {
			t.Fatalf("frame %d = %v, want first byte %d", i, f.pcm, want[i])
		}
	}
}

func TestAudioRingBufferIgnoresEmptyPush(t *testing.T) {
	b := newAudioRingBuffer(10)
	b.push(nil, 8000)
	b.push([]byte{}, 8000)
	if got := b.drain(); len(got) != 0 {
		t.Fatalf("got %d frames after pushing only empty PCM, want 0", len(got))
	}
}

// --- legState ---

func TestLegStateRecordFailureDegradesAfterMaxConsecutive(t *testing.T) {
	ls := newLegState("caller", audioBufferFrames)

	if ls.isDegraded() {
		t.Fatal("new legState must not start degraded")
	}

	newlyDegraded := ls.recordFailure(false, 3)
	if newlyDegraded || ls.isDegraded() {
		t.Fatal("1st failure of 3 must not degrade the leg")
	}
	newlyDegraded = ls.recordFailure(false, 3)
	if newlyDegraded || ls.isDegraded() {
		t.Fatal("2nd failure of 3 must not degrade the leg")
	}
	newlyDegraded = ls.recordFailure(false, 3)
	if !newlyDegraded || !ls.isDegraded() {
		t.Fatal("3rd consecutive failure must degrade the leg and report newly-degraded")
	}

	// Once degraded, further failures report "not newly degraded" (it
	// already was).
	if ls.recordFailure(false, 3) {
		t.Fatal("recordFailure on an already-degraded leg must not report newly-degraded again")
	}
}

func TestLegStateRecordSuccessResetsConsecutiveCountButNotDegraded(t *testing.T) {
	ls := newLegState("agent", audioBufferFrames)

	ls.recordFailure(false, 3)
	ls.recordFailure(false, 3)
	ls.recordSuccess() // resets the streak

	if ls.recordFailure(false, 3) {
		t.Fatal("count should have reset after recordSuccess; this must be failure 1/3, not 3/3")
	}
	if ls.recordFailure(false, 3) {
		t.Fatal("this must be failure 2/3")
	}
	if !ls.recordFailure(false, 3) {
		t.Fatal("this must be failure 3/3 and degrade the leg")
	}

	// recordSuccess never un-degrades a leg once it is permanently
	// degraded (see legState.recordSuccess's doc comment).
	ls.recordSuccess()
	if !ls.isDegraded() {
		t.Fatal("recordSuccess must not clear permanent degradation")
	}
}

func TestLegStateFatalErrorDegradesImmediately(t *testing.T) {
	ls := newLegState("caller", audioBufferFrames)
	if !ls.recordFailure(true, 3) {
		t.Fatal("a single fatal failure must degrade the leg immediately")
	}
	if !ls.isDegraded() {
		t.Fatal("leg must be degraded after a fatal failure")
	}
}

// --- generateDegradeTone ---

func TestGenerateDegradeToneShapeAndDeterminism(t *testing.T) {
	sampleRate := 8000
	tone := generateDegradeTone(sampleRate)

	wantLen := sampleRate * degradeToneDurationMS / 1000 * 2 // 16-bit samples
	if len(tone) != wantLen {
		t.Fatalf("tone length = %d bytes, want %d", len(tone), wantLen)
	}

	// Deterministic: same sample rate must produce byte-identical output.
	again := generateDegradeTone(sampleRate)
	if string(tone) != string(again) {
		t.Fatal("generateDegradeTone must be deterministic for a fixed sampleRate")
	}

	// Not silence: at least one non-zero byte.
	allZero := true
	for _, b := range tone {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("tone must not be all-zero silence")
	}
}

func TestGenerateDegradeToneNonPositiveSampleRateDefaultsTo8kHz(t *testing.T) {
	tone := generateDegradeTone(0)
	want := generateDegradeTone(8000)
	if string(tone) != string(want) {
		t.Fatal("sampleRate <= 0 should default to 8000Hz")
	}
}

// --- buildPassthroughChunks ---

func TestBuildPassthroughChunksWithToneAndFrames(t *testing.T) {
	frames := []bufferedFrame{
		{pcm: []byte{1, 2}, sampleRate: 8000},
		{pcm: []byte{3, 4}, sampleRate: 8000},
	}
	chunks := buildPassthroughChunks(frames, true)

	if len(chunks) != 3 { // tone + 2 frames
		t.Fatalf("got %d chunks, want 3 (tone + 2 frames)", len(chunks))
	}
	if len(chunks[0].PCM) == 0 {
		t.Fatal("first chunk should be the non-empty warning tone")
	}
	if string(chunks[1].PCM) != "\x01\x02" || string(chunks[2].PCM) != "\x03\x04" {
		t.Fatalf("frame chunks out of order or corrupted: %+v", chunks)
	}
	for i, c := range chunks {
		wantFinal := i == len(chunks)-1
		if c.IsFinal != wantFinal {
			t.Errorf("chunk %d IsFinal = %v, want %v", i, c.IsFinal, wantFinal)
		}
	}
}

func TestBuildPassthroughChunksWithoutTone(t *testing.T) {
	frames := []bufferedFrame{{pcm: []byte{5, 6}, sampleRate: 16000}}
	chunks := buildPassthroughChunks(frames, false)

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 (no tone, one frame)", len(chunks))
	}
	if string(chunks[0].PCM) != "\x05\x06" {
		t.Fatalf("chunk PCM = %v, want the raw frame", chunks[0].PCM)
	}
	if chunks[0].SampleRate != 16000 {
		t.Fatalf("SampleRate = %d, want 16000 (from the buffered frame)", chunks[0].SampleRate)
	}
	if !chunks[0].IsFinal {
		t.Fatal("single chunk must be marked final")
	}
}

func TestBuildPassthroughChunksNoFramesNoTone(t *testing.T) {
	chunks := buildPassthroughChunks(nil, false)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 (defensive empty-final marker)", len(chunks))
	}
	if !chunks[0].IsFinal {
		t.Fatal("defensive empty chunk must still be marked final")
	}
}

func TestBuildPassthroughChunksNoFramesToneOnly(t *testing.T) {
	chunks := buildPassthroughChunks(nil, true)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 (tone only)", len(chunks))
	}
	if len(chunks[0].PCM) == 0 {
		t.Fatal("expected the tone-only chunk to carry non-empty PCM")
	}
	if !chunks[0].IsFinal {
		t.Fatal("tone-only chunk must be marked final")
	}
}

// --- chunksChannel ---

func TestChunksChannelDeliversInOrderThenCloses(t *testing.T) {
	in := []tts.AudioChunk{
		{PCM: []byte{1}},
		{PCM: []byte{2}},
	}
	ch := chunksChannel(in)

	first, ok := <-ch
	if !ok || string(first.PCM) != "\x01" {
		t.Fatalf("first chunk = %+v ok=%v, want PCM=[1] ok=true", first, ok)
	}
	second, ok := <-ch
	if !ok || string(second.PCM) != "\x02" {
		t.Fatalf("second chunk = %+v ok=%v, want PCM=[2] ok=true", second, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after delivering all chunks")
	}
}

// --- recordFallback / recordSuccessMetric: use the *real*,
// pre-existing observability.LatencyRecorder API (RecordError/RecordEvent),
// per this change's constraint of not adding anything new to
// pkg/observability.

func TestRecordFallbackUsesExistingObservabilityAPI(t *testing.T) {
	rec := observability.NewLatencyRecorder()

	recordFallback(rec, stageTranslate, "gpt4o")
	recordSuccessMetric(rec, stageTranslate, "gpt4o")

	if got := rec.ErrorCount(stageTranslate, "gpt4o"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
	if got := rec.EventCount(stageTranslate, "gpt4o"); got != 2 {
		t.Fatalf("EventCount = %d, want 2 (1 error + 1 success)", got)
	}
	if got := rec.ErrorRate(stageTranslate, "gpt4o"); got != 0.5 {
		t.Fatalf("ErrorRate = %v, want 0.5", got)
	}
}

func TestRecordFallbackNilRecorderIsNoop(t *testing.T) {
	// Must not panic when no recorder is configured.
	recordFallback(nil, stageTTS, "cartesia")
	recordSuccessMetric(nil, stageTTS, "cartesia")
}

func TestRecordFallbackEmptyVendorFallsBackToUnknown(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	recordFallback(rec, stageLegDegraded, "")
	if got := rec.ErrorCount(stageLegDegraded, "unknown"); got != 1 {
		t.Fatalf("ErrorCount(unknown) = %d, want 1", got)
	}
}

// --- recordFallbackErr: like recordFallback, but additionally tags
// "circuit_open" (via RecordErrorReason) when the given err indicates the
// vendor client's own circuit breaker rejected the call locally
// (translate.ErrCircuitOpen / tts.ErrCircuitOpen, or an error wrapping
// either).

func TestRecordFallbackErrTranslateCircuitOpenRecordsReason(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	err := fmt.Errorf("translate/gpt4o: %w", translate.ErrCircuitOpen)

	recordFallbackErr(rec, stageTranslate, "gpt4o", err)

	if got := rec.ReasonCount(stageTranslate, "gpt4o", "circuit_open"); got != 1 {
		t.Fatalf("ReasonCount(circuit_open) = %d, want 1", got)
	}
	// A RecordErrorReason call must still count as an ordinary error too
	// (see ReasonStats's doc comment: Reasons is a breakdown of Errors,
	// not tracked separately).
	if got := rec.ErrorCount(stageTranslate, "gpt4o"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
}

func TestRecordFallbackErrTTSCircuitOpenRecordsReason(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	err := fmt.Errorf("tts/cartesia: %w", tts.ErrCircuitOpen)

	recordFallbackErr(rec, stageTTS, "cartesia", err)

	if got := rec.ReasonCount(stageTTS, "cartesia", "circuit_open"); got != 1 {
		t.Fatalf("ReasonCount(circuit_open) = %d, want 1", got)
	}
	if got := rec.ErrorCount(stageTTS, "cartesia"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
}

func TestRecordFallbackErrOrdinaryErrorRecordsNoReason(t *testing.T) {
	rec := observability.NewLatencyRecorder()

	recordFallbackErr(rec, stageTranslate, "gpt4o", errors.New("some transient network blip"))

	if got := rec.ReasonCount(stageTranslate, "gpt4o", "circuit_open"); got != 0 {
		t.Fatalf("ReasonCount(circuit_open) = %d, want 0 for an ordinary error", got)
	}
	// The plain empty-reason count (recorded via RecordErrorReason(...,
	// "")) is intentionally not tracked in ReasonSnapshot/ReasonCount --
	// see RecordErrorReason's doc comment -- but the failure must still
	// show up as an ordinary ErrorCount/EventCount, identical to what
	// recordFallback would have produced.
	if got := rec.ErrorCount(stageTranslate, "gpt4o"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
	if got := rec.ReasonCount(stageTranslate, "gpt4o", ""); got != 0 {
		t.Fatalf("ReasonCount(\"\") = %d, want 0 (empty reason never tracked)", got)
	}
}

func TestRecordFallbackErrOrdinaryTTSErrorRecordsNoReason(t *testing.T) {
	rec := observability.NewLatencyRecorder()

	recordFallbackErr(rec, stageTTS, "elevenlabs", errors.New("connection reset"))

	if got := rec.ReasonCount(stageTTS, "elevenlabs", "circuit_open"); got != 0 {
		t.Fatalf("ReasonCount(circuit_open) = %d, want 0 for an ordinary error", got)
	}
	if got := rec.ErrorCount(stageTTS, "elevenlabs"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
}

func TestRecordFallbackErrNilRecorderIsNoop(t *testing.T) {
	// Must not panic when no recorder is configured.
	recordFallbackErr(nil, stageTranslate, "gpt4o", translate.ErrCircuitOpen)
}

func TestRecordFallbackErrEmptyVendorFallsBackToUnknown(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	recordFallbackErr(rec, stageTranslate, "", translate.ErrCircuitOpen)
	if got := rec.ReasonCount(stageTranslate, "unknown", "circuit_open"); got != 1 {
		t.Fatalf("ReasonCount(unknown, circuit_open) = %d, want 1", got)
	}
}

func TestRecordFallbackErrNilErrRecordsNoReason(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	recordFallbackErr(rec, stageTranslate, "gpt4o", nil)
	if got := rec.ReasonCount(stageTranslate, "gpt4o", "circuit_open"); got != 0 {
		t.Fatalf("ReasonCount(circuit_open) = %d, want 0 for a nil error", got)
	}
	if got := rec.ErrorCount(stageTranslate, "gpt4o"); got != 1 {
		t.Fatalf("ErrorCount = %d, want 1", got)
	}
}
