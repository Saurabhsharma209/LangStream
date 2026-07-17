// Week 3 graceful-degradation behavior (ROADMAP.md: "Fallback behavior:
// what happens when translation lags, a leg drops, or confidence is low
// (never silently mistranslate — degrade gracefully, e.g. pass through
// original audio with a warning tone)").
//
// The orchestrator (session.go's runLeg) never silently drops or
// mistranslates a final ASR transcript. Instead, on any of the three
// triggers below it falls back to forwarding the *original*
// source-language audio for that utterance to the listening party
// (optionally preceded by a short synthetic warning tone — see
// generateDegradeTone), and records the event so other layers can see it
// happened:
//
//  1. Low ASR confidence: tr.Confidence < FallbackConfig.ConfidenceThreshold.
//  2. The Translator returns an error, or exceeds FallbackConfig.TranslateTimeout,
//     for one utterance.
//  3. The Synthesizer returns an error, or never starts producing audio
//     within FallbackConfig.SynthesizeTimeout, for one utterance.
//
// A leg (the caller->agent or agent->caller translation pipeline — not
// the underlying ASR socket, which today's ASR backends already
// reconnect/retry internally, see pkg/asr/backoff.go) additionally becomes
// *permanently* degraded — every subsequent utterance on that leg is
// passed through without even attempting MT/TTS — once it accumulates
// FallbackConfig.MaxConsecutiveFailures consecutive MT/TTS failures, or
// the moment any single error implements FatalError and reports
// Fatal() == true. That models ROADMAP.md's "a leg drops ... backend
// returns a fatal, non-retryable error" without requiring pkg/asr,
// pkg/translate, or pkg/tts (owned by other workstreams this sprint) to
// grow a new shared error type: any backend can opt in later just by
// implementing FatalError on its error values. None of today's backends
// (mock, Deepgram, Sarvam, GPT-4o, Cartesia) implement it yet, so in
// practice MaxConsecutiveFailures is what actually detects "permanently
// unavailable" backends right now — that's expected and fine.
package langstream

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// FallbackConfig configures LangStream's graceful-degradation behavior.
//
// A completely zero-value FallbackConfig (i.e. SessionConfig.Fallback left
// untouched) is filled in entirely by DefaultFallbackConfig at Session
// construction, so existing callers get sane defaults for free. If you set
// *any* field explicitly, set DegradeToneEnabled explicitly too: Go can't
// distinguish "left unset" from "explicitly set to false" for a bool once
// the struct is no longer the zero value, so NewSession only applies the
// tone-enabled default when the whole struct was untouched (see
// FallbackConfig.withDefaults in session.go).
type FallbackConfig struct {
	// ConfidenceThreshold is the minimum asr.Transcript.Confidence below
	// which a final transcript is passed through untranslated instead of
	// translated. A Confidence of exactly 0 is treated as "unknown/not
	// reported" (some backends don't report it) and is never treated as
	// low-confidence on its own. Default 0.55.
	ConfidenceThreshold float64

	// DegradeToneEnabled, if true, prepends a short synthetic warning
	// tone (see generateDegradeTone) to every degraded (passed-through)
	// audio segment, so the listening party has an audible cue that the
	// segment was not translated. Default true.
	DegradeToneEnabled bool

	// TranslateTimeout bounds a single Translator.Translate call for one
	// utterance. Exceeding it is treated the same as Translate returning
	// an error. Default 2s.
	TranslateTimeout time.Duration

	// SynthesizeTimeout bounds how long a single Synthesizer.SynthesizeStream
	// call is given to deliver its *first* audio chunk. Exceeding it is
	// treated the same as SynthesizeStream returning an error. A stream
	// that starts producing chunks within budget is allowed to run to
	// completion afterward — a long-but-flowing synthesis is not cut off
	// just because its *total* duration exceeds SynthesizeTimeout, only a
	// backend that never starts responding is treated as failed. Default 3s.
	SynthesizeTimeout time.Duration

	// MaxConsecutiveFailures is how many consecutive MT/TTS failures on
	// one leg (translate error/timeout or synthesize error/timeout) mark
	// that leg permanently degraded for the rest of the call, so the
	// session stops paying the TranslateTimeout/SynthesizeTimeout cost on
	// every single utterance against a backend that has gone away. A
	// single FatalError degrades the leg immediately regardless of this
	// count. Default 3.
	MaxConsecutiveFailures int

	// Metrics, if set, receives a RecordEvent/RecordError call — using
	// pkg/observability's existing exported API (see
	// pkg/observability/metrics.go's LatencyRecorder.RecordEvent /
	// RecordError, both pre-existing, unmodified by this change) — every
	// time a fallback decision is made or a leg permanently degrades.
	// Stage names used: "asr_confidence", "translate", "tts",
	// "leg_degraded". If left nil, Session creates its own recorder (see
	// Session.Metrics) so fallback events are never silently lost; pass a
	// shared recorder here instead if you want fallback events from
	// multiple sessions aggregated in one place.
	//
	// Done (2026-07-15): cmd/langstream/main.go and duplex.go already
	// pass Session.Metrics() straight into observability.NewDashboardServer,
	// and this field defaults to the Session's own recorder when nil, so
	// fallback/degrade events recorded here land in the same recorder the
	// dashboard reads from — no separate wiring needed.
	Metrics *observability.LatencyRecorder
}

// DefaultFallbackConfig returns FallbackConfig's documented defaults.
func DefaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		ConfidenceThreshold:    0.55,
		DegradeToneEnabled:     true,
		TranslateTimeout:       2 * time.Second,
		SynthesizeTimeout:      3 * time.Second,
		MaxConsecutiveFailures: 3,
	}
}

// withDefaults returns a copy of cfg with every unset (zero-value) numeric
// field filled in from DefaultFallbackConfig, so a caller can set just the
// one field they care about and get sane defaults everywhere else.
// DegradeToneEnabled and Metrics are left exactly as given: see the
// FallbackConfig doc comment for why the bool can't be defaulted here.
func (cfg FallbackConfig) withDefaults() FallbackConfig {
	d := DefaultFallbackConfig()
	if cfg.ConfidenceThreshold <= 0 {
		cfg.ConfidenceThreshold = d.ConfidenceThreshold
	}
	if cfg.TranslateTimeout <= 0 {
		cfg.TranslateTimeout = d.TranslateTimeout
	}
	if cfg.SynthesizeTimeout <= 0 {
		cfg.SynthesizeTimeout = d.SynthesizeTimeout
	}
	if cfg.MaxConsecutiveFailures <= 0 {
		cfg.MaxConsecutiveFailures = d.MaxConsecutiveFailures
	}
	return cfg
}

// FatalError is an interface an ASR/MT/TTS backend's error values can
// optionally implement to signal "this failure is permanent for the rest
// of the call" (e.g. invalid credentials, an unsupported language
// negotiated mid-call) as opposed to "transient, worth trying again on the
// next utterance" (e.g. one dropped network frame). See the package doc
// comment above for why this is an opt-in extension point rather than a
// change to pkg/asr/pkg/translate/pkg/tts.
type FatalError interface {
	error
	Fatal() bool
}

// isFatal reports whether err (or any error it wraps) implements
// FatalError and reports Fatal() == true. A nil err, or one that doesn't
// implement FatalError at all, is never considered fatal.
func isFatal(err error) bool {
	if err == nil {
		return false
	}
	var fe FatalError
	if errors.As(err, &fe) {
		return fe.Fatal()
	}
	return false
}

// audioBufferFrames bounds how many raw PCM frames a legState's
// audioRingBuffer retains before the oldest are dropped — a hard cap on
// memory used to support passthrough, independent of how long an
// utterance runs before its final transcript arrives. At a typical 20ms
// telephony packetization interval this is ~10 seconds of audio, well
// beyond a normal utterance.
const audioBufferFrames = 500

// bufferedFrame is one raw PCM frame retained for possible passthrough.
type bufferedFrame struct {
	pcm        []byte
	sampleRate int
}

// audioRingBuffer retains the most recently pushed frames, dropping the
// oldest once it exceeds capacity. Safe for concurrent use: Session calls
// push from Push{Caller,Agent}Audio (any goroutine) and drain from the
// leg's own long-lived goroutine in runLeg.
type audioRingBuffer struct {
	mu       sync.Mutex
	frames   []bufferedFrame
	capacity int

	// utteranceAt is set to time.Now() by the first push since the last
	// drain (i.e. the first audio frame of a fresh utterance), and reset
	// to the zero Time by drain. It backs session.go's "asr_first_chunk"
	// and "total" latency instrumentation, which need to know when an
	// utterance's audio started arriving, not just when its final
	// transcript arrived.
	utteranceAt time.Time
}

func newAudioRingBuffer(capacity int) *audioRingBuffer {
	if capacity <= 0 {
		capacity = audioBufferFrames
	}
	return &audioRingBuffer{capacity: capacity}
}

// push appends one frame, copying pcm so the caller's slice can be reused
// or mutated after push returns. If this is the first frame since the
// last drain (i.e. the start of a fresh utterance), it also stamps
// utteranceAt so callers can later measure end-to-end utterance latency.
func (b *audioRingBuffer) push(pcm []byte, sampleRate int) {
	if len(pcm) == 0 {
		return
	}
	cp := make([]byte, len(pcm))
	copy(cp, pcm)

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.frames) == 0 {
		b.utteranceAt = time.Now()
	}
	b.frames = append(b.frames, bufferedFrame{pcm: cp, sampleRate: sampleRate})
	if over := len(b.frames) - b.capacity; over > 0 {
		b.frames = b.frames[over:]
	}
}

// utteranceStart returns the time the first frame of the current
// (not-yet-drained) utterance was pushed, or the zero Time if no frame
// has been pushed since the last drain. Callers should read this *before*
// calling drain (which clears it), typically right before draining the
// buffer for a just-arrived final transcript.
func (b *audioRingBuffer) utteranceStart() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.utteranceAt
}

// drain returns every frame buffered so far and empties the buffer, so the
// next utterance starts from a clean slate regardless of whether this
// utterance's audio ends up used (passthrough) or discarded (successful
// translation).
func (b *audioRingBuffer) drain() []bufferedFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.frames
	b.frames = nil
	b.utteranceAt = time.Time{}
	return out
}

// legState tracks fallback bookkeeping for one leg (caller->agent or
// agent->caller) of a Session across its lifetime.
type legState struct {
	// name identifies the leg for logging/metrics labels only
	// ("caller" or "agent" — the direction whose ASR feeds this leg).
	name string

	consecutiveFailures atomic.Int32
	degraded            atomic.Bool

	audio *audioRingBuffer
}

func newLegState(name string, bufferFrames int) *legState {
	return &legState{name: name, audio: newAudioRingBuffer(bufferFrames)}
}

// recordFailure records one MT/TTS failure for the leg and reports whether
// this call caused the leg to become newly (permanently) degraded: either
// fatal is true, or the leg has now accumulated maxConsecutive consecutive
// failures.
func (ls *legState) recordFailure(fatal bool, maxConsecutive int) bool {
	if fatal {
		return ls.degraded.CompareAndSwap(false, true)
	}
	n := ls.consecutiveFailures.Add(1)
	if maxConsecutive > 0 && int(n) >= maxConsecutive {
		return ls.degraded.CompareAndSwap(false, true)
	}
	return false
}

// recordSuccess resets the leg's consecutive-failure count after an MT+TTS
// round trip completes successfully. It does not clear degraded: once a
// leg is marked permanently degraded it stays that way for the rest of the
// call by design (ROADMAP.md: "keep passing through raw audio for it
// rather than ... silently dropping audio for the rest of the call") —
// auto-retrying a backend that already failed repeatedly mid-call is a
// judgment call better made by an operator than silently retried forever.
func (ls *legState) recordSuccess() {
	ls.consecutiveFailures.Store(0)
}

func (ls *legState) isDegraded() bool {
	return ls.degraded.Load()
}

// Fallback stage names used with FallbackConfig.Metrics's
// RecordEvent/RecordError calls (see pkg/observability/metrics.go).
const (
	stageASRConfidence = "asr_confidence"
	stageTranslate     = "translate"
	stageTTS           = "tts"
	stageLegDegraded   = "leg_degraded"
)

// Latency stage names used with LatencyRecorder.Record/RecordStage (see
// pkg/observability/metrics.go's package doc comment, which names exactly
// these four stages). Distinct from the RecordEvent/RecordError stage
// names above: those track fallback-decision success/error counts, these
// track actual elapsed-time samples for the dashboard's latency-percentile
// view (see session.go's runLeg).
const (
	stageASRFirstChunk = "asr_first_chunk"
	stageMT            = "mt"
	stageTTSFirstChunk = "tts_first_chunk"
	stageTotal         = "total"
)

// recordFallback calls rec.RecordError(stage, vendor) if rec is non-nil,
// using pkg/observability's pre-existing exported API. vendor is typically
// the relevant backend's Name(), or a leg name ("caller"/"agent") for
// stageLegDegraded, which isn't attributable to one specific backend call.
func recordFallback(rec *observability.LatencyRecorder, stage, vendor string) {
	if rec == nil {
		return
	}
	if vendor == "" {
		vendor = "unknown"
	}
	rec.RecordError(stage, vendor)
}

// recordFallbackErr behaves like recordFallback, but additionally tags
// the event with reason "circuit_open" (via RecordErrorReason) when err
// indicates the vendor client's own circuit breaker rejected the call
// locally (translate.ErrCircuitOpen / tts.ErrCircuitOpen, or an error
// wrapping either -- see errors.Is), rather than an ordinary per-request
// failure. Every other kind of err (including nil, which shouldn't
// normally happen here but is handled defensively) still records with
// the same empty reason recordFallback would have used, so this is a
// strict superset of recordFallback's behavior for non-circuit-open
// failures.
func recordFallbackErr(rec *observability.LatencyRecorder, stage, vendor string, err error) {
	if rec == nil {
		return
	}
	if vendor == "" {
		vendor = "unknown"
	}
	reason := ""
	if errors.Is(err, translate.ErrCircuitOpen) || errors.Is(err, tts.ErrCircuitOpen) {
		reason = "circuit_open"
	}
	rec.RecordErrorReason(stage, vendor, reason)
}

// recordSuccessMetric calls rec.RecordEvent(stage, vendor) if rec is
// non-nil, so RecordError's implicit denominator (see
// LatencyRecorder.ErrorRate's doc) reflects real traffic, not just
// failures.
func recordSuccessMetric(rec *observability.LatencyRecorder, stage, vendor string) {
	if rec == nil {
		return
	}
	if vendor == "" {
		vendor = "unknown"
	}
	rec.RecordEvent(stage, vendor)
}

// recordLatency calls rec.Record(stage, ms) if rec is non-nil, mirroring
// recordFallback/recordSuccessMetric's defensive nil-check so callers (and
// tests) never need to special-case a nil recorder.
func recordLatency(rec *observability.LatencyRecorder, stage string, ms float64) {
	if rec == nil {
		return
	}
	rec.Record(stage, ms)
}

// msSince returns the elapsed time since t in milliseconds, as a float64
// suitable for LatencyRecorder.Record.
func msSince(t time.Time) float64 {
	return float64(time.Since(t)) / float64(time.Millisecond)
}

// recordTotalIfStarted records a "total" (glass-to-glass) latency sample
// from start to now, unless start is the zero Time -- which happens if a
// fallback/passthrough path runs for an utterance whose audio was never
// actually pushed via Push{Caller,Agent}Audio (defensive; shouldn't
// normally happen, see audioRingBuffer.utteranceStart's doc comment).
func recordTotalIfStarted(rec *observability.LatencyRecorder, start time.Time) {
	if start.IsZero() {
		return
	}
	recordLatency(rec, stageTotal, msSince(start))
}

// Degrade-tone synthesis parameters. The tone is a short, quiet,
// deterministic sine wave — unmistakably not speech — used to cue the
// listening party that the audio immediately following is untranslated
// passthrough rather than a translation.
const (
	degradeToneHz         = 440.0 // A4
	degradeToneDurationMS = 150
	degradeToneAmplitude  = 0.2 // well below full scale so it never clips or startles
)

// generateDegradeTone synthesizes the warning tone described above as
// 16-bit little-endian mono PCM at sampleRate. It is fully deterministic
// (no randomness), so tests can assert on its exact contents.
func generateDegradeTone(sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = 8000
	}
	n := sampleRate * degradeToneDurationMS / 1000
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		sample := degradeToneAmplitude * math.Sin(2*math.Pi*degradeToneHz*float64(i)/float64(sampleRate))
		v := int16(sample * math.MaxInt16)
		buf[2*i] = byte(v)
		buf[2*i+1] = byte(v >> 8)
	}
	return buf
}

// buildPassthroughChunks converts buffered raw audio frames into the
// tts.AudioChunk sequence Session forwards to the listening party in place
// of a translation, optionally preceded by generateDegradeTone's warning
// tone. If frames is empty (e.g. a fallback triggered before any audio was
// buffered for this utterance — defensive, shouldn't normally happen), it
// still emits the tone alone when enabled, and otherwise falls back to a
// single empty final marker chunk, so the listening party (or a test)
// always observes *some* result rather than nothing distinguishable from
// "no event happened at all".
func buildPassthroughChunks(frames []bufferedFrame, toneEnabled bool) []tts.AudioChunk {
	sampleRate := 8000
	for _, f := range frames {
		if f.sampleRate > 0 {
			sampleRate = f.sampleRate
			break
		}
	}

	var chunks []tts.AudioChunk
	if toneEnabled {
		chunks = append(chunks, tts.AudioChunk{PCM: generateDegradeTone(sampleRate), SampleRate: sampleRate})
	}
	for _, f := range frames {
		chunks = append(chunks, tts.AudioChunk{PCM: f.pcm, SampleRate: f.sampleRate})
	}
	if len(chunks) == 0 {
		return []tts.AudioChunk{{SampleRate: sampleRate, IsFinal: true}}
	}
	chunks[len(chunks)-1].IsFinal = true
	return chunks
}

// chunksChannel returns an already-populated, already-closed channel
// containing chunks in order, so callers can feed a precomputed slice of
// chunks (e.g. from buildPassthroughChunks) through the same
// Session.forwardAudio path used for live-streamed TTS output.
func chunksChannel(chunks []tts.AudioChunk) <-chan tts.AudioChunk {
	ch := make(chan tts.AudioChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch
}
