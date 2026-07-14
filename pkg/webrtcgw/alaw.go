// Package webrtcgw is a real-time WebRTC gateway for LangStream: it lets
// two real browser participants join a "room" over ordinary WebRTC (mic
// in, translated voice out), each speaking their own language, bridged
// through the same langstream.Session duplex orchestrator that
// pkg/rtp.DuplexSession uses for ClearStream's telephony RTP legs.
//
// # Why G.711 (PCMA), not Opus
//
// Browsers' WebRTC audio is normally Opus (48kHz, variable bitrate,
// requires a codec library to decode/encode). Decoding Opus in Go without
// cgo/libopus is real added complexity this repo doesn't need: G.711
// (PCMU/PCMA) is a *mandatory-to-implement* codec for any WebRTC-compliant
// browser (RFC 7874 -- https://www.rfc-editor.org/rfc/rfc7874.html,
// "WebRTC Audio Codec and Processing Requirements"), specifically so
// browsers can always interoperate with legacy telephony gateways. Chrome,
// Firefox, and Safari all include PCMU (payload type 0) and PCMA (payload
// type 8) in their own SDP offers by default, alongside Opus.
//
// This package's pion/webrtc MediaEngine (see peer.go) registers *only*
// PCMA for audio -- no Opus codec is registered at all. Since pion acts as
// the answerer (the browser creates the offer, this gateway answers), the
// answer's m=audio line can only select a payload type both sides
// support; restricting our side to PCMA forces negotiation onto it. The
// browser's own WebRTC engine then encodes/decodes G.711 natively -- this
// gateway never touches Opus, and the browser never has to do anything
// unusual (no setCodecPreferences, no SDP munging on the client side).
//
// G.711's A-law companding is simple 8-bit-per-sample math (see this
// file), not a real codec requiring a library: this is what makes the
// whole approach avoid needing cgo/libopus in this environment.
package webrtcgw

// alawDecodeTable is used by alawToPCM16 for exact ITU-T G.711 A-law
// decoding (an 8-bit companded sample -> 16-bit linear PCM). Generated
// once at package init via alawDecodeByte's bit-manipulation algorithm
// (the standard G.711 A-law expansion), then used as a fast lookup table
// -- this repo's audio path decodes one RTP packet's worth of samples
// (typically 160 bytes/20ms) many times a second per active leg, so a
// precomputed 256-entry table avoids repeating the same bit math for
// every sample.
var alawDecodeTable [256]int16

// alawEncodeTable mirrors alawDecodeTable for the encode direction, but
// encoding (16-bit linear -> 8-bit A-law) can't be a flat 256-entry table
// since the input domain is 65536 values; pcm16ToALaw instead calls
// alawEncodeSample directly per sample (the encode algorithm is cheap: a
// handful of comparisons and shifts, no division).
func init() {
	for i := 0; i < 256; i++ {
		alawDecodeTable[i] = alawDecodeByte(byte(i))
	}
}

// alawDecodeByte implements the standard ITU-T G.711 A-law expansion for
// one companded byte, returning the corresponding 16-bit linear PCM
// sample. This is the well-known reference algorithm (the same shape used
// by, e.g., the Asterisk/FreeSWITCH/libsndfile G.711 implementations),
// not something invented for this repo.
func alawDecodeByte(alaw byte) int16 {
	alaw ^= 0x55
	sign := alaw & 0x80
	exponent := (alaw >> 4) & 0x07
	mantissa := alaw & 0x0F

	var sample int
	if exponent == 0 {
		sample = (int(mantissa) << 4) + 8
	} else {
		sample = ((int(mantissa) << 4) + 0x108) << (exponent - 1)
	}
	if sign == 0 {
		sample = -sample
	}
	return int16(sample)
}

// alawEncodeSample implements the standard ITU-T G.711 A-law compression
// for one 16-bit linear PCM sample, returning the companded byte.
func alawEncodeSample(sample int16) byte {
	const clip = 32635 // 32767 - 132, the standard G.711 clip bound.

	s := int(sample)
	sign := byte(0x80)
	if s < 0 {
		s = -s - 1
		sign = 0
	}
	if s > clip {
		s = clip
	}

	var exponent byte
	if s >= 256 {
		// Find the exponent: the position of the highest set bit above
		// bit 7, per the standard segment-table approach.
		exponent = 7
		for mask := 0x4000; (s&mask) == 0 && exponent > 0; mask >>= 1 {
			exponent--
		}
	}

	var mantissa byte
	if exponent == 0 {
		mantissa = byte(s>>4) & 0x0F
	} else {
		mantissa = byte(s>>(exponent+3)) & 0x0F
	}

	alaw := sign | (exponent << 4) | mantissa
	return alaw ^ 0x55
}

// alawToPCM16 decodes a buffer of A-law-companded bytes (one byte per
// sample, as carried in an RTP PCMA payload) into 16-bit little-endian
// linear PCM (the format every other pkg/asr, pkg/tts, and
// pkg/langstream.Session interface in this repo already uses).
func alawToPCM16(alaw []byte) []byte {
	pcm := make([]byte, len(alaw)*2)
	for i, b := range alaw {
		s := alawDecodeTable[b]
		pcm[i*2] = byte(s)
		pcm[i*2+1] = byte(s >> 8)
	}
	return pcm
}

// pcm16ToALaw encodes a buffer of 16-bit little-endian linear PCM (as
// produced by every tts.Synthesizer in this repo) into A-law-companded
// bytes suitable for an RTP PCMA payload. pcm's length must be even
// (whole samples); a trailing odd byte, if present, is ignored rather
// than causing a panic, since some upstream buffering can hand off
// slightly uneven slices and dropping the last incomplete sample is
// harmless for a 20ms audio frame.
func pcm16ToALaw(pcm []byte) []byte {
	n := len(pcm) / 2
	alaw := make([]byte, n)
	for i := 0; i < n; i++ {
		s := int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8)
		alaw[i] = alawEncodeSample(s)
	}
	return alaw
}
