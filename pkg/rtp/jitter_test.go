package rtp

import (
	"math/rand"
	"testing"
	"time"
)

// --- Basic mechanics ---

func TestJitterBufferPullBeforeAnyPushIsEmpty(t *testing.T) {
	b := NewJitterBuffer(Config{})
	_, status := b.Pull(time.Now())
	if status != PullEmpty {
		t.Fatalf("Pull before any Push = %v, want PullEmpty", status)
	}
}

func TestJitterBufferInOrderNoJitterNoLoss(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 40 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	const n = 20
	for i := 0; i < n; i++ {
		arrival := base.Add(time.Duration(i) * cfg.PacketInterval)
		status := b.Push(Packet{SeqNum: uint16(i), Payload: []byte{byte(i)}}, arrival)
		if status != PushAccepted {
			t.Fatalf("Push #%d = %v, want PushAccepted", i, status)
		}
	}

	// Playout clock ticks every PacketInterval, starting once
	// TargetDelay has elapsed since the first packet's arrival.
	now := base.Add(cfg.TargetDelay)
	for i := 0; i < n; i++ {
		pkt, status := b.Pull(now)
		if status != PullOK {
			t.Fatalf("Pull #%d = %v, want PullOK", i, status)
		}
		if pkt.SeqNum != uint16(i) {
			t.Fatalf("Pull #%d SeqNum = %d, want %d", i, pkt.SeqNum, i)
		}
		now = now.Add(cfg.PacketInterval)
	}

	stats := b.Stats()
	if stats.Received != n {
		t.Fatalf("Received = %d, want %d", stats.Received, n)
	}
	if stats.Lost != 0 || stats.Late != 0 || stats.Duplicates != 0 {
		t.Fatalf("expected zero loss/late/duplicates for a clean in-order stream, got %+v", stats)
	}
}

func TestJitterBufferReordersOutOfOrderPackets(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 100 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	// Arrive in the order 0, 2, 1, 3 - packet 1 and 2 are swapped - but
	// well within TargetDelay of each other, so all four should still be
	// played out in strict sequence order 0,1,2,3.
	order := []int{0, 2, 1, 3}
	for _, seq := range order {
		arrival := base.Add(time.Duration(seq) * 5 * time.Millisecond) // small, jittered arrival spacing
		status := b.Push(Packet{SeqNum: uint16(seq), Payload: []byte{byte(seq)}}, arrival)
		if status != PushAccepted {
			t.Fatalf("Push seq=%d = %v, want PushAccepted", seq, status)
		}
	}

	now := base.Add(cfg.TargetDelay + 50*time.Millisecond) // comfortably past every deadline
	for want := 0; want < 4; want++ {
		pkt, status := b.Pull(now)
		if status != PullOK {
			t.Fatalf("Pull (want seq=%d) = %v, want PullOK", want, status)
		}
		if pkt.SeqNum != uint16(want) {
			t.Fatalf("Pull returned seq=%d, want %d (reordering failed)", pkt.SeqNum, want)
		}
	}
}

func TestJitterBufferDuplicatePacketIgnored(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 40 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	b.Push(Packet{SeqNum: 0, Payload: []byte{0}}, base)
	status := b.Push(Packet{SeqNum: 0, Payload: []byte{0}}, base.Add(time.Millisecond))
	if status != PushDuplicate {
		t.Fatalf("second Push of the same not-yet-played SeqNum = %v, want PushDuplicate", status)
	}
	if got := b.Stats().Duplicates; got != 1 {
		t.Fatalf("Duplicates = %d, want 1", got)
	}
}

func TestJitterBufferLatePacketAfterPlayoutIsDropped(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 20 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	b.Push(Packet{SeqNum: 0, Payload: []byte{0}}, base)
	if _, status := b.Pull(base.Add(cfg.TargetDelay)); status != PullOK {
		t.Fatalf("expected seq 0 to play out, got status %v", status)
	}

	// seq 0 has already been played; a (very) late arrival for it now
	// must be rejected as PushLate, not silently accepted or treated as
	// a duplicate.
	status := b.Push(Packet{SeqNum: 0, Payload: []byte{0}}, base.Add(500*time.Millisecond))
	if status != PushLate {
		t.Fatalf("Push of an already-played SeqNum = %v, want PushLate", status)
	}
	if got := b.Stats().Late; got != 1 {
		t.Fatalf("Late = %d, want 1", got)
	}
}

func TestJitterBufferMissingPacketDeclaredLostThenProgresses(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 40 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	// seq 0 arrives; seq 1 never arrives; seq 2 arrives on schedule.
	b.Push(Packet{SeqNum: 0, Payload: []byte{0}}, base)
	b.Push(Packet{SeqNum: 2, Payload: []byte{2}}, base.Add(2*cfg.PacketInterval))

	now := base.Add(cfg.TargetDelay)
	pkt, status := b.Pull(now)
	if status != PullOK || pkt.SeqNum != 0 {
		t.Fatalf("Pull #1 = (pkt=%v, status=%v), want seq=0 PullOK", pkt, status)
	}

	// Not yet past seq 1's deadline: must wait, not declare loss early.
	now = now.Add(1 * time.Millisecond)
	if _, status := b.Pull(now); status != PullWaiting {
		t.Fatalf("Pull before seq 1's deadline = %v, want PullWaiting", status)
	}

	// Advance past seq 1's deadline (base + TargetDelay + 1*PacketInterval).
	now = base.Add(cfg.TargetDelay + cfg.PacketInterval + time.Millisecond)
	_, status = b.Pull(now)
	if status != PullLost {
		t.Fatalf("Pull past seq 1's deadline = %v, want PullLost", status)
	}

	pkt, status = b.Pull(now)
	if status != PullOK || pkt.SeqNum != 2 {
		t.Fatalf("Pull after loss = (pkt=%v, status=%v), want seq=2 PullOK", pkt, status)
	}

	stats := b.Stats()
	if stats.Lost != 1 {
		t.Fatalf("Lost = %d, want 1", stats.Lost)
	}
	if stats.Received != 2 {
		t.Fatalf("Received = %d, want 2", stats.Received)
	}
}

func TestJitterBufferSequenceNumberWraparound(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 40 * time.Millisecond, PacketInterval: 20 * time.Millisecond}
	b := NewJitterBuffer(cfg)

	seqs := []uint16{65534, 65535, 0, 1}
	for i, seq := range seqs {
		arrival := base.Add(time.Duration(i) * cfg.PacketInterval)
		if status := b.Push(Packet{SeqNum: seq, Payload: []byte{byte(i)}}, arrival); status != PushAccepted {
			t.Fatalf("Push seq=%d = %v, want PushAccepted", seq, status)
		}
	}

	now := base.Add(cfg.TargetDelay + 3*cfg.PacketInterval)
	for i, want := range seqs {
		pkt, status := b.Pull(now)
		if status != PullOK {
			t.Fatalf("Pull #%d = %v, want PullOK", i, status)
		}
		if pkt.SeqNum != want {
			t.Fatalf("Pull #%d SeqNum = %d, want %d (wraparound ordering broken)", i, pkt.SeqNum, want)
		}
	}
}

func TestJitterBufferCapacityEvictsFurthestPacket(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := Config{TargetDelay: 500 * time.Millisecond, PacketInterval: 20 * time.Millisecond, MaxPacketsBuffered: 2}
	b := NewJitterBuffer(cfg)

	// nextSeq is 0 (established by this first Push). Never pull it, so
	// it stays "buffered" the whole test... actually seq 0 itself is
	// nextSeq and gets buffered too (dist 0), counting against capacity.
	if status := b.Push(Packet{SeqNum: 0}, base); status != PushAccepted {
		t.Fatalf("Push seq=0 = %v, want PushAccepted", status)
	}
	if status := b.Push(Packet{SeqNum: 5}, base); status != PushAccepted {
		t.Fatalf("Push seq=5 = %v, want PushAccepted", status)
	}
	// Buffer is now at capacity (2) with seq 0 (dist 0) and seq 5 (dist 5)
	// held. seq 3 (dist 3) is nearer than seq 5, so seq 5 should be
	// evicted to make room for seq 3.
	status := b.Push(Packet{SeqNum: 3}, base)
	if status != PushAccepted {
		t.Fatalf("Push seq=3 = %v, want PushAccepted", status)
	}

	stats := b.Stats()
	if stats.EvictedForCapacity != 1 {
		t.Fatalf("EvictedForCapacity = %d, want 1", stats.EvictedForCapacity)
	}
	if stats.CurrentlyBuffered != 2 {
		t.Fatalf("CurrentlyBuffered = %d, want 2", stats.CurrentlyBuffered)
	}

	// A packet further out than everything currently buffered (dist 9 >
	// dist 3, dist 0) should itself be the one dropped, not evict
	// anything nearer.
	status = b.Push(Packet{SeqNum: 9}, base)
	if status != PushEvictedCapacity {
		t.Fatalf("Push seq=9 (furthest) = %v, want PushEvictedCapacity", status)
	}
}

func TestConfigDefaultsApplied(t *testing.T) {
	b := NewJitterBuffer(Config{})
	if b.cfg.TargetDelay != DefaultTargetDelay {
		t.Errorf("TargetDelay = %v, want default %v", b.cfg.TargetDelay, DefaultTargetDelay)
	}
	if b.cfg.PacketInterval != DefaultPacketInterval {
		t.Errorf("PacketInterval = %v, want default %v", b.cfg.PacketInterval, DefaultPacketInterval)
	}
	if b.cfg.MaxPacketsBuffered != DefaultMaxPacketsBuffered {
		t.Errorf("MaxPacketsBuffered = %d, want default %d", b.cfg.MaxPacketsBuffered, DefaultMaxPacketsBuffered)
	}
}

// --- seqDelta correctness (underpins reordering + wraparound) ---

func TestSeqDelta(t *testing.T) {
	cases := []struct {
		a, b uint16
		want int32
	}{
		{0, 0, 0},
		{5, 3, 2},
		{3, 5, -2},
		{0, 65535, 1},   // wraparound forward
		{65535, 0, -1},  // wraparound backward
		{10, 65530, 16}, // wraps across the boundary
	}
	for _, tc := range cases {
		if got := seqDelta(tc.a, tc.b); got != tc.want {
			t.Errorf("seqDelta(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- PSTN-condition simulation: reordering, jitter, and loss together,
// with deterministic seeded randomness so the test never flakes. ---

// simulatedPacket is one packet in a synthetic PSTN-like stream: sent at a
// nominal time (seq * PacketInterval after the stream start) but observed
// arriving at arrival (nominal + random jitter), or dropped entirely
// (present=false) to simulate packet loss.
type simulatedPacket struct {
	seq     uint16
	arrival time.Time
	present bool
}

// simulatePSTNStream generates a deterministic (seed-driven) synthetic
// packet stream of n packets at the given nominal interval, with:
//   - jitter: each packet's arrival is nominal +/- a random offset drawn
//     from a normal distribution (stddev jitterStdDev), clamped to
//     non-negative, so packets never "arrive" before the previous one was
//     sent relative to stream start (a real network can still reorder
//     packets this way without any packet appearing to travel backward in
//     time overall).
//   - loss: each packet is independently dropped with probability lossP.
//   - reordering falls out naturally from jitter: two adjacent packets'
//     arrival order can flip if their jittered arrivals cross.
//
// The returned slice is sorted by arrival time (i.e. in the order a
// receiver would actually observe them), matching how Push would really
// be called as packets come off the wire.
func simulatePSTNStream(rng *rand.Rand, n int, start time.Time, nominalInterval time.Duration, jitterStdDev time.Duration, lossP float64) []simulatedPacket {
	pkts := make([]simulatedPacket, 0, n)
	for i := 0; i < n; i++ {
		if rng.Float64() < lossP {
			pkts = append(pkts, simulatedPacket{seq: uint16(i), present: false})
			continue
		}
		nominal := start.Add(time.Duration(i) * nominalInterval)
		jitter := time.Duration(rng.NormFloat64() * float64(jitterStdDev))
		arrival := nominal.Add(jitter)
		if arrival.Before(start) {
			arrival = start
		}
		pkts = append(pkts, simulatedPacket{seq: uint16(i), arrival: arrival, present: true})
	}

	// Sort present packets by arrival time (stable, so ties keep sequence
	// order) to model receive order.
	present := pkts[:0:0]
	for _, p := range pkts {
		if p.present {
			present = append(present, p)
		}
	}
	for i := 1; i < len(present); i++ {
		for j := i; j > 0 && present[j].arrival.Before(present[j-1].arrival); j-- {
			present[j], present[j-1] = present[j-1], present[j]
		}
	}
	return present
}

func TestJitterBufferUnderSimulatedPSTNConditions(t *testing.T) {
	const (
		n               = 500
		nominalInterval = 20 * time.Millisecond
		jitterStdDev    = 15 * time.Millisecond
		lossP           = 0.03 // 3% loss, a rough "bad PSTN leg" ballpark
	)
	start := time.Unix(1_700_000_000, 0)

	rng := rand.New(rand.NewSource(42)) // fixed seed: deterministic, no flakes
	stream := simulatePSTNStream(rng, n, start, nominalInterval, jitterStdDev, lossP)

	cfg := Config{
		TargetDelay:        100 * time.Millisecond, // generous relative to jitterStdDev
		PacketInterval:     nominalInterval,
		MaxPacketsBuffered: 64,
	}
	b := NewJitterBuffer(cfg)

	// Drive Push and Pull interleaved on the same simulated timeline, the
	// way a real receive goroutine (Push, as packets actually arrive) and
	// playout goroutine (Pull, once per PacketInterval tick) would:
	// pushing everything up front before ever pulling would make the
	// buffer look artificially overwhelmed (every out-of-order/future
	// packet piles up with nothing ever drained), which isn't how a live
	// jitter buffer is ever actually driven.
	//
	// totalTicks runs comfortably past the last packet's nominal send
	// time plus TargetDelay plus several jitterStdDev of slack, so every
	// sequence number resolves (played or lost) well before the loop ends.
	totalTicks := n + int(cfg.TargetDelay/nominalInterval) + int(5*jitterStdDev/nominalInterval) + 10

	var playedInOrder []uint16
	var lostCount int
	streamIdx := 0
	for tick := 0; tick < totalTicks; tick++ {
		now := start.Add(time.Duration(tick) * nominalInterval)

		for streamIdx < len(stream) && !stream[streamIdx].arrival.After(now) {
			b.Push(Packet{SeqNum: stream[streamIdx].seq}, stream[streamIdx].arrival)
			streamIdx++
		}

		pkt, status := b.Pull(now)
		switch status {
		case PullOK:
			playedInOrder = append(playedInOrder, pkt.SeqNum)
		case PullLost:
			lostCount++
		case PullWaiting, PullEmpty:
			// Nothing to do this tick; try again next tick.
		}
	}
	if streamIdx != len(stream) {
		t.Fatalf("test bug: only pushed %d of %d generated arrivals before totalTicks ran out - widen totalTicks", streamIdx, len(stream))
	}

	// Every played sequence number must come out in strictly increasing
	// order (reordering + jitter absorption must never invert playout
	// order), and every one must be a real, distinct sequence number
	// from the original stream.
	seen := make(map[uint16]bool)
	for i, seq := range playedInOrder {
		if seen[seq] {
			t.Fatalf("sequence %d played more than once", seq)
		}
		seen[seq] = true
		if i > 0 && int32(seq)-int32(playedInOrder[i-1]) <= 0 {
			t.Fatalf("playout order not increasing: %d then %d", playedInOrder[i-1], seq)
		}
	}

	stats := b.Stats()
	t.Logf("PSTN simulation: n=%d received=%d lost=%d late=%d duplicates=%d evicted=%d played=%d",
		n, stats.Received, stats.Lost, stats.Late, stats.Duplicates, stats.EvictedForCapacity, len(playedInOrder))

	// Sanity bounds: with lossP=3% we expect on the order of n*lossP
	// packets to genuinely never arrive; JitterBuffer's own Lost count
	// (post-buffering) should be in a plausible range around that,
	// accounting for TargetDelay being generous enough that jitter alone
	// rarely causes extra loss (it should mostly be the simulator's
	// actual drops that surface as PullLost).
	if stats.Lost == 0 {
		t.Error("expected at least some PullLost given 3% simulated packet loss")
	}
	if float64(stats.Lost) > float64(n)*0.15 {
		t.Errorf("Lost = %d is implausibly high for %d packets at %.0f%% simulated loss with a %v target delay",
			stats.Lost, n, lossP*100, cfg.TargetDelay)
	}
	if len(playedInOrder)+lostCount == 0 {
		t.Fatal("nothing played and nothing lost - buffer made no progress")
	}
	if got := len(playedInOrder); got < n-int(float64(n)*0.15) {
		t.Errorf("played %d of %d packets, expected most of them to survive jitter+reordering (only real drops should be lost)", got, n)
	}
}

func TestSimulatePSTNStreamDeterministicForFixedSeed(t *testing.T) {
	start := time.Unix(0, 0)
	a := simulatePSTNStream(rand.New(rand.NewSource(7)), 100, start, 20*time.Millisecond, 10*time.Millisecond, 0.05)
	b := simulatePSTNStream(rand.New(rand.NewSource(7)), 100, start, 20*time.Millisecond, 10*time.Millisecond, 0.05)

	if len(a) != len(b) {
		t.Fatalf("len(a)=%d len(b)=%d, want equal for the same seed", len(a), len(b))
	}
	for i := range a {
		if a[i].seq != b[i].seq || !a[i].arrival.Equal(b[i].arrival) {
			t.Fatalf("stream %d differs between runs with the same seed: %+v vs %+v", i, a[i], b[i])
		}
	}
}
