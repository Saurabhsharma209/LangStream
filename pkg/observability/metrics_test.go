package observability

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestRecordAndSnapshot(t *testing.T) {
	r := NewLatencyRecorder()

	r.Record("asr_first_chunk", 100)
	r.Record("asr_first_chunk", 200)
	r.Record("mt", 50)

	snap := r.Snapshot()

	if got, want := len(snap["asr_first_chunk"]), 2; got != want {
		t.Fatalf("asr_first_chunk sample count = %d, want %d", got, want)
	}
	if got, want := snap["asr_first_chunk"][0], 100.0; got != want {
		t.Errorf("asr_first_chunk[0] = %v, want %v", got, want)
	}
	if got, want := snap["asr_first_chunk"][1], 200.0; got != want {
		t.Errorf("asr_first_chunk[1] = %v, want %v", got, want)
	}
	if got, want := len(snap["mt"]), 1; got != want {
		t.Fatalf("mt sample count = %d, want %d", got, want)
	}

	// Mutating the snapshot must not affect the recorder's internal state.
	snap["asr_first_chunk"][0] = 999
	fresh := r.Snapshot()
	if fresh["asr_first_chunk"][0] != 100 {
		t.Errorf("Snapshot leaked internal state: got %v after external mutation, want 100", fresh["asr_first_chunk"][0])
	}
}

func TestSnapshotEmptyStage(t *testing.T) {
	r := NewLatencyRecorder()
	snap := r.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %v", snap)
	}
	if got := r.Percentile("nonexistent", 50); got != 0 {
		t.Errorf("Percentile on empty stage = %v, want 0", got)
	}
}

func TestPercentileKnownValues(t *testing.T) {
	r := NewLatencyRecorder()
	// 1..10 ms samples for stage "tts_first_chunk".
	for i := 1; i <= 10; i++ {
		r.Record("tts_first_chunk", float64(i))
	}

	// With linear interpolation over sorted [1..10] (0-indexed rank):
	// p50 -> rank = 0.5*9 = 4.5 -> between values[4]=5 and values[5]=6 -> 5.5
	// p95 -> rank = 0.95*9 = 8.55 -> between values[8]=9 and values[9]=10 -> 9.55
	// p100 -> 10
	// p0 -> 1
	cases := []struct {
		p    float64
		want float64
	}{
		{0, 1},
		{50, 5.5},
		{95, 9.55},
		{100, 10},
	}

	for _, c := range cases {
		got := r.Percentile("tts_first_chunk", c.p)
		if !almostEqual(got, c.want, 1e-9) {
			t.Errorf("Percentile(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestPercentileSingleSample(t *testing.T) {
	r := NewLatencyRecorder()
	r.Record("total", 42.0)

	for _, p := range []float64{0, 50, 95, 99, 100} {
		got := r.Percentile("total", p)
		if got != 42.0 {
			t.Errorf("Percentile(%v) with single sample = %v, want 42", p, got)
		}
	}
}

func TestRecordStageHelper(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordStage(StageLatency{Stage: "mt", Milliseconds: 12.5})

	if got := r.Count("mt"); got != 1 {
		t.Fatalf("Count(mt) = %d, want 1", got)
	}
	if got := r.Percentile("mt", 50); got != 12.5 {
		t.Errorf("Percentile(mt, 50) = %v, want 12.5", got)
	}
}

func TestConcurrentRecord(t *testing.T) {
	r := NewLatencyRecorder()
	var wg sync.WaitGroup

	const goroutines = 50
	const perGoroutine = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				r.Record("asr_first_chunk", 1.0)
			}
		}()
	}
	wg.Wait()

	if got, want := r.Count("asr_first_chunk"), goroutines*perGoroutine; got != want {
		t.Fatalf("Count after concurrent writes = %d, want %d", got, want)
	}
}

func TestWriteTextFormat(t *testing.T) {
	r := NewLatencyRecorder()
	r.Record("asr_first_chunk", 100)
	r.Record("asr_first_chunk", 200)
	r.Record("mt", 50)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText returned error: %v", err)
	}

	out := buf.String()

	requiredSubstrings := []string{
		`# TYPE langstream_stage_latency_ms gauge`,
		`langstream_stage_latency_ms{stage="asr_first_chunk"}`,
		`langstream_stage_latency_count{stage="asr_first_chunk"} 2`,
		`langstream_stage_latency_count{stage="mt"} 1`,
		`langstream_stage_latency_sum_ms{stage="asr_first_chunk"} 300`,
		`langstream_stage_latency_ms_quantile{stage="asr_first_chunk",quantile="0.5"}`,
	}
	for _, sub := range requiredSubstrings {
		if !strings.Contains(out, sub) {
			t.Errorf("WriteText output missing expected substring %q\nfull output:\n%s", sub, out)
		}
	}

	// Every non-comment line must parse as `name{labels} value`.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed metric line %q: expected 2 whitespace-separated fields, got %d", line, len(fields))
		}
		if _, err := strconv.ParseFloat(fields[1], 64); err != nil {
			t.Fatalf("malformed metric value in line %q: %v", line, err)
		}
		if !strings.Contains(fields[0], "{") || !strings.HasSuffix(fields[0], "}") {
			t.Fatalf("metric name/labels field %q does not look like name{labels}", fields[0])
		}
	}
}

func TestWriteTextEmptyRecorder(t *testing.T) {
	r := NewLatencyRecorder()
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText returned error: %v", err)
	}
	// Should not panic and should still emit HELP/TYPE headers even with no data.
	if !strings.Contains(buf.String(), "# HELP langstream_stage_latency_ms") {
		t.Errorf("expected HELP header even with no samples, got:\n%s", buf.String())
	}
}

func TestRecordEventAndErrorRate(t *testing.T) {
	r := NewLatencyRecorder()

	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordError("asr_first_chunk", "deepgram")

	if got, want := r.EventCount("asr_first_chunk", "deepgram"), int64(4); got != want {
		t.Fatalf("EventCount = %d, want %d", got, want)
	}
	if got, want := r.ErrorCount("asr_first_chunk", "deepgram"), int64(1); got != want {
		t.Fatalf("ErrorCount = %d, want %d", got, want)
	}
	if got, want := r.ErrorRate("asr_first_chunk", "deepgram"), 0.25; !almostEqual(got, want, 1e-9) {
		t.Errorf("ErrorRate = %v, want %v", got, want)
	}

	// A different vendor on the same stage is tracked independently.
	r.RecordEvent("asr_first_chunk", "google-stt")
	if got, want := r.ErrorRate("asr_first_chunk", "google-stt"), 0.0; got != want {
		t.Errorf("ErrorRate for untouched vendor = %v, want %v", got, want)
	}
}

func TestErrorRateNoEvents(t *testing.T) {
	r := NewLatencyRecorder()
	if got := r.ErrorRate("nonexistent", "nobody"); got != 0 {
		t.Errorf("ErrorRate with no events = %v, want 0", got)
	}
	if got := r.ErrorCount("nonexistent", "nobody"); got != 0 {
		t.Errorf("ErrorCount with no events = %v, want 0", got)
	}
	if got := r.EventCount("nonexistent", "nobody"); got != 0 {
		t.Errorf("EventCount with no events = %v, want 0", got)
	}
}

func TestErrorSnapshotSortedAndIsolated(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordEvent("tts", "elevenlabs")
	r.RecordError("asr_first_chunk", "deepgram")
	r.RecordEvent("asr_first_chunk", "azure")

	snap := r.ErrorSnapshot()
	if len(snap) != 3 {
		t.Fatalf("ErrorSnapshot len = %d, want 3", len(snap))
	}
	// Sorted by stage then vendor: asr_first_chunk/azure, asr_first_chunk/deepgram, tts/elevenlabs
	if snap[0].Stage != "asr_first_chunk" || snap[0].Vendor != "azure" {
		t.Errorf("snap[0] = %+v, want stage=asr_first_chunk vendor=azure", snap[0])
	}
	if snap[1].Stage != "asr_first_chunk" || snap[1].Vendor != "deepgram" {
		t.Errorf("snap[1] = %+v, want stage=asr_first_chunk vendor=deepgram", snap[1])
	}
	if snap[2].Stage != "tts" || snap[2].Vendor != "elevenlabs" {
		t.Errorf("snap[2] = %+v, want stage=tts vendor=elevenlabs", snap[2])
	}
}

func TestRecordCostAndTotals(t *testing.T) {
	r := NewLatencyRecorder()

	r.RecordCost("deepgram", 0.05)
	r.RecordCost("deepgram", 0.025)
	r.RecordCost("openai-tts", 1.20)

	if got, want := r.CostTotal("deepgram"), 0.075; !almostEqual(got, want, 1e-9) {
		t.Errorf("CostTotal(deepgram) = %v, want %v", got, want)
	}
	if got, want := r.CostEventCount("deepgram"), int64(2); got != want {
		t.Errorf("CostEventCount(deepgram) = %d, want %d", got, want)
	}
	if got, want := r.CostTotal("openai-tts"), 1.20; !almostEqual(got, want, 1e-9) {
		t.Errorf("CostTotal(openai-tts) = %v, want %v", got, want)
	}
	if got := r.CostTotal("nonexistent-vendor"); got != 0 {
		t.Errorf("CostTotal(nonexistent-vendor) = %v, want 0", got)
	}
}

func TestCostPerMinute(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordCost("deepgram", 0.05)
	r.RecordCost("deepgram", 0.05)

	// $0.10 total over a 30s call -> $0.20/minute.
	if got, want := r.CostPerMinute("deepgram", 30), 0.20; !almostEqual(got, want, 1e-9) {
		t.Errorf("CostPerMinute(deepgram, 30s) = %v, want %v", got, want)
	}

	// Zero or negative duration is undefined -> 0, not a divide-by-zero panic.
	if got := r.CostPerMinute("deepgram", 0); got != 0 {
		t.Errorf("CostPerMinute with 0 duration = %v, want 0", got)
	}
	if got := r.CostPerMinute("deepgram", -5); got != 0 {
		t.Errorf("CostPerMinute with negative duration = %v, want 0", got)
	}
}

func TestCostSnapshotSorted(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordCost("openai-tts", 1.0)
	r.RecordCost("deepgram", 0.5)

	snap := r.CostSnapshot()
	if len(snap) != 2 {
		t.Fatalf("CostSnapshot len = %d, want 2", len(snap))
	}
	if snap[0].Vendor != "deepgram" || snap[1].Vendor != "openai-tts" {
		t.Fatalf("CostSnapshot not sorted by vendor: %+v", snap)
	}
}

func TestWriteTextIncludesErrorAndCostMetrics(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordEvent("asr_first_chunk", "deepgram")
	r.RecordError("asr_first_chunk", "deepgram")
	r.RecordCost("deepgram", 0.42)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText returned error: %v", err)
	}
	out := buf.String()

	for _, sub := range []string{
		`langstream_stage_errors_total{stage="asr_first_chunk",vendor="deepgram"} 1`,
		`langstream_stage_events_total{stage="asr_first_chunk",vendor="deepgram"} 2`,
		`langstream_stage_error_rate{stage="asr_first_chunk",vendor="deepgram"} 0.5`,
		`langstream_vendor_cost_usd_total{vendor="deepgram"} 0.42`,
		`langstream_vendor_cost_events_total{vendor="deepgram"} 1`,
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("WriteText output missing expected substring %q\nfull output:\n%s", sub, out)
		}
	}

	// Every non-comment line must still parse as `name{labels} value`,
	// matching the pre-existing latency lines' conventions.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed metric line %q: expected 2 whitespace-separated fields, got %d", line, len(fields))
		}
		if _, err := strconv.ParseFloat(fields[1], 64); err != nil {
			t.Fatalf("malformed metric value in line %q: %v", line, err)
		}
	}
}

func TestConcurrentErrorAndCostRecording(t *testing.T) {
	r := NewLatencyRecorder()
	var wg sync.WaitGroup

	const goroutines = 50
	const perGoroutine = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if j%5 == 0 {
					r.RecordError("asr_first_chunk", "deepgram")
				} else {
					r.RecordEvent("asr_first_chunk", "deepgram")
				}
				r.RecordCost("deepgram", 0.01)
			}
		}(i)
	}
	wg.Wait()

	wantEvents := int64(goroutines * perGoroutine)
	wantErrors := int64(goroutines * (perGoroutine / 5))

	if got := r.EventCount("asr_first_chunk", "deepgram"); got != wantEvents {
		t.Fatalf("EventCount after concurrent writes = %d, want %d", got, wantEvents)
	}
	if got := r.ErrorCount("asr_first_chunk", "deepgram"); got != wantErrors {
		t.Fatalf("ErrorCount after concurrent writes = %d, want %d", got, wantErrors)
	}
	if got, want := r.CostEventCount("deepgram"), int64(goroutines*perGoroutine); got != want {
		t.Fatalf("CostEventCount after concurrent writes = %d, want %d", got, want)
	}
	wantCost := float64(goroutines*perGoroutine) * 0.01
	if got := r.CostTotal("deepgram"); !almostEqual(got, wantCost, 1e-6) {
		t.Fatalf("CostTotal after concurrent writes = %v, want %v", got, wantCost)
	}
}

func TestRecordErrorReasonBackwardCompatibleWithRecordError(t *testing.T) {
	r := NewLatencyRecorder()

	// Plain RecordError (used by every pre-existing call site, e.g.
	// pkg/langstream/fallback.go's recordFallback) must behave exactly as
	// before: it contributes to ErrorCount/EventCount/ErrorRate, but never
	// shows up in ReasonSnapshot, since it carries no reason.
	r.RecordError("mt", "gpt-4o")

	if got, want := r.ErrorCount("mt", "gpt-4o"), int64(1); got != want {
		t.Fatalf("ErrorCount = %d, want %d", got, want)
	}
	if got, want := r.EventCount("mt", "gpt-4o"), int64(1); got != want {
		t.Fatalf("EventCount = %d, want %d", got, want)
	}
	if got := r.ReasonCount("mt", "gpt-4o", ""); got != 0 {
		t.Errorf("ReasonCount for empty reason = %d, want 0 (plain RecordError must not populate reasons)", got)
	}
	if snap := r.ReasonSnapshot(); len(snap) != 0 {
		t.Errorf("ReasonSnapshot after plain RecordError = %+v, want empty", snap)
	}
}

func TestRecordErrorReasonDistinguishesCircuitOpenFromOrdinaryError(t *testing.T) {
	r := NewLatencyRecorder()

	// Simulate: two ordinary vendor failures, then three circuit-breaker
	// fast-fails while the breaker is open (see pkg/translate/
	// circuitbreaker.go's errCircuitOpen -- this test exercises the
	// observability-side contract that would let a caller like
	// pkg/langstream/fallback.go tag those fast-fails distinctly).
	r.RecordError("mt", "gpt-4o")
	r.RecordError("mt", "gpt-4o")
	r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
	r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
	r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
	r.RecordEvent("mt", "gpt-4o")

	// The aggregate error/event view is unaffected by reason tagging:
	// all 5 failures count toward Errors, all 6 calls toward Total.
	stats := r.ErrorSnapshot()
	if len(stats) != 1 {
		t.Fatalf("ErrorSnapshot len = %d, want 1", len(stats))
	}
	if stats[0].Errors != 5 || stats[0].Total != 6 {
		t.Fatalf("ErrorSnapshot[0] = %+v, want Errors=5 Total=6", stats[0])
	}

	// But ReasonSnapshot lets a dashboard separate the 3 confirmed-down
	// fast-fails from the 2 ordinary per-call failures -- which is exactly
	// the distinction that's invisible from ErrorSnapshot/ErrorRate alone.
	if got, want := r.ReasonCount("mt", "gpt-4o", "circuit_open"), int64(3); got != want {
		t.Fatalf("ReasonCount(circuit_open) = %d, want %d", got, want)
	}

	reasons := r.ReasonSnapshot()
	if len(reasons) != 1 {
		t.Fatalf("ReasonSnapshot len = %d, want 1, got %+v", len(reasons), reasons)
	}
	rs := reasons[0]
	if rs.Stage != "mt" || rs.Vendor != "gpt-4o" || rs.Reason != "circuit_open" || rs.Count != 3 {
		t.Errorf("ReasonSnapshot[0] = %+v, want {mt gpt-4o circuit_open 3}", rs)
	}
}

func TestReasonSnapshotSortedAndIsolated(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordErrorReason("tts", "elevenlabs", "circuit_open")
	r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
	r.RecordErrorReason("mt", "gpt-4o", "timeout")
	// A different vendor/stage combination is tracked independently and
	// plain RecordError never leaks into the reason breakdown.
	r.RecordError("mt", "gpt-4o")

	snap := r.ReasonSnapshot()
	if len(snap) != 3 {
		t.Fatalf("ReasonSnapshot len = %d, want 3, got %+v", len(snap), snap)
	}
	// Sorted by stage, then vendor, then reason:
	// mt/gpt-4o/circuit_open, mt/gpt-4o/timeout, tts/elevenlabs/circuit_open
	if snap[0].Stage != "mt" || snap[0].Vendor != "gpt-4o" || snap[0].Reason != "circuit_open" {
		t.Errorf("snap[0] = %+v, want mt/gpt-4o/circuit_open", snap[0])
	}
	if snap[1].Stage != "mt" || snap[1].Vendor != "gpt-4o" || snap[1].Reason != "timeout" {
		t.Errorf("snap[1] = %+v, want mt/gpt-4o/timeout", snap[1])
	}
	if snap[2].Stage != "tts" || snap[2].Vendor != "elevenlabs" || snap[2].Reason != "circuit_open" {
		t.Errorf("snap[2] = %+v, want tts/elevenlabs/circuit_open", snap[2])
	}
}

func TestReasonCountUnknownIsZero(t *testing.T) {
	r := NewLatencyRecorder()
	if got := r.ReasonCount("nonexistent", "nobody", "circuit_open"); got != 0 {
		t.Errorf("ReasonCount for unknown triple = %d, want 0", got)
	}
}

func TestWriteTextIncludesReasonMetric(t *testing.T) {
	r := NewLatencyRecorder()
	r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
	r.RecordError("mt", "gpt-4o")

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"langstream_stage_error_reason_total",
		`langstream_stage_error_reason_total{stage="mt",vendor="gpt-4o",reason="circuit_open"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteText output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestConcurrentRecordErrorReason(t *testing.T) {
	r := NewLatencyRecorder()
	const goroutines = 10
	const perGoroutine = 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				r.RecordErrorReason("mt", "gpt-4o", "circuit_open")
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine)
	if got := r.ReasonCount("mt", "gpt-4o", "circuit_open"); got != want {
		t.Fatalf("ReasonCount after concurrent writes = %d, want %d", got, want)
	}
	if got := r.ErrorCount("mt", "gpt-4o"); got != want {
		t.Fatalf("ErrorCount after concurrent writes = %d, want %d", got, want)
	}
}
