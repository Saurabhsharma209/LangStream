// Package langstream_test - QA's Sprint 3 integration coverage proving
// Tech's fallback instrumentation and SRE's observability dashboard
// (pkg/observability/metrics.go, pkg/observability/dashboard.go) actually
// compose: a *real* langstream.Session sharing a *real*
// *observability.LatencyRecorder feeds fallback error events into it (see
// pkg/langstream/fallback.go's recordFallback/recordSuccessMetric), and
// that same recorder is then served by SRE's real
// observability.NewDashboardHandler, exercised end-to-end over HTTP via
// httptest. This is the "would these pieces actually work if wired
// together in cmd/langstream" check called for by QA's charter - today
// cmd/langstream/main.go does not yet wire a shared recorder into the
// dashboard at all (there's no dashboard flag/subcommand yet), so nothing
// else in the repo currently proves this composition works.
package langstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// TestObservabilityDashboard_ReflectsRealSessionFallbackAndCost drives a
// real Session (real mock ASR/TTS, a translator that fails on specific
// calls) with a shared *observability.LatencyRecorder wired in via
// SessionConfig.Fallback.Metrics, then asserts SRE's dashboard handler
// (/dashboard.json and /metrics) reports exactly the error rate and cost
// numbers that history should produce.
func TestObservabilityDashboard_ReflectsRealSessionFallbackAndCost(t *testing.T) {
	ctx := context.Background()

	// 4 caller utterances; translate calls #2 and #4 fail (non-fatal,
	// non-consecutive so the leg never permanently degrades - this
	// exercises the steady-state "some errors, not a dead leg" case a
	// dashboard needs to render, not just the all-or-nothing fallback
	// paths fallback_integration_test.go already covers).
	callerScript := make([]asr.Transcript, 4)
	for i := range callerScript {
		callerScript[i] = asr.Transcript{Text: fmt.Sprintf("utterance %d", i), Language: "hi", IsFinal: true, Confidence: 0.99}
	}
	asrRec := &scriptedRecognizer{scripts: [][]asr.Transcript{callerScript, nil}}

	realTranslator := translate.NewMockTranslator() // supports hi<->en normally
	translator := &countingTranslator{
		inner: realTranslator,
		failOn: map[int]error{
			2: fmt.Errorf("translate/test: simulated transient vendor error (call 2)"),
			4: fmt.Errorf("translate/test: simulated transient vendor error (call 4)"),
		},
	}

	rec := observability.NewLatencyRecorder()

	cfg := langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asrRec,
		Translator:     translator,
		TTS:            tts.NewMockSynthesizer("hi", "en"),
		Fallback: langstream.FallbackConfig{
			Metrics:            rec,
			DegradeToneEnabled: true, // touching any Fallback field requires setting this explicitly (see FallbackConfig's doc comment)
		},
	}

	sess, err := langstream.NewSession(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	if got := sess.Metrics(); got != rec {
		t.Fatal("Session.Metrics() must return the exact recorder passed via SessionConfig.Fallback.Metrics, not a private one, once one is supplied")
	}

	for i := 0; i < len(callerScript); i++ {
		frame := asr.AudioFrame{PCM: []byte{byte(i + 1), byte(i + 1)}, SampleRate: 8000}
		if err := sess.PushCallerAudio(frame); err != nil {
			t.Fatalf("PushCallerAudio #%d: %v", i, err)
		}
		drainUntilFinal(t, sess.AgentHearsAudio(), 2*time.Second)
	}

	if sess.CallerLegDegraded() {
		t.Fatal("2 non-consecutive failures out of 4 must not permanently degrade the leg (default MaxConsecutiveFailures is 3)")
	}

	// Simulate the billing/cost-accounting hook a real cmd/langstream
	// deployment would drive off the same shared recorder (Session itself
	// never calls RecordCost anywhere today - there is no automatic cost
	// instrumentation yet, see pkg/observability/metrics.go's RecordCost
	// doc comment), so the dashboard's cost section is not always empty
	// once real backends are wired in.
	rec.RecordCost("mock", 0.02)
	rec.RecordCost("mock", 0.015)
	rec.RecordCost("mock", 0.025)

	// --- Now hit SRE's real dashboard handler over actual HTTP ---

	server := newDashboardTestServer(t, rec)
	defer server.Close()

	data := fetchDashboardJSON(t, server.URL)

	var translateStats *observability.ErrorStats
	for i := range data.Errors {
		if data.Errors[i].Stage == "translate" && data.Errors[i].Vendor == "mock" {
			translateStats = &data.Errors[i]
		}
	}
	if translateStats == nil {
		t.Fatalf("dashboard.json Errors has no (translate, mock) entry; got %+v", data.Errors)
	}
	if translateStats.Errors != 2 {
		t.Errorf("translate/mock Errors = %d, want 2", translateStats.Errors)
	}
	if translateStats.Total != 4 {
		t.Errorf("translate/mock Total = %d, want 4", translateStats.Total)
	}
	if translateStats.Rate != 0.5 {
		t.Errorf("translate/mock Rate = %v, want 0.5", translateStats.Rate)
	}

	var costStats *observability.CostStats
	for i := range data.Costs {
		if data.Costs[i].Vendor == "mock" {
			costStats = &data.Costs[i]
		}
	}
	if costStats == nil {
		t.Fatalf("dashboard.json Costs has no mock entry; got %+v", data.Costs)
	}
	const wantCost = 0.02 + 0.015 + 0.025
	if diff := costStats.TotalUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("mock TotalUSD = %v, want %v", costStats.TotalUSD, wantCost)
	}
	if costStats.Events != 3 {
		t.Errorf("mock cost Events = %d, want 3", costStats.Events)
	}

	// Cross-check the same numbers are reachable straight off the shared
	// recorder too (proves the dashboard didn't just make them up from
	// its own separate state - there is none; BuildDashboardData always
	// re-snapshots r).
	if got := rec.ErrorRate("translate", "mock"); got != 0.5 {
		t.Fatalf("rec.ErrorRate(translate, mock) = %v, want 0.5", got)
	}
	if got := rec.CostTotal("mock"); got-wantCost > 1e-9 || got-wantCost < -1e-9 {
		t.Fatalf("rec.CostTotal(mock) = %v, want %v", got, wantCost)
	}

	// --- /metrics (Prometheus text exposition) ---

	metricsBody := fetchMetricsText(t, server.URL)
	wantSubstrings := []string{
		`langstream_stage_errors_total{stage="translate",vendor="mock"} 2`,
		`langstream_stage_events_total{stage="translate",vendor="mock"} 4`,
		`langstream_stage_error_rate{stage="translate",vendor="mock"} 0.5`,
		`langstream_vendor_cost_events_total{vendor="mock"} 3`,
		// Not asserting the exact cost total string here (float64 summation
		// of 0.02+0.015+0.025 can render with harmless extra precision
		// noise depending on accumulation order); the JSON assertions above
		// already pin the exact numeric value with an epsilon.
		`langstream_vendor_cost_usd_total{vendor="mock"}`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(metricsBody, want) {
			t.Errorf("/metrics missing expected line containing %q\nfull body:\n%s", want, metricsBody)
		}
	}

	// --- "/" HTML page: light smoke test that it renders at all and
	// reflects the same vendor, without over-fitting to exact markup. ---

	htmlBody := fetchPath(t, server.URL, "/")
	if !strings.Contains(htmlBody, "LangStream Observability Dashboard") {
		t.Error("/ response missing expected dashboard title")
	}
	if !strings.Contains(htmlBody, "mock") {
		t.Error("/ response does not mention the mock vendor anywhere")
	}
}

// TestObservabilityDashboard_EmptyRecorderRendersCleanly is a smaller
// composition check for the boundary case: a Session that never has any
// audio pushed still produces a *observability.LatencyRecorder the
// dashboard handler can serve without error (no nil maps / no panics on
// zero data), since a dashboard operator will load "/" long before any
// real call has completed.
func TestObservabilityDashboard_EmptyRecorderRendersCleanly(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	server := newDashboardTestServer(t, rec)
	defer server.Close()

	data := fetchDashboardJSON(t, server.URL)
	if len(data.Errors) != 0 {
		t.Errorf("expected no error entries on an untouched recorder, got %+v", data.Errors)
	}
	if len(data.Costs) != 0 {
		t.Errorf("expected no cost entries on an untouched recorder, got %+v", data.Costs)
	}

	htmlBody := fetchPath(t, server.URL, "/")
	if !strings.Contains(htmlBody, "No error/event data recorded yet.") {
		t.Error("expected the empty-state message for errors on a fresh recorder")
	}
	if !strings.Contains(htmlBody, "No cost data recorded yet.") {
		t.Error("expected the empty-state message for costs on a fresh recorder")
	}
}

// --- shared HTTP helpers ---

func newDashboardTestServer(t *testing.T, rec *observability.LatencyRecorder) *httptest.Server {
	t.Helper()
	return httptest.NewServer(observability.NewDashboardHandler(rec))
}

func fetchDashboardJSON(t *testing.T, baseURL string) observability.DashboardData {
	t.Helper()
	body := fetchPath(t, baseURL, "/dashboard.json")
	var data observability.DashboardData
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("decoding /dashboard.json: %v\nbody: %s", err, body)
	}
	return data
}

func fetchMetricsText(t *testing.T, baseURL string) string {
	t.Helper()
	return fetchPath(t, baseURL, "/metrics")
}

func fetchPath(t *testing.T, baseURL, path string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}
