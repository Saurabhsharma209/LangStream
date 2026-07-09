// Package observability provides latency, error-rate, and per-vendor-cost
// instrumentation for the LangStream pipeline. It is intentionally
// dependency-free (standard library only) for Week 1 so it doesn't force a
// go.sum on packages other agents are still scaffolding. Week 2+ can swap
// WriteText's output for a real prometheus/client_golang registry without
// changing the recording API.
//
// Week 3 adds error-rate tracking (RecordError/RecordEvent/ErrorRate) and
// per-vendor cost tracking (RecordCost/CostTotal/CostPerMinute) on top of
// the existing latency Recorder, plus a minimal human-readable dashboard
// (see dashboard.go) that renders all three alongside each other.
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

	// errorEvents/totalEvents back RecordError/RecordEvent/ErrorRate.
	// totalEvents counts every RecordError + RecordEvent call per
	// (stage, vendor); errorEvents counts only the RecordError calls.
	errorEvents map[errorKey]int64
	totalEvents map[errorKey]int64

	// costTotals/costEvents back RecordCost/CostTotal/CostPerMinute,
	// keyed by vendor.
	costTotals map[string]float64
	costEvents map[string]int64
}

// NewLatencyRecorder returns a ready-to-use LatencyRecorder.
func NewLatencyRecorder() *LatencyRecorder {
	return &LatencyRecorder{
		samples:     make(map[string][]float64),
		errorEvents: make(map[errorKey]int64),
		totalEvents: make(map[errorKey]int64),
		costTotals:  make(map[string]float64),
		costEvents:  make(map[string]int64),
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

// errorKey identifies a (pipeline stage, vendor) pair for error/event
// counting, e.g. {"asr_first_chunk", "deepgram"}.
type errorKey struct {
	Stage  string
	Vendor string
}

// ErrorStats is a point-in-time snapshot of error/event counts for a single
// (stage, vendor) pair, plus the derived error rate.
type ErrorStats struct {
	Stage  string
	Vendor string
	Errors int64
	Total  int64
	// Rate is Errors/Total (a ratio in [0,1]), or 0 if Total is 0.
	Rate float64
}

// CostStats is a point-in-time snapshot of running cost totals for a single
// vendor.
type CostStats struct {
	Vendor   string
	TotalUSD float64
	Events   int64
}

// RecordEvent records one successful (non-error) event for the given
// pipeline stage and vendor. It contributes to the denominator used by
// ErrorRate. Call this on the "happy path"; call RecordError instead (not
// additionally) when the event failed.
func (r *LatencyRecorder) RecordEvent(stage, vendor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalEvents[errorKey{Stage: stage, Vendor: vendor}]++
}

// RecordError records one failed event for the given pipeline stage and
// vendor, e.g. RecordError("asr_first_chunk", "deepgram") when a vendor call
// errors out. It counts toward both the error total and the overall event
// total (do not also call RecordEvent for the same occurrence).
func (r *LatencyRecorder) RecordError(stage, vendor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := errorKey{Stage: stage, Vendor: vendor}
	r.errorEvents[key]++
	r.totalEvents[key]++
}

// ErrorRate returns the error rate for a (stage, vendor) pair, defined as
// errors observed divided by total events observed (RecordError +
// RecordEvent calls), i.e. a ratio in [0,1]. It returns 0 if no events have
// been recorded for that pair. This is an all-time ratio over the life of
// the recorder, not a windowed rate; callers wanting a rate "per unit time"
// can sample ErrorCount/EventCount at an interval and diff themselves.
func (r *LatencyRecorder) ErrorRate(stage, vendor string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := errorKey{Stage: stage, Vendor: vendor}
	total := r.totalEvents[key]
	if total == 0 {
		return 0
	}
	return float64(r.errorEvents[key]) / float64(total)
}

// ErrorCount returns the number of RecordError calls for a (stage, vendor)
// pair.
func (r *LatencyRecorder) ErrorCount(stage, vendor string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errorEvents[errorKey{Stage: stage, Vendor: vendor}]
}

// EventCount returns the total number of events (RecordError + RecordEvent
// calls) for a (stage, vendor) pair.
func (r *LatencyRecorder) EventCount(stage, vendor string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalEvents[errorKey{Stage: stage, Vendor: vendor}]
}

// ErrorSnapshot returns a point-in-time list of ErrorStats for every
// (stage, vendor) pair that has recorded at least one event, sorted by
// stage then vendor.
func (r *LatencyRecorder) ErrorSnapshot() []ErrorStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]ErrorStats, 0, len(r.totalEvents))
	for key, total := range r.totalEvents {
		errs := r.errorEvents[key]
		var rate float64
		if total > 0 {
			rate = float64(errs) / float64(total)
		}
		out = append(out, ErrorStats{
			Stage:  key.Stage,
			Vendor: key.Vendor,
			Errors: errs,
			Total:  total,
			Rate:   rate,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].Vendor < out[j].Vendor
	})
	return out
}

// RecordCost records a cost event of amountUSD attributed to vendor,
// adding to that vendor's running total. amountUSD is typically the cost of
// a single vendor API call (e.g. ASR seconds billed, MT characters billed,
// TTS characters billed) but callers may batch as they see fit.
func (r *LatencyRecorder) RecordCost(vendor string, amountUSD float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.costTotals[vendor] += amountUSD
	r.costEvents[vendor]++
}

// CostTotal returns the running total cost in USD recorded for a vendor.
func (r *LatencyRecorder) CostTotal(vendor string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.costTotals[vendor]
}

// CostEventCount returns the number of RecordCost calls for a vendor.
func (r *LatencyRecorder) CostEventCount(vendor string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.costEvents[vendor]
}

// CostPerMinute returns the vendor's running total cost (see CostTotal)
// divided by durationSeconds/60 -- i.e. a derived $/minute rate given an
// externally-measured call/session duration. It returns 0 if
// durationSeconds <= 0, since a per-minute rate is undefined for a
// zero-or-negative duration.
func (r *LatencyRecorder) CostPerMinute(vendor string, durationSeconds float64) float64 {
	if durationSeconds <= 0 {
		return 0
	}
	total := r.CostTotal(vendor)
	minutes := durationSeconds / 60.0
	return total / minutes
}

// CostSnapshot returns a point-in-time list of CostStats for every vendor
// that has recorded at least one cost event, sorted by vendor name.
func (r *LatencyRecorder) CostSnapshot() []CostStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]CostStats, 0, len(r.costTotals))
	for vendor, total := range r.costTotals {
		out = append(out, CostStats{
			Vendor:   vendor,
			TotalUSD: total,
			Events:   r.costEvents[vendor],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
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

	errorSnap := r.ErrorSnapshot()

	b.WriteString("# HELP langstream_stage_errors_total Total error events recorded per stage/vendor.\n")
	b.WriteString("# TYPE langstream_stage_errors_total counter\n")
	for _, es := range errorSnap {
		fmt.Fprintf(&b, "langstream_stage_errors_total{stage=%q,vendor=%q} %d\n", es.Stage, es.Vendor, es.Errors)
	}

	b.WriteString("# HELP langstream_stage_events_total Total events (errors + successes) recorded per stage/vendor.\n")
	b.WriteString("# TYPE langstream_stage_events_total counter\n")
	for _, es := range errorSnap {
		fmt.Fprintf(&b, "langstream_stage_events_total{stage=%q,vendor=%q} %d\n", es.Stage, es.Vendor, es.Total)
	}

	b.WriteString("# HELP langstream_stage_error_rate Error rate (errors / total events) per stage/vendor, in [0,1].\n")
	b.WriteString("# TYPE langstream_stage_error_rate gauge\n")
	for _, es := range errorSnap {
		fmt.Fprintf(&b, "langstream_stage_error_rate{stage=%q,vendor=%q} %s\n", es.Stage, es.Vendor, formatFloat(es.Rate))
	}

	costSnap := r.CostSnapshot()

	b.WriteString("# HELP langstream_vendor_cost_usd_total Running total cost in USD recorded per vendor.\n")
	b.WriteString("# TYPE langstream_vendor_cost_usd_total counter\n")
	for _, cs := range costSnap {
		fmt.Fprintf(&b, "langstream_vendor_cost_usd_total{vendor=%q} %s\n", cs.Vendor, formatFloat(cs.TotalUSD))
	}

	b.WriteString("# HELP langstream_vendor_cost_events_total Number of cost events recorded per vendor.\n")
	b.WriteString("# TYPE langstream_vendor_cost_events_total counter\n")
	for _, cs := range costSnap {
		fmt.Fprintf(&b, "langstream_vendor_cost_events_total{vendor=%q} %d\n", cs.Vendor, cs.Events)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// formatFloat renders a float64 compactly (no trailing zeros beyond what's
// needed), matching typical Prometheus text-format conventions.
func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
