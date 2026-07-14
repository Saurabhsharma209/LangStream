package webrtcgw

import "time"

// inboundBufferDuration governs how much decoded PCM this gateway
// accumulates from a browser's 20ms RTP packets before calling
// langstream.Session.Push{Caller,Agent}Audio. 400ms was chosen
// empirically, live-verified against the real Sarvam ASR backend on
// 2026-07-14: pushing every individual 20ms packet through on its own
// worked fine for a one-shot demo (which explicitly closes the ASR
// session, and Close() sends a flush signal Sarvam responds to) but
// silently never finalized any utterance in a real, ongoing,
// never-closed room -- Sarvam's own VAD needs a large enough per-message
// audio window to detect a speech/silence transition within. 400ms adds
// a small, bounded amount of latency (worst case: an utterance's last
// few words wait up to 400ms extra before Sarvam even sees them) in
// exchange for actually working continuously for the life of a call, not
// just a single utterance. See DEVLOG.md's 2026-07-14 entry for the full
// investigation (root-caused by isolating chunk size as the only
// variable between a working and a silently-broken run against the real
// vendor).
const inboundBufferDuration = 400 * time.Millisecond

// inboundBuffer accumulates PCM16 bytes (as decoded from successive RTP
// packets) and calls onFull once at least targetBytes have accumulated,
// resetting afterward. flush() forces an early call with whatever has
// accumulated so far (even if under targetBytes) -- used when a track
// ends or the room is torn down, so the last few hundred milliseconds of
// a hung-up call's speech aren't silently dropped.
//
// Split out from peer.go's startInboundBridge into its own type
// specifically so this buffering behavior -- the actual fix for the real
// bug described in inboundBufferDuration's doc comment -- has a direct
// unit test that doesn't require a live pion TrackRemote/RTP transport.
type inboundBuffer struct {
	targetBytes int
	onFull      func(pcm []byte)
	buffered    []byte
}

// newInboundBuffer returns an inboundBuffer that calls onFull once at
// least duration's worth of 8kHz/16-bit mono PCM (see sampleRate) has
// accumulated via add.
func newInboundBuffer(duration time.Duration, onFull func(pcm []byte)) *inboundBuffer {
	targetBytes := int(sampleRate) * 2 * int(duration/time.Millisecond) / 1000
	return &inboundBuffer{targetBytes: targetBytes, onFull: onFull}
}

// add appends pcm to the buffer, calling onFull (and resetting) if the
// buffer has now reached targetBytes.
func (b *inboundBuffer) add(pcm []byte) {
	b.buffered = append(b.buffered, pcm...)
	if len(b.buffered) >= b.targetBytes {
		b.flush()
	}
}

// flush calls onFull with whatever has accumulated so far (a no-op if the
// buffer is currently empty) and resets.
func (b *inboundBuffer) flush() {
	if len(b.buffered) == 0 {
		return
	}
	pcm := b.buffered
	b.buffered = nil
	b.onFull(pcm)
}
