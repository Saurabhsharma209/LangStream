package langstream

import (
	"encoding/binary"
	"testing"

	"github.com/exotel/langstream/pkg/asr"
)

// pcmFromSamples encodes samples as 16-bit little-endian mono PCM.
func pcmFromSamples(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], uint16(s))
	}
	return buf
}

// constantSamples returns n samples all equal to amplitude, alternating
// sign so the signal isn't DC (RMS of a genuinely constant DC signal is
// still just |amplitude|, but alternating is closer to real audio and
// avoids relying on that coincidence).
func constantSamples(n int, amplitude int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = amplitude
		} else {
			out[i] = -amplitude
		}
	}
	return out
}

func TestVADRMSSilence(t *testing.T) {
	v := NewVAD(0) // defaults to DefaultVADThreshold
	silence := pcmFromSamples(make([]int16, 160))
	if rms := v.RMS(silence); rms != 0 {
		t.Fatalf("RMS(silence) = %v, want 0", rms)
	}
	if v.IsSpeech(silence) {
		t.Fatal("IsSpeech(silence) = true, want false")
	}
}

func TestVADRMSLoudSignalIsSpeech(t *testing.T) {
	v := NewVAD(0)
	loud := pcmFromSamples(constantSamples(160, 5000))
	rms := v.RMS(loud)
	if rms < 4999 || rms > 5001 {
		t.Fatalf("RMS(loud) = %v, want ~5000", rms)
	}
	if !v.IsSpeech(loud) {
		t.Fatalf("IsSpeech(loud) = false, want true (RMS %v > threshold %v)", rms, v.Threshold)
	}
}

func TestVADCustomThreshold(t *testing.T) {
	v := &VAD{Threshold: 4000}
	quiet := pcmFromSamples(constantSamples(160, 1000))
	loud := pcmFromSamples(constantSamples(160, 4500))

	if v.IsSpeech(quiet) {
		t.Fatal("IsSpeech(quiet) = true, want false under 4000 threshold")
	}
	if !v.IsSpeech(loud) {
		t.Fatal("IsSpeech(loud) = false, want true under 4000 threshold")
	}
}

func TestVADEmptyAndOddLengthInput(t *testing.T) {
	v := NewVAD(0)
	if rms := v.RMS(nil); rms != 0 {
		t.Fatalf("RMS(nil) = %v, want 0", rms)
	}
	if rms := v.RMS([]byte{0x01}); rms != 0 {
		t.Fatalf("RMS(odd-length) = %v, want 0", rms)
	}
}

func TestUtteranceBoundaryTrackerReachesBoundaryAfterSilenceHold(t *testing.T) {
	tr := NewUtteranceBoundaryTracker(NewVAD(1000), 100) // 100ms silence hold
	silence := pcmFromSamples(make([]int16, 160))

	if tr.Observe(silence, 40) {
		t.Fatal("boundary reached too early at 40ms silence")
	}
	if tr.Observe(silence, 40) {
		t.Fatal("boundary reached too early at 80ms silence")
	}
	if !tr.Observe(silence, 40) {
		t.Fatal("boundary not reached at 120ms silence (>= 100ms hold)")
	}
	if got := tr.SilenceMS(); got != 120 {
		t.Fatalf("SilenceMS() = %d, want 120", got)
	}
}

func TestUtteranceBoundaryTrackerResetsOnSpeech(t *testing.T) {
	tr := NewUtteranceBoundaryTracker(NewVAD(1000), 100)
	silence := pcmFromSamples(make([]int16, 160))
	loud := pcmFromSamples(constantSamples(160, 5000))

	tr.Observe(silence, 80)
	if got := tr.SilenceMS(); got != 80 {
		t.Fatalf("SilenceMS() after 80ms silence = %d, want 80", got)
	}

	if tr.Observe(loud, 40) {
		t.Fatal("speech frame must never report a boundary")
	}
	if got := tr.SilenceMS(); got != 0 {
		t.Fatalf("SilenceMS() after speech = %d, want 0 (reset)", got)
	}

	// Silence hold must restart from zero, not continue where it left off.
	if tr.Observe(silence, 80) {
		t.Fatal("boundary reached too early after reset")
	}
	if !tr.Observe(silence, 40) {
		t.Fatal("boundary not reached after accumulating 120ms since reset")
	}
}

func TestUtteranceBoundaryTrackerReset(t *testing.T) {
	tr := NewUtteranceBoundaryTracker(NewVAD(1000), 100)
	silence := pcmFromSamples(make([]int16, 160))

	tr.Observe(silence, 90)
	if tr.SilenceMS() != 90 {
		t.Fatalf("SilenceMS() = %d, want 90", tr.SilenceMS())
	}
	tr.Reset()
	if tr.SilenceMS() != 0 {
		t.Fatalf("SilenceMS() after Reset = %d, want 0", tr.SilenceMS())
	}
}

func TestUtteranceBoundaryTrackerObserveFrame(t *testing.T) {
	tr := NewUtteranceBoundaryTracker(NewVAD(1000), 100)

	// 8kHz mono, 16-bit: 800 samples = 1600 bytes = 100ms.
	frame := asr.AudioFrame{
		PCM:        pcmFromSamples(make([]int16, 800)),
		SampleRate: 8000,
	}

	if !tr.ObserveFrame(frame) {
		t.Fatal("expected boundary reached after a single 100ms silent frame")
	}
}

func TestNewVADDefaultsThreshold(t *testing.T) {
	v := NewVAD(-5)
	if v.Threshold != DefaultVADThreshold {
		t.Fatalf("NewVAD(-5).Threshold = %v, want DefaultVADThreshold (%v)", v.Threshold, DefaultVADThreshold)
	}
}

func TestNewUtteranceBoundaryTrackerDefaults(t *testing.T) {
	tr := NewUtteranceBoundaryTracker(nil, 0)
	if tr.vad == nil {
		t.Fatal("expected default VAD to be created when nil is passed")
	}
	if tr.silenceHoldMS != DefaultSilenceHoldMS {
		t.Fatalf("silenceHoldMS = %d, want DefaultSilenceHoldMS (%d)", tr.silenceHoldMS, DefaultSilenceHoldMS)
	}
}
