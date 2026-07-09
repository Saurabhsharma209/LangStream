package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newSampleRecorder() *LatencyRecorder {
	r := NewLatencyRecorder()

	r.Record("asr_first_chunk", 100)
	r.Record("asr_first_chunk", 150)
	r.Record("mt", 20)

	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordError("asr_first_chunk", "deepgram")

	r.RecordCost("deepgram", 0.05)
	r.RecordCost("deepgram", 0.03)
	r.RecordCost("google-translate", 0.10)

	return r
}

func TestBuildDashboardData(t *testing.T) {
	r := newSampleRecorder()
	data := BuildDashboardData(r)

	if len(data.Latency) != 2 {
		t.Fatalf("Latency sections = %d, want 2 (asr_first_chunk, mt)", len(data.Latency))
	}
	var sawASR bool
	for _, s := range data.Latency {
		if s.Stage == "asr_first_chunk" {
			sawASR = true
			if s.Count != 2 {
				t.Errorf("asr_first_chunk count = %d, want 2", s.Count)
			}
		}
	}
	if !sawASR {
		t.Fatalf("expected an asr_first_chunk latency summary, got %+v", data.Latency)
	}

	if len(data.Errors) != 1 {
		t.Fatalf("Errors sections = %d, want 1", len(data.Errors))
	}
	es := data.Errors[0]
	if es.Stage != "asr_first_chunk" || es.Vendor != "deepgram" {
		t.Errorf("unexpected error stats key: %+v", es)
	}
	if es.Errors != 1 || es.Total != 3 {
		t.Errorf("error stats = %+v, want Errors=1 Total=3", es)
	}
	if !almostEqual(es.Rate, 1.0/3.0, 1e-9) {
		t.Errorf("error rate = %v, want %v", es.Rate, 1.0/3.0)
	}

	if len(data.Costs) != 2 {
		t.Fatalf("Costs sections = %d, want 2", len(data.Costs))
	}
	var sawDeepgram bool
	for _, c := range data.Costs {
		if c.Vendor == "deepgram" {
			sawDeepgram = true
			if !almostEqual(c.TotalUSD, 0.08, 1e-9) {
				t.Errorf("deepgram cost total = %v, want 0.08", c.TotalUSD)
			}
			if c.Events != 2 {
				t.Errorf("deepgram cost events = %d, want 2", c.Events)
			}
		}
	}
	if !sawDeepgram {
		t.Fatalf("expected a deepgram cost entry, got %+v", data.Costs)
	}
}

func TestBuildDashboardDataEmpty(t *testing.T) {
	r := NewLatencyRecorder()
	data := BuildDashboardData(r)

	if len(data.Latency) != 0 || len(data.Errors) != 0 || len(data.Costs) != 0 {
		t.Fatalf("expected all-empty dashboard data for fresh recorder, got %+v", data)
	}
}

// TestDashboardHandlerHTML exercises the HTML page via httptest.NewRecorder,
// i.e. without binding any real network port.
func TestDashboardHandlerHTML(t *testing.T) {
	r := newSampleRecorder()
	handler := NewDashboardHandler(r)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"LangStream Observability Dashboard",
		"asr_first_chunk",
		"deepgram",
		"google-translate",
		"33.333333%", // 1/3 error rate
	} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML body missing expected substring %q\nbody:\n%s", want, body)
		}
	}
}

func TestDashboardHandlerHTMLEmptyRecorder(t *testing.T) {
	r := NewLatencyRecorder()
	handler := NewDashboardHandler(r)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"No latency samples recorded yet.",
		"No error/event data recorded yet.",
		"No cost data recorded yet.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML body missing empty-state message %q\nbody:\n%s", want, body)
		}
	}
}

func TestDashboardHandlerUnknownPath404s(t *testing.T) {
	r := newSampleRecorder()
	handler := NewDashboardHandler(r)

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /does-not-exist status = %d, want 404", rec.Code)
	}
}

func TestDashboardHandlerJSON(t *testing.T) {
	r := newSampleRecorder()
	handler := NewDashboardHandler(r)

	req := httptest.NewRequest(http.MethodGet, "/dashboard.json", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard.json status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var data DashboardData
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to unmarshal JSON body: %v\nbody:\n%s", err, rec.Body.String())
	}
	if len(data.Latency) != 2 {
		t.Errorf("decoded Latency len = %d, want 2", len(data.Latency))
	}
	if len(data.Errors) != 1 {
		t.Errorf("decoded Errors len = %d, want 1", len(data.Errors))
	}
	if len(data.Costs) != 2 {
		t.Errorf("decoded Costs len = %d, want 2", len(data.Costs))
	}
}

func TestDashboardHandlerMetrics(t *testing.T) {
	r := newSampleRecorder()
	handler := NewDashboardHandler(r)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"langstream_stage_latency_ms",
		"langstream_stage_errors_total",
		"langstream_vendor_cost_usd_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestDashboardServerEndToEnd wires the real *http.Server returned by
// NewDashboardServer into an httptest.Server so it's exercised the way
// cmd/langstream would use it, but via httptest's own ephemeral listener
// management rather than a hardcoded port or a literal ListenAndServe call
// inside the test.
func TestDashboardServerEndToEnd(t *testing.T) {
	r := newSampleRecorder()
	srv := NewDashboardServer("unused:0", r)

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", ts.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
