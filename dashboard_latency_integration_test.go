// Package langstream_test - QA's Sprint (2026-07-12) integration coverage
// for Task 1 of today's charter: verifying Tech's new per-stage latency
// instrumentation (pkg/langstream/session.go's recordLatency/
// recordTotalIfStarted calls, see pkg/langstream/latency_test.go) actually
// reaches SRE's observability dashboard, not just the Session's own
// *observability.LatencyRecorder.
//
// Tech's own new test (pkg/langstream/latency_test.go) already proves the
// recorder itself accumulates real "asr_first_chunk"/"mt"/"tts_first_chunk"/
// "total" samples for a live Session. That's necessary but not sufficient:
// DEVLOG.md's 2026-07-10 entry flagged, as a "worth knowing" observation,
// that before this sprint Session only ever called RecordEvent/RecordError
// (which feed the dashboard's *error-rate* view), never Record/RecordStage
// (which feed its *latency-percentile* view) - so a real session's traffic
// never moved the dashboard's latency section at all. This file closes the
// loop one layer up, in the same spirit as
// observability_dashboard_integration_test.go (which did exactly this for
// the error-rate/cost sections): build a real langstream.Session sharing a
// real *observability.LatencyRecorder, drive real audio through it, and
// assert SRE's actual dashboard HTTP handler (exercised via httptest, using
// the newDashboardTestServer/fetchDashboardJSON helpers already defined in
// observability_dashboard_integration_test.go) now reports non-zero-count
// latency entries for all four stages.
package langstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// delayingTranslator wraps a real translate.Translator, sleeping delay
// before delegating every Translate call. Used so the "mt" latency sample
// this test asserts on has a real, unmistakable, non-near-zero value -
// proving the dashboard is showing an actual measurement rather than a
// placeholder/zero value that happens to pass a ">0" check by
// coincidence, mirroring the same technique pkg/langstream/latency_test.go
// uses (its local slowTranslator) one layer down.
type delayingTranslator struct {
	inner translate.Translator
	delay time.Duration
}

func (d *delayingTranslator) Name() string { return d.inner.Name() }
func (d *delayingTranslator) SupportedPairs() [][2]translate.Language {
	return d.inner.SupportedPairs()
}
func (d *delayingTranslator) Translate(ctx context.Context, text string, source, target translate.Language, isFinal bool) (translate.Chunk, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return translate.Chunk{}, ctx.Err()
	}
	return d.inner.Translate(ctx, text, source, target, isFinal)
}

var _ translate.Translator = (*delayingTranslator)(nil)

// TestDashboardLatency_ReflectsRealSessionUtteranceEndToEnd is the
// highest-priority Task 1 check: a real langstream.Session (real
// scriptedRecognizer ASR, a real translate.MockTranslator delayed by an
// artificial 40ms so "mt" is assertably non-trivial, a real
// tts.MockSynthesizer) sharing a real *observability.LatencyRecorder via
// SessionConfig.Fallback.Metrics, driven through one full successful
// utterance round trip, then read back through SRE's actual dashboard
// HTTP handler (not BuildDashboardData called directly, and not the
// recorder's own Count/Percentile methods) - proving the wiring holds
// through the full real path a dashboard operator or /dashboard.json
// consumer would actually see.
func TestDashboardLatency_ReflectsRealSessionUtteranceEndToEnd(t *testing.T) {
	ctx := context.Background()

	callerScript := []asr.Transcript{
		{Text: "hello agent", Language: "hi", IsFinal: true, Confidence: 0.99},
	}
	asrRec := &scriptedRecognizer{scripts: [][]asr.Transcript{callerScript, nil}}

	const artificialMTDelay = 40 * time.Millisecond
	translator := &delayingTranslator{inner: translate.NewMockTranslator(), delay: artificialMTDelay}

	rec := observability.NewLatencyRecorder()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asrRec,
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
		Fallback: langstream.FallbackConfig{
			Metrics:            rec,
			DegradeToneEnabled: true, // touching any Fallback field requires setting this explicitly
		},
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if got := sess.Metrics(); got != rec {
		t.Fatal("Session.Metrics() must return the exact recorder passed via SessionConfig.Fallback.Metrics")
	}

	frame := asr.AudioFrame{PCM: []byte{1, 2, 3, 4, 5, 6}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}
	chunks := drainUntilFinal(t, sess.AgentHearsAudio(), 3*time.Second)
	if len(chunks) == 0 {
		t.Fatal("expected at least one translated audio chunk out of a full round trip")
	}

	// --- Read the latency data back through the real dashboard HTTP
	// handler, not the recorder directly, so this test genuinely exercises
	// the same path a dashboard operator/scraper would. ---

	server := newDashboardTestServer(t, rec)
	defer server.Close()

	data := fetchDashboardJSON(t, server.URL)

	byStage := make(map[string]observability.StageLatencySummary, len(data.Latency))
	for _, s := range data.Latency {
		byStage[s.Stage] = s
	}

	for _, stage := range []string{"asr_first_chunk", "mt", "tts_first_chunk", "total"} {
		summary, ok := byStage[stage]
		if !ok {
			t.Errorf("dashboard.json Latency has no entry for stage %q; got stages %+v -- the DEVLOG 2026-07-10 gap (Session never called Record/RecordStage) is NOT closed for this stage", stage, data.Latency)
			continue
		}
		if summary.Count == 0 {
			t.Errorf("dashboard.json Latency[%q].Count = 0, want > 0 for a completed round trip", stage)
		}
	}

	// The "mt" stage in particular must reflect the real artificial delay,
	// not a hardcoded/zero placeholder that happened to pass the Count>0
	// check above.
	if mt, ok := byStage["mt"]; ok {
		wantMinMs := float64(artificialMTDelay/time.Millisecond) / 2 // generous margin below the 40ms delay
		if mt.P50 < wantMinMs {
			t.Errorf("dashboard.json Latency[mt].P50 = %.2fms, want at least ~%.0fms given the %v artificial translator delay", mt.P50, wantMinMs, artificialMTDelay)
		}
	}
	// "total" spans the whole utterance including the MT delay, so it must
	// be at least as large as "mt" was.
	if total, ok := byStage["total"]; ok {
		wantMinMs := float64(artificialMTDelay/time.Millisecond) / 2
		if total.P50 < wantMinMs {
			t.Errorf("dashboard.json Latency[total].P50 = %.2fms, want at least ~%.0fms given the %v artificial translator delay", total.P50, wantMinMs, artificialMTDelay)
		}
	}

	// Cross-check the same data is reachable via BuildDashboardData
	// directly too (proves the HTTP handler isn't computing something
	// different from the documented programmatic API), matching the style
	// of the existing observability dashboard integration test's
	// recorder-vs-HTTP cross-check for errors/costs.
	direct := observability.BuildDashboardData(rec)
	directByStage := make(map[string]observability.StageLatencySummary, len(direct.Latency))
	for _, s := range direct.Latency {
		directByStage[s.Stage] = s
	}
	for _, stage := range []string{"asr_first_chunk", "mt", "tts_first_chunk", "total"} {
		if directByStage[stage].Count != byStage[stage].Count {
			t.Errorf("BuildDashboardData(rec) Latency[%q].Count = %d, dashboard.json reported %d -- HTTP handler and direct API disagree", stage, directByStage[stage].Count, byStage[stage].Count)
		}
	}
}

// TestDashboardLatency_PassthroughUtteranceOnlyShowsTotalStage is the
// dashboard-perspective counterpart of
// pkg/langstream/latency_test.go's TestSessionPassthroughSkipsUnattemptedStagesButRecordsTotal:
// a forced-passthrough utterance (low ASR confidence, so Translate/
// SynthesizeStream are never attempted) must show up in the dashboard's
// latency section under "total" only -- "asr_first_chunk"/"mt"/
// "tts_first_chunk" must not appear at all (not just "count 0": Stages()
// only returns stages with at least one sample, so an unattempted stage
// should be entirely absent from the dashboard, not present with a
// zero/placeholder row).
func TestDashboardLatency_PassthroughUtteranceOnlyShowsTotalStage(t *testing.T) {
	ctx := context.Background()

	callerScript := []asr.Transcript{
		{Text: "mumble", Language: "hi", IsFinal: true, Confidence: 0.1}, // below default 0.55 threshold
	}
	asrRec := &scriptedRecognizer{scripts: [][]asr.Transcript{callerScript, nil}}

	translator := &countingTranslator{inner: translate.NewMockTranslator()}

	rec := observability.NewLatencyRecorder()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asrRec,
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
		Fallback: langstream.FallbackConfig{
			Metrics:            rec,
			DegradeToneEnabled: true,
		},
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	frame := asr.AudioFrame{PCM: []byte{9, 9, 9, 9}, SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}
	chunks := drainUntilFinal(t, sess.AgentHearsAudio(), 3*time.Second)
	if len(chunks) == 0 {
		t.Fatal("expected passthrough audio for a low-confidence utterance")
	}
	if got := translator.callCount(); got != 0 {
		t.Fatalf("translator called %d times, want 0 (low-confidence transcripts must never reach Translate)", got)
	}

	server := newDashboardTestServer(t, rec)
	defer server.Close()

	data := fetchDashboardJSON(t, server.URL)

	byStage := make(map[string]observability.StageLatencySummary, len(data.Latency))
	for _, s := range data.Latency {
		byStage[s.Stage] = s
	}

	for _, stage := range []string{"asr_first_chunk", "mt", "tts_first_chunk"} {
		if summary, ok := byStage[stage]; ok {
			t.Errorf("dashboard.json Latency has an entry for stage %q (Count=%d) on a passthrough utterance that never attempted it; want no entry at all", stage, summary.Count)
		}
	}
	total, ok := byStage["total"]
	if !ok {
		t.Fatal("dashboard.json Latency has no entry for stage \"total\" -- glass-to-glass latency must still be recorded for degraded/passthrough calls")
	}
	if total.Count == 0 {
		t.Error("dashboard.json Latency[total].Count = 0, want > 0 for a completed passthrough utterance")
	}
}
