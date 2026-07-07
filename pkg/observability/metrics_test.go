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
