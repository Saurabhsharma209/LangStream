// Jitter buffer groundwork (ROADMAP.md Week 3: "Jitter buffer tuning
// against real PSTN conditions"). This file is explicitly *groundwork*,
// not the full roadmap item: real PSTN tuning needs measurements against
// a live/real transport (or at minimum real captured PSTN traces) that
// LangStream does not have yet (see pkg/rtp/doc.go and DEVLOG.md's
// 2026-07-08 entry: duplex RTP itself is still blocked on a ClearStream
// decision). What this file provides instead is the generic, algorithmic
// piece that any future transport wiring (ClearStream-based or otherwise)
// will need: a jitter buffer that operates purely on sequence-numbered,
// timestamped packets, with no socket, goroutine, or ClearStream
// dependency, so it is fully and deterministically unit-testable today and
// simply gets fed real arrival times once a real transport exists.
//
// Algorithm: a fixed-delay (non-adaptive) jitter buffer, the same "keep it
// simple for the pilot" choice this package's sibling pkg/langstream/vad.go
// already made (static RMS threshold instead of a learned VAD model). The
// first packet pushed establishes a base sequence number and base arrival
// time; every subsequent sequence number's playout deadline is
// base arrival + TargetDelay + PacketInterval*(seq-baseSeq), computed once
// and never revised. A packet arriving before its deadline is buffered
// (absorbing jitter and out-of-order arrival); a sequence number whose
// deadline passes with no packet on hand is declared lost and skipped.
//
// Repurposed (2026-07-13, EM decision) from inbound network-jitter
// absorption to outbound TTS pacing: this file's original motivating use
// case was smoothing inbound RTP arrival jitter ahead of ASR (see the
// ROADMAP.md reference above), built before DuplexSession (duplex.go)
// existed. Once DuplexSession landed, it turned out ClearStream's own
// rtp.Session already jitter-buffers each leg's inbound audio internally
// (see LegConfig.JitterDepth) before ever handing LangStream clean PCM via
// CleanAudio() -- making JitterBuffer's original inbound role redundant.
// Rather than delete working, thoroughly-tested code, JitterBuffer is now
// used by duplex.go's feedTTSPacer/runTTSPacer as an outbound pacing
// buffer on the TTS-synthesis -> InjectBotAudio path instead: TTS
// synthesis latency is bursty (variable per-chunk generation time), and
// this buffer's existing fixed-delay-then-release algorithm is exactly the
// right shape to smooth that burstiness before InjectBotAudio, just
// applied at a different point in the pipeline than originally designed
// for. The algorithm/API below is unchanged; only the caller and the
// Config values it is constructed with differ (see duplex.go's
// DefaultTTSPacingTargetDelay/DefaultTTSPacingInterval). One consequence
// worth calling out explicitly: PullLost's meaning shifts in this new
// context from "a network packet genuinely never arrived" to "TTS
// synthesis for this chunk stalled longer than the pacing buffer's
// TargetDelay budget, so it was dropped to keep output latency bounded"
// -- see PullLost's doc comment and duplex.go's runTTSPacer.
package rtp

import (
	"sync"
	"time"
)

// DefaultTargetDelay is the default fixed playout delay a JitterBuffer
// adds on top of a packet's arrival time before that packet becomes
// eligible for playout. It's the buffer's tunable absorption capacity for
// network jitter: bigger values tolerate more inter-packet delay variance
// at the cost of added end-to-end latency. 60ms is a common starting
// point for VoIP/PSTN jitter buffers; ROADMAP.md's "tuning against real
// PSTN conditions" is exactly the exercise of adjusting this value (and
// MaxPacketsBuffered) against real measured jitter once real traffic
// exists.
const DefaultTargetDelay = 60 * time.Millisecond

// DefaultPacketInterval is the default nominal spacing between
// consecutive sequence numbers' presentation times, matching a typical
// 20ms telephony RTP packetization interval (see e.g.
// pkg/tts.mockFrameBytes's doc comment elsewhere in this repo, which uses
// the same 20ms assumption).
const DefaultPacketInterval = 20 * time.Millisecond

// DefaultMaxPacketsBuffered caps how many not-yet-played packets a
// JitterBuffer holds at once, bounding memory if a burst of far-future
// (very out-of-order) sequence numbers arrives.
const DefaultMaxPacketsBuffered = 200

// Packet is one sequence-numbered, timestamped media packet. It is
// intentionally transport-agnostic: it has no RTP-header, UDP, or
// ClearStream dependency, so any real transport can adapt its own packet
// representation into this shape at the boundary.
type Packet struct {
	// SeqNum is the packet's sequence number. It is expected to
	// increment by 1 per packet and to wrap at 65535->0 (RFC 3550
	// sequence numbering), which JitterBuffer accounts for using signed
	// 16-bit circular distance (see seqDelta).
	SeqNum uint16
	// Timestamp is the packet's media-clock timestamp (RTP-style,
	// typically sample count at the media clock rate). JitterBuffer does
	// not currently use it for scheduling (SeqNum + Config.PacketInterval
	// drive playout timing instead, since not every caller's Timestamp
	// units/clock rate are known generically) — it is carried through
	// untouched for the caller's own use (e.g. logging, or a future
	// timestamp-based scheduling mode).
	Timestamp uint32
	// Payload is the packet's raw media payload (e.g. PCM, or an encoded
	// codec frame) - opaque to JitterBuffer.
	Payload []byte
}

// Config configures a JitterBuffer's playout policy. The zero value is
// not directly usable (a zero TargetDelay/PacketInterval would make every
// packet late); use NewJitterBuffer, which fills in
// DefaultTargetDelay/DefaultPacketInterval/DefaultMaxPacketsBuffered for
// any field left at its zero value.
type Config struct {
	// TargetDelay is the fixed playout delay added on top of a packet's
	// arrival time (see the package doc comment's algorithm description).
	TargetDelay time.Duration
	// PacketInterval is the nominal spacing between consecutive sequence
	// numbers' presentation times.
	PacketInterval time.Duration
	// MaxPacketsBuffered caps how many not-yet-played packets are held
	// at once. When a Push would exceed this, the currently-buffered
	// packet furthest in the future (least imminent, i.e. safest to
	// drop) is evicted to make room, unless the incoming packet itself
	// is the furthest, in which case the incoming packet is dropped
	// instead. See PushResult.
	MaxPacketsBuffered int
}

// withDefaults returns a copy of cfg with every zero-value field filled in.
func (cfg Config) withDefaults() Config {
	if cfg.TargetDelay <= 0 {
		cfg.TargetDelay = DefaultTargetDelay
	}
	if cfg.PacketInterval <= 0 {
		cfg.PacketInterval = DefaultPacketInterval
	}
	if cfg.MaxPacketsBuffered <= 0 {
		cfg.MaxPacketsBuffered = DefaultMaxPacketsBuffered
	}
	return cfg
}

// PushStatus classifies the outcome of a JitterBuffer.Push call.
type PushStatus int

const (
	// PushAccepted means the packet was newly buffered and is (or will
	// become) eligible for playout.
	PushAccepted PushStatus = iota
	// PushDuplicate means a packet with this SeqNum is already buffered
	// (not yet played); the incoming packet was ignored.
	PushDuplicate
	// PushLate means this SeqNum's playout slot has already passed
	// (already played, or already declared PullLost) by the time it
	// arrived; the incoming packet was dropped.
	PushLate
	// PushEvictedCapacity means the buffer was at Config.MaxPacketsBuffered
	// and this packet was the furthest in the future of all
	// buffered-or-incoming packets, so it was dropped to stay within
	// capacity instead of evicting a more-imminent one.
	PushEvictedCapacity
)

// String implements fmt.Stringer for readable test failures/logs.
func (s PushStatus) String() string {
	switch s {
	case PushAccepted:
		return "PushAccepted"
	case PushDuplicate:
		return "PushDuplicate"
	case PushLate:
		return "PushLate"
	case PushEvictedCapacity:
		return "PushEvictedCapacity"
	default:
		return "PushStatus(?)"
	}
}

// PullStatus classifies the outcome of a JitterBuffer.Pull call.
type PullStatus int

const (
	// PullEmpty means Pull was called before any packet was ever pushed
	// (the buffer isn't primed with a base sequence/arrival time yet).
	PullEmpty PullStatus = iota
	// PullOK means a Packet for the expected sequence number was
	// available and is returned.
	PullOK
	// PullWaiting means the expected sequence number's playout deadline
	// has not yet passed, but the packet hasn't arrived (yet) either;
	// the caller should call Pull again later (e.g. on its next playout
	// tick) rather than treat this as loss.
	PullWaiting
	// PullLost means the expected sequence number's playout deadline
	// passed with no packet ever arriving for it; JitterBuffer has
	// skipped past it (advanced its internal expected-sequence pointer)
	// so subsequent Pulls make progress. The caller decides concealment
	// (e.g. comfort noise, silence, or repeating the previous frame) --
	// that is a media-specific policy this transport-agnostic package
	// deliberately does not impose.
	//
	// In duplex.go's outbound-TTS-pacing use of JitterBuffer (see the
	// package doc comment's repurposing note), "the packet" is a
	// synthesized tts.AudioChunk, not an RTP packet, so PullLost there
	// means "TTS synthesis for this chunk took longer than the pacing
	// buffer's TargetDelay budget", and the caller's chosen concealment
	// policy (see runTTSPacer) is simply to drop it and move on, bounding
	// added latency rather than letting synthesized speech fall
	// arbitrarily far behind real-time.
	PullLost
)

// String implements fmt.Stringer for readable test failures/logs.
func (s PullStatus) String() string {
	switch s {
	case PullEmpty:
		return "PullEmpty"
	case PullOK:
		return "PullOK"
	case PullWaiting:
		return "PullWaiting"
	case PullLost:
		return "PullLost"
	default:
		return "PullStatus(?)"
	}
}

// Stats is a point-in-time snapshot of a JitterBuffer's counters, useful
// for tests and for a future observability/dashboard integration (owned by
// SRE/EM this sprint, not wired up here — see ROADMAP.md's Week 3
// "observability dashboard" item).
type Stats struct {
	Received           int64 // PushAccepted count
	Duplicates         int64 // PushDuplicate count
	Late               int64 // PushLate count
	EvictedForCapacity int64 // PushEvictedCapacity count
	Lost               int64 // PullLost count
	CurrentlyBuffered  int   // packets held right now, awaiting playout
}

// JitterBuffer absorbs variable inter-packet arrival delay ("jitter"),
// reorders out-of-order packets back into sequence order, and declares
// loss for sequence numbers that never arrive within the buffering
// window, per the package doc comment's fixed-delay algorithm. It is safe
// for concurrent use by one writer (Push, e.g. a packet-receive
// goroutine) and one reader (Pull, e.g. a playout-clock goroutine).
//
// JitterBuffer has no notion of wall-clock time itself: every method that
// needs "now" takes it as an explicit parameter, so it can be driven by a
// real clock in production or by a synthetic/simulated clock in tests
// (see jitter_test.go), with fully deterministic results either way.
type JitterBuffer struct {
	cfg Config

	mu sync.Mutex

	initialized bool
	baseSeq     uint16
	baseArrival time.Time
	nextSeq     uint16

	buffered map[uint16]Packet

	stats Stats
}

// NewJitterBuffer returns a ready-to-use JitterBuffer. Any zero-value
// field of cfg is filled in with its documented default (see
// Config.withDefaults).
func NewJitterBuffer(cfg Config) *JitterBuffer {
	return &JitterBuffer{
		cfg:      cfg.withDefaults(),
		buffered: make(map[uint16]Packet),
	}
}

// seqDelta returns the signed circular distance a-b for RFC 3550-style
// wrapping 16-bit sequence numbers, i.e. the number of sequence-number
// steps from b to a (positive if a is "after" b, negative if "before"),
// correct across the 65535->0 wraparound as long as the true distance
// between a and b is less than 32768 (always true for any buffering
// window sane enough to fit in memory).
func seqDelta(a, b uint16) int32 {
	return int32(int16(a - b))
}

// deadline returns the wall-clock time by which seq must have arrived to
// still be eligible for playout, given the buffer's established base
// sequence/arrival time. Only valid once b.initialized is true.
func (b *JitterBuffer) deadlineLocked(seq uint16) time.Time {
	steps := seqDelta(seq, b.baseSeq)
	return b.baseArrival.Add(b.cfg.TargetDelay + time.Duration(steps)*b.cfg.PacketInterval)
}

// Push offers pkt to the buffer, having arrived at wall-clock time
// arrival. The first-ever Push establishes the buffer's base sequence
// number and base arrival time (see the package doc comment); every
// subsequent packet's playout deadline is fixed relative to that base and
// never revised, so arrival jitter on later packets doesn't shift earlier
// packets' deadlines.
func (b *JitterBuffer) Push(pkt Packet, arrival time.Time) PushStatus {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.initialized {
		b.initialized = true
		b.baseSeq = pkt.SeqNum
		b.baseArrival = arrival
		b.nextSeq = pkt.SeqNum
	}

	dist := seqDelta(pkt.SeqNum, b.nextSeq)
	if dist < 0 {
		// This sequence number's playout slot is already in the past
		// (already played, or already declared lost) - too late to be
		// useful.
		b.stats.Late++
		return PushLate
	}

	if _, exists := b.buffered[pkt.SeqNum]; exists {
		b.stats.Duplicates++
		return PushDuplicate
	}

	if len(b.buffered) >= b.cfg.MaxPacketsBuffered {
		// At capacity: evict whichever packet (buffered or incoming) is
		// furthest in the future, i.e. least imminent / safest to drop,
		// so the buffer stays useful for packets closer to their
		// deadline.
		furthestSeq, furthestDist := pkt.SeqNum, dist
		for seq := range b.buffered {
			if d := seqDelta(seq, b.nextSeq); d > furthestDist {
				furthestSeq, furthestDist = seq, d
			}
		}
		if furthestSeq == pkt.SeqNum {
			b.stats.EvictedForCapacity++
			return PushEvictedCapacity
		}
		delete(b.buffered, furthestSeq)
		b.stats.EvictedForCapacity++
	}

	b.buffered[pkt.SeqNum] = pkt
	b.stats.Received++
	return PushAccepted
}

// Pull attempts to produce the next packet in sequence order, given the
// current wall-clock playout time now. See PullStatus's docs for the
// three possible non-OK outcomes. A typical playout loop calls Pull once
// per Config.PacketInterval tick, advancing now each time, and applies its
// own concealment policy whenever the status is PullLost.
func (b *JitterBuffer) Pull(now time.Time) (Packet, PullStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.initialized {
		return Packet{}, PullEmpty
	}

	if pkt, ok := b.buffered[b.nextSeq]; ok {
		delete(b.buffered, b.nextSeq)
		b.nextSeq++
		return pkt, PullOK
	}

	if now.Before(b.deadlineLocked(b.nextSeq)) {
		return Packet{}, PullWaiting
	}

	// Deadline passed with nothing on hand: declare this sequence number
	// lost and move on, so the buffer always makes forward progress
	// instead of waiting forever for a packet that will never arrive.
	b.nextSeq++
	b.stats.Lost++
	return Packet{}, PullLost
}

// Stats returns a point-in-time snapshot of the buffer's counters.
func (b *JitterBuffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	snap := b.stats
	snap.CurrentlyBuffered = len(b.buffered)
	return snap
}
