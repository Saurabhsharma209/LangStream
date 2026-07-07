// Package observability provides latency instrumentation for the LangStream
// pipeline. It is intentionally dependency-free (standard library only) for
// Week 1 so it doesn't force a go.sum on packages other agents are still
// scaffolding. Week 2+ can swap WriteText's output for a real
// prometheus/client_golang registry without changing the recording API.
package observability

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

// StageLatency is a single latency observation for a named pipeline stage,
// e.g. "asr_first_chunk", "mt", "tts_first_chunk", "total".
type StageLatency struct {
	Stage        string
	Milliseconds float64
}

// LatencyRecorder collects per-stage latency samples in a thread-safe manner
// and can report percentiles or export them in a Prometheus-text-exposition
// compatible format.
type LatencyRecorder struct {
	mu      sync.Mutex
	samples map[string][]float64
}

// NewLatencyRecorder returns a ready-to-use LatencyRecorder.
func NewLatencyRecorder() *LatencyRecorder {
	return &LatencyRecorder{
		samples: make(map[string][]float64),
	}
}

// Record adds a latency observation (in milliseconds) for the given stage.
func (r *LatencyRecorder) Record(stage string, ms float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples[stage] = append(r.samples[stage], ms)
}

// RecordStage is a convenience wrapper around Record that accepts a
// StageLatency value.
func (r *LatencyRecorder) RecordStage(sl StageLatency) {
	r.Record(sl.Stage, sl.Milliseconds)
}

// Snapshot returns a copy of all raw samples recorded so far, keyed by
// stage name. The returned map (and its slices) are safe to mutate without
// affecting the recorder's internal state.
func (r *LatencyRecorder) Snapshot() map[string][]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string][]float64, len(r.samples))
	for stage, values := range r.samples {
		cp := make([]float64, len(values))
		copy(cp, values)
		out[stage] = cp
	}
	return out
}

// Percentile computes the p-th percentile (0 <= p <= 100) of the samples
// recorded for the given stage using linear interpolation between the two
// nearest ranks (the same approach as NIST/Excel's "inclusive" method).
// It returns 0 if the stage has no samples.
func (r *LatencyRecorder) Percentile(stage string, p float64) float64 {
	r.mu.Lock()
	values := make([]float64, len(r.samples[stage]))
	copy(values, r.samples[stage])
	r.mu.Unlock()

	return percentile(values, p)
}

// percentile computes the p-th percentile of values (0 <= p <= 100) using
// linear interpolation between closest ranks. values is sorted in place.
func percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return values[0]
	}

	sort.Float64s(values)

	if p <= 0 {
		return values[0]
	}
	if p >= 100 {
		return values[n-1]
	}

	// Rank in [0, n-1] using linear interpolation.
	rank := (p / 100) * float64(n-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper {
		return values[lower]
	}

	frac := rank - float64(lower)
	return values[lower]*(1-frac) + values[upper]*frac
}

// Count returns the number of samples recorded for a stage.
func (r *LatencyRecorder) Count(stage string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.samples[stage])
}

// Stages returns the sorted list of stage names that currently have at
// least one recorded sample.
func (r *LatencyRecorder) Stages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	stages := make([]string, 0, len(r.samples))
	for stage := range r.samples {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	return stages
}

// WriteText writes a Prometheus text-exposition-format-compatible dump of
// per-stage latency metrics: count, sum, and common percentiles (p50, p90,
// p95, p99) of the observed latency-in-milliseconds gauge per stage. This
// is a drop-in scrape target shape; a real Prometheus client library can
// replace this in Week 2 without changing the recording API.
func (r *LatencyRecorder) WriteText(w io.Writer) error {
	snapshot := r.Snapshot()

	stages := make([]string, 0, len(snapshot))
	for stage := range snapshot {
		stages = append(stages, stage)
	}
	sort.Strings(stages)

	var b strings.Builder

	b.WriteString("# HELP langstream_stage_latency_ms Per-stage pipeline latency in milliseconds.\n")
	b.WriteString("# TYPE langstream_stage_latency_ms gauge\n")
	for _, stage := range stages {
		values := snapshot[stage]
		if len(values) == 0 {
			continue
		}
		latest := values[len(values)-1]
		fmt.Fprintf(&b, "langstream_stage_latency_ms{stage=%q} %s\n", stage, formatFloat(latest))
	}

	b.WriteString("# HELP langstream_stage_latency_count Number of latency samples recorded per stage.\n")
	b.WriteString("# TYPE langstream_stage_latency_count counter\n")
	for _, stage := range stages {
		fmt.Fprintf(&b, "langstream_stage_latency_count{stage=%q} %d\n", stage, len(snapshot[stage]))
	}

	b.WriteString("# HELP langstream_stage_latency_sum_ms Sum of latency samples in milliseconds per stage.\n")
	b.WriteString("# TYPE langstream_stage_latency_sum_ms counter\n")
	for _, stage := range stages {
		var sum float64
		for _, v := range snapshot[stage] {
			sum += v
		}
		fmt.Fprintf(&b, "langstream_stage_latency_sum_ms{stage=%q} %s\n", stage, formatFloat(sum))
	}

	b.WriteString("# HELP langstream_stage_latency_ms_quantile Latency percentiles in milliseconds per stage.\n")
	b.WriteString("# TYPE langstream_stage_latency_ms_quantile gauge\n")
	for _, stage := range stages {
		values := make([]float64, len(snapshot[stage]))
		copy(values, snapshot[stage])
		for _, q := range []float64{50, 90, 95, 99} {
			p := percentile(values, q)
			fmt.Fprintf(&b, "langstream_stage_latency_ms_quantile{stage=%q,quantile=\"%s\"} %s\n",
				stage, formatFloat(q/100), formatFloat(p))
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// formatFloat renders a float64 compactly (no trailing zeros beyond what's
// needed), matching typical Prometheus text-format conventions.
func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
