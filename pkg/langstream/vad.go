package langstream

import (
	"encoding/binary"
	"math"

	"github.com/exotel/langstream/pkg/asr"
)

// DefaultVADThreshold is the default RMS energy threshold (on the 16-bit
// PCM amplitude scale, i.e. 0..32767) used to distinguish speech from
// silence when a VAD is constructed without an explicit threshold. This
// mirrors ClearStream's approach (pkg/rtp): a static RMS threshold over
// 16-bit PCM rather than a learned/adaptive model, which is cheap, has no
// external dependency, and is good enough to gate utterance boundaries for
// a Week 1 pilot.
const DefaultVADThreshold = 500.0

// DefaultSilenceHoldMS is the default amount of consecutive silence (in
// milliseconds) required before UtteranceBoundaryTracker reports an
// utterance boundary. 600ms is a common telephony VAD endpointing value:
// short enough to keep latency down, long enough to avoid cutting off
// speech during natural pauses.
const DefaultSilenceHoldMS int64 = 600

// VAD is a simple, static-threshold voice-activity detector operating on
// 16-bit little-endian mono PCM, matching the format used throughout
// LangStream (see asr.AudioFrame, tts.AudioChunk).
type VAD struct {
	// Threshold is the RMS amplitude (0..32767) above which a PCM buffer
	// is classified as speech. Values <= 0 are treated as
	// DefaultVADThreshold by NewVAD; a zero-value VAD{} instead treats a
	// zero Threshold literally (everything above silence, i.e. any
	// non-zero signal, counts as speech) so the type remains usable
	// without a constructor for the simplest test cases.
	Threshold float64
}

// NewVAD returns a VAD with the given RMS threshold. If threshold <= 0,
// DefaultVADThreshold is used instead.
func NewVAD(threshold float64) *VAD {
	if threshold <= 0 {
		threshold = DefaultVADThreshold
	}
	return &VAD{Threshold: threshold}
}

// RMS computes the root-mean-square amplitude of pcm, interpreted as
// 16-bit little-endian mono samples. It returns 0 for empty or
// malformed (odd-length) input.
func (v *VAD) RMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sumSquares float64
	for i := 0; i < n; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		s := float64(sample)
		sumSquares += s * s
	}
	return math.Sqrt(sumSquares / float64(n))
}

// IsSpeech reports whether pcm's RMS energy exceeds v.Threshold, i.e.
// whether this buffer should be classified as speech rather than silence.
func (v *VAD) IsSpeech(pcm []byte) bool {
	return v.RMS(pcm) > v.Threshold
}

// frameDurationMS estimates the duration, in milliseconds, of a 16-bit
// mono PCM buffer sampled at sampleRate. It returns 0 if sampleRate <= 0.
func frameDurationMS(pcm []byte, sampleRate int) int64 {
	if sampleRate <= 0 {
		return 0
	}
	samples := len(pcm) / 2
	return int64(samples) * 1000 / int64(sampleRate)
}

// UtteranceBoundaryTracker consumes a sequence of audio frames for a
// single stream and reports when enough consecutive silence has elapsed
// to consider the current utterance finished. It is the building block
// that lets the orchestrator (or a future RTP-level VAD gate) decide when
// to flush a partial ASR transcript as final ahead of the backend's own
// endpointing, or independently confirm the backend's finalization.
//
// UtteranceBoundaryTracker is stateful and is not safe for concurrent use
// by multiple goroutines; each stream (leg) should own its own instance.
type UtteranceBoundaryTracker struct {
	vad           *VAD
	silenceHoldMS int64

	silenceMS int64
}

// NewUtteranceBoundaryTracker returns a tracker that uses vad to classify
// individual frames and reports a boundary once silenceHoldMS of
// consecutive silence has been observed. If vad is nil, NewVAD(0) (i.e.
// DefaultVADThreshold) is used. If silenceHoldMS <= 0, DefaultSilenceHoldMS
// is used.
func NewUtteranceBoundaryTracker(vad *VAD, silenceHoldMS int64) *UtteranceBoundaryTracker {
	if vad == nil {
		vad = NewVAD(0)
	}
	if silenceHoldMS <= 0 {
		silenceHoldMS = DefaultSilenceHoldMS
	}
	return &UtteranceBoundaryTracker{vad: vad, silenceHoldMS: silenceHoldMS}
}

// Observe feeds one frame of PCM audio of the given duration into the
// tracker. It returns true exactly when this frame's silence pushes the
// tracker's consecutive-silence total at or above the configured
// silence-hold threshold (i.e. "utterance boundary reached now"); any
// speech frame resets the consecutive-silence counter to zero.
func (u *UtteranceBoundaryTracker) Observe(pcm []byte, durationMS int64) bool {
	if u.vad.IsSpeech(pcm) {
		u.silenceMS = 0
		return false
	}
	u.silenceMS += durationMS
	return u.silenceMS >= u.silenceHoldMS
}

// ObserveFrame is a convenience wrapper around Observe that derives the
// frame's duration from its sample rate and PCM length, so callers working
// directly with asr.AudioFrame don't need to compute duration themselves.
func (u *UtteranceBoundaryTracker) ObserveFrame(frame asr.AudioFrame) bool {
	return u.Observe(frame.PCM, frameDurationMS(frame.PCM, frame.SampleRate))
}

// SilenceMS returns the current consecutive-silence total, in
// milliseconds, accumulated since the last speech frame (or since
// construction/Reset if no speech has been observed yet).
func (u *UtteranceBoundaryTracker) SilenceMS() int64 {
	return u.silenceMS
}

// Reset clears the tracker's consecutive-silence counter, e.g. after the
// caller has acted on a reported boundary and started a new utterance.
func (u *UtteranceBoundaryTracker) Reset() {
	u.silenceMS = 0
}
