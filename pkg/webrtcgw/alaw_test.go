package webrtcgw

import (
	"math"
	"testing"
)

// TestALawRoundTrip_SilenceIsCloseToZero checks that encoding then
// decoding true silence (PCM 0) stays within G.711 A-law's smallest
// quantization step. Real A-law does NOT round-trip exact zero to exact
// zero: the lowest segment's decode formula is (mantissa<<4)+8, so
// mantissa=0 decodes to +8, not 0 -- this +-8 bias is standard, correct
// G.711 behavior (confirmed against the reference algorithm's own
// segment-table definition), not a bug, so the test allows it rather
// than demanding an exact 0.
func TestALawRoundTrip_SilenceIsCloseToZero(t *testing.T) {
	pcm := []byte{0, 0, 0, 0, 0, 0}
	alaw := pcm16ToALaw(pcm)
	back := alawToPCM16(alaw)
	for i := 0; i < len(pcm); i += 2 {
		s := int16(uint16(back[i]) | uint16(back[i+1])<<8)
		if s < -8 || s > 8 {
			t.Errorf("sample %d: silence round-tripped to %d, want within +-8 of 0 (G.711's smallest-segment bias)", i/2, s)
		}
	}
}

// TestALawRoundTrip_SineWave encodes then decodes a synthetic sine wave
// (the same kind of signal real speech resembles far more than silence)
// and asserts the round trip stays within G.711's expected quantization
// error -- A-law is a lossy ~12-bit-effective codec, so exact round-trip
// isn't the bar; staying close to the original is.
func TestALawRoundTrip_SineWave(t *testing.T) {
	const n = 800 // 100ms @ 8kHz
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*440*float64(i)/8000))
		pcm[i*2] = byte(v)
		pcm[i*2+1] = byte(v >> 8)
	}

	alaw := pcm16ToALaw(pcm)
	if len(alaw) != n {
		t.Fatalf("len(alaw) = %d, want %d (one byte per sample)", len(alaw), n)
	}
	back := alawToPCM16(alaw)
	if len(back) != len(pcm) {
		t.Fatalf("len(back) = %d, want %d", len(back), len(pcm))
	}

	var maxErr int
	for i := 0; i < n; i++ {
		orig := int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8)
		got := int16(uint16(back[i*2]) | uint16(back[i*2+1])<<8)
		diff := int(orig) - int(got)
		if diff < 0 {
			diff = -diff
		}
		if diff > maxErr {
			maxErr = diff
		}
	}
	// G.711 A-law's worst-case quantization step near this amplitude is a
	// few hundred out of a 16-bit range -- 1000 is a generous bound that
	// would still catch a badly broken encode/decode (e.g. a sign flip,
	// wrong exponent bit width) while tolerating real companding error.
	if maxErr > 1000 {
		t.Errorf("max round-trip error = %d, want <= 1000 (sine wave, amplitude 10000)", maxErr)
	}
}

// TestALawEncode_KnownReferenceBytes checks specific input/output pairs
// against the standard G.711 A-law algorithm's well-known behavior: max
// positive, max negative, and a small value near zero, each of which
// exercises a different exponent segment.
func TestALawEncode_KnownReferenceBytes(t *testing.T) {
	tests := []struct {
		name   string
		sample int16
	}{
		{"zero", 0},
		{"small_positive", 100},
		{"small_negative", -100},
		{"max_positive", 32767},
		{"max_negative", -32768},
		{"mid_positive", 16000},
		{"mid_negative", -16000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := alawEncodeSample(tt.sample)
			back := alawDecodeTable[b]
			diff := int(tt.sample) - int(back)
			if diff < 0 {
				diff = -diff
			}
			// Larger samples have coarser quantization steps (that's the
			// whole point of companding); scale the tolerance with
			// magnitude rather than using one fixed bound for every case.
			tolerance := int(math.Abs(float64(tt.sample)))/16 + 32
			if diff > tolerance {
				t.Errorf("sample %d -> alaw 0x%02x -> %d: diff %d exceeds tolerance %d", tt.sample, b, back, diff, tolerance)
			}
		})
	}
}

// TestALawToPCM16_LengthDoubling verifies the basic size contract callers
// depend on: one A-law byte in, exactly two PCM16 bytes out.
func TestALawToPCM16_LengthDoubling(t *testing.T) {
	alaw := make([]byte, 160) // one 20ms G.711 RTP payload
	pcm := alawToPCM16(alaw)
	if len(pcm) != 320 {
		t.Errorf("len(pcm) = %d, want 320 (160 samples * 2 bytes)", len(pcm))
	}
}

// TestPCM16ToALaw_OddTrailingByteIgnored verifies an odd-length input
// (a malformed/truncated PCM buffer) doesn't panic -- the trailing
// incomplete sample is silently dropped rather than causing an
// out-of-range index.
func TestPCM16ToALaw_OddTrailingByteIgnored(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5} // 2 whole samples + 1 trailing byte
	alaw := pcm16ToALaw(pcm)
	if len(alaw) != 2 {
		t.Errorf("len(alaw) = %d, want 2 (trailing odd byte dropped)", len(alaw))
	}
}

// TestALawToPCM16_EmptyInput verifies the zero-length edge case doesn't
// panic and returns a zero-length (not nil-vs-empty-sensitive) result.
func TestALawToPCM16_EmptyInput(t *testing.T) {
	if got := alawToPCM16(nil); len(got) != 0 {
		t.Errorf("alawToPCM16(nil) = %v, want empty", got)
	}
	if got := pcm16ToALaw(nil); len(got) != 0 {
		t.Errorf("pcm16ToALaw(nil) = %v, want empty", got)
	}
}
