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

// --- Harsher stress-test scenarios (2026-07-12 Tech-workstream sprint,
// Task 2): pkg/rtp/jitter.go is algorithmic groundwork only (see its
// package doc comment) -- real PSTN tuning is blocked on a live/real
// transport that doesn't exist yet. These tests don't attempt that; they
// widen the simulated-condition coverage beyond
// TestJitterBufferUnderSimulatedPSTNConditions's single ~3%-loss scenario,
// so the buffer's core invariants (no panics, bounded memory via
// MaxPacketsBuffered, monotonic in-order playout) are demonstrated under
// harsher/different synthetic conditions too, ahead of eventual real-
// traffic tuning. ---

// driveJitterBufferSimulation pushes stream through b and pulls once per
// nominalInterval tick, mirroring
// TestJitterBufferUnderSimulatedPSTNConditions's interleaved push/pull
// pattern (a real receive goroutine and a real playout goroutine driving
// the same buffer concurrently in production, rather than pushing
// everything up front). It fails the test outright if the buffer's
// CurrentlyBuffered count ever exceeds Config.MaxPacketsBuffered (the
// memory-bound invariant) or if not every generated arrival was pushed
// before ticks ran out (a test-setup bug, not a buffer bug). It returns
// the sequence of successfully played packets in play order and the total
// PullLost count.
func driveJitterBufferSimulation(t *testing.T, b *JitterBuffer, stream []simulatedPacket, start time.Time, nominalInterval time.Duration, totalTicks int, n int) (played []uint16, lostCount int) {
	t.Helper()
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
			played = append(played, pkt.SeqNum)
		case PullLost:
			lostCount++
		case PullWaiting, PullEmpty:
			// Nothing to do this tick; try again next tick.
		}

		if cur := b.Stats().CurrentlyBuffered; cur > b.cfg.MaxPacketsBuffered {
			t.Fatalf("tick %d: CurrentlyBuffered = %d, exceeds MaxPacketsBuffered = %d -- unbounded memory growth under stress", tick, cur, b.cfg.MaxPacketsBuffered)
		}

		// Packet-accounting invariant (2026-07-13 QA-workstream follow-up
		// to the 2026-07-12 Tech-workstream sprint that added these
		// harsher stress scenarios): JitterBuffer.Pull always resolves
		// exactly one sequence number per successful call, advancing its
		// internal "next expected" pointer by exactly 1 via either
		// PullOK (played) or PullLost (lost) -- see jitter.go's Pull. So
		// len(played)+lostCount must equal exactly the number of
		// sequence positions resolved so far, and once it reaches n (the
		// simulated stream's full 0..n-1 range) every one of the n
		// intended packets has been accounted for as played XOR lost,
		// with none silently dropped without being counted.
		//
		// We stop driving the simulation the instant that happens
		// instead of continuing to burn through totalTicks's slack,
		// because JitterBuffer has no notion of "end of stream" (by
		// design -- it's transport-agnostic and doesn't know how many
		// packets a caller intends to send). If we kept calling Pull
		// past this point, every further tick would manufacture a
		// *phantom* PullLost for a sequence number that was never part
		// of the simulated stream at all (nextSeq just keeps climbing
		// past n forever once its deadline math says "expired"),
		// inflating lostCount by however many extra ticks of headroom
		// totalTicks happens to contain -- a number with no relation to
		// real packet loss. (This was verified empirically: before this
		// fix, e.g. the bursty-reordering scenario -- which the
		// simulator never drops a single packet in -- still reported
		// stats.Lost=10, exactly matching that test's "+10" totalTicks
		// padding, i.e. 10 purely phantom losses.) Stopping here keeps
		// played/lostCount an honest, exact account of precisely the n
		// real packets under test: no tolerance window is needed
		// because the buffer's one-resolution-per-Pull design makes the
		// invariant exact, not approximate, as long as the simulation
		// itself stops exactly at n instead of overrunning it.
		if len(played)+lostCount >= n {
			break
		}
	}
	if streamIdx != len(stream) {
		t.Fatalf("test bug: only pushed %d of %d generated arrivals before the stream fully resolved - widen TargetDelay relative to jitter so arrivals land within the buffering window", streamIdx, len(stream))
	}
	if got := len(played) + lostCount; got != n {
		t.Fatalf("packet-accounting invariant violated: played(%d)+lost(%d) = %d, want exactly n = %d -- either a packet was silently dropped without being counted as lost, or totalTicks ran out before the stream fully resolved (widen totalTicks)", len(played), lostCount, got, n)
	}
	return played, lostCount
}

// assertPacketAccounting asserts the core packet-accounting invariant
// this package's jitter buffer must uphold under any of the harsher
// simulated scenarios below: every one of the n sequence numbers in the
// simulated stream must end up counted as exactly one of played or lost,
// with none silently disappearing uncounted (e.g. a regression that
// dropped a packet on the floor without ever incrementing stats.Lost).
//
// This is asserted as an *exact* equality, not a fuzzy tolerance window.
// That's possible (and correct) here because JitterBuffer.Pull always
// resolves exactly one sequence number per successful call by advancing
// its internal "next expected" pointer by exactly 1, via either PullOK
// (played) or PullLost (lost) -- see jitter.go's Pull doc comment. So
// played+lost is, by construction, always exactly the count of sequence
// positions resolved so far; it can only fail to equal n if either (a) a
// packet was dropped without being accounted for (the real regression
// this guards against), or (b) the simulation kept driving Pull past the
// stream's real end, which manufactures *phantom* losses for sequence
// numbers that were never actually sent (see
// driveJitterBufferSimulation's comment for why, and why it stops itself
// exactly at n to avoid that). A tolerance window would risk masking
// exactly the silent-drop regression this check exists to catch, so we
// don't use one.
func assertPacketAccounting(t *testing.T, played []uint16, lostCount int, statsLost int64, n int) {
	t.Helper()
	if got := len(played) + lostCount; got != n {
		t.Errorf("packet-accounting invariant violated: played(%d)+lost(%d) = %d, want exactly n=%d -- a packet may have been silently dropped without being counted as lost", len(played), lostCount, got, n)
	}
	if int64(lostCount) != statsLost {
		t.Errorf("lostCount observed by the test (%d) != JitterBuffer's own Stats().Lost (%d) -- the buffer's own counter disagrees with what Pull actually returned", lostCount, statsLost)
	}
}

// assertMonotonicNoDuplicates fails the test if played is not a strictly
// increasing sequence of distinct sequence numbers -- the core
// correctness invariant a jitter buffer must uphold regardless of how
// harsh the simulated network conditions are: playout order must never
// regress or double-play a packet.
func assertMonotonicNoDuplicates(t *testing.T, played []uint16) {
	t.Helper()
	seen := make(map[uint16]bool)
	for i, seq := range played {
		if seen[seq] {
			t.Fatalf("sequence %d played more than once", seq)
		}
		seen[seq] = true
		if i > 0 && int32(seq)-int32(played[i-1]) <= 0 {
			t.Fatalf("playout order not increasing: %d then %d", played[i-1], seq)
		}
	}
}

// TestJitterBufferUnderHarshPacketLoss widens
// TestJitterBufferUnderSimulatedPSTNConditions's ~3% loss to a much
// harsher ~13% -- a genuinely bad PSTN/last-mile leg -- and asserts the
// buffer still makes monotonic, panic-free progress and stays within its
// memory bound.
func TestJitterBufferUnderHarshPacketLoss(t *testing.T) {
	const (
		n               = 500
		nominalInterval = 20 * time.Millisecond
		jitterStdDev    = 15 * time.Millisecond
		lossP           = 0.13 // ~13% loss, well above the existing 3% baseline test
	)
	start := time.Unix(1_700_000_100, 0)

	rng := rand.New(rand.NewSource(101))
	stream := simulatePSTNStream(rng, n, start, nominalInterval, jitterStdDev, lossP)

	cfg := Config{
		TargetDelay:        100 * time.Millisecond,
		PacketInterval:     nominalInterval,
		MaxPacketsBuffered: 64,
	}
	b := NewJitterBuffer(cfg)

	totalTicks := n + int(cfg.TargetDelay/nominalInterval) + int(5*jitterStdDev/nominalInterval) + 10
	played, lostCount := driveJitterBufferSimulation(t, b, stream, start, nominalInterval, totalTicks, n)

	assertMonotonicNoDuplicates(t, played)

	stats := b.Stats()
	t.Logf("harsh-loss simulation: n=%d received=%d lost=%d late=%d duplicates=%d evicted=%d played=%d",
		n, stats.Received, stats.Lost, stats.Late, stats.Duplicates, stats.EvictedForCapacity, len(played))

	assertPacketAccounting(t, played, lostCount, stats.Lost, n)

	if stats.Lost == 0 {
		t.Error("expected substantial PullLost given ~13% simulated packet loss")
	}
	if float64(stats.Lost) < float64(n)*0.08 || float64(stats.Lost) > float64(n)*0.30 {
		t.Errorf("Lost = %d is implausible for %d packets at %.0f%% simulated loss with a %v target delay",
			stats.Lost, n, lossP*100, cfg.TargetDelay)
	}
	if len(played)+lostCount == 0 {
		t.Fatal("nothing played and nothing lost - buffer made no progress")
	}
}

// simulateBurstyReorderStream generates a deterministic stream of n
// packets, nominally spaced nominalInterval apart, but shuffles sequence
// numbers within successive non-overlapping windows of windowSize packets
// before assigning arrival times -- so within a burst, a packet can arrive
// several sequence positions out of order (e.g. seq 7 arriving before seq
// 0 within an 8-packet window), not just swapped with its immediate
// neighbor like TestJitterBufferReordersOutOfOrderPackets. The globally
// lowest sequence number (0) is guaranteed to be the first arrival, since
// JitterBuffer establishes its base sequence/arrival time from whichever
// packet is pushed first (see jitter.go's package doc comment) -- a real
// stream's first-sent packet arriving before the stream "began" isn't a
// scenario this buffer is designed to handle, so the test doesn't
// manufacture that impossible case.
func simulateBurstyReorderStream(rng *rand.Rand, n int, start time.Time, nominalInterval time.Duration, windowSize int) []simulatedPacket {
	pkts := make([]simulatedPacket, n)
	for i := 0; i < n; i++ {
		pkts[i] = simulatedPacket{seq: uint16(i), present: true}
	}

	for ws := 0; ws < n; ws += windowSize {
		we := ws + windowSize
		if we > n {
			we = n
		}
		window := pkts[ws:we]
		rng.Shuffle(len(window), func(i, j int) { window[i], window[j] = window[j], window[i] })
	}

	minIdx := 0
	for i, p := range pkts {
		if p.seq < pkts[minIdx].seq {
			minIdx = i
		}
	}
	pkts[0], pkts[minIdx] = pkts[minIdx], pkts[0]

	// Arrival times themselves stay regularly paced at nominalInterval
	// (matching real per-packet network throughput and the playout
	// clock's own pace) -- only the sequence-number *labels* attached to
	// each arrival slot are shuffled above. This models "packets arrived
	// out of order" without also compressing/inflating the stream's
	// overall arrival rate, which would conflate reordering with a
	// separate capacity-under-burst scenario.
	for i := range pkts {
		pkts[i].arrival = start.Add(time.Duration(i) * nominalInterval)
	}
	return pkts
}

// TestJitterBufferBurstyMultiPositionReordering exercises reordering
// bursts harsher than TestJitterBufferReordersOutOfOrderPackets's single
// adjacent swap: packets arrive shuffled several sequence positions out of
// order within each burst window. Given a TargetDelay generous enough to
// absorb a full window's worth of reordering, the buffer should still
// deliver nearly everything, strictly in order, with no duplicates and no
// unbounded memory growth.
func TestJitterBufferBurstyMultiPositionReordering(t *testing.T) {
	const (
		n               = 300
		nominalInterval = 20 * time.Millisecond
		windowSize      = 8 // packets can arrive up to ~7 positions out of order within a burst
	)
	start := time.Unix(1_700_000_200, 0)

	rng := rand.New(rand.NewSource(202))
	stream := simulateBurstyReorderStream(rng, n, start, nominalInterval, windowSize)

	cfg := Config{
		TargetDelay:        time.Duration(windowSize+2) * nominalInterval, // generous enough to absorb a full window's worth of reordering
		PacketInterval:     nominalInterval,
		MaxPacketsBuffered: 64,
	}
	b := NewJitterBuffer(cfg)

	totalTicks := n + int(cfg.TargetDelay/nominalInterval) + 10
	played, lostCount := driveJitterBufferSimulation(t, b, stream, start, nominalInterval, totalTicks, n)

	assertMonotonicNoDuplicates(t, played)

	stats := b.Stats()
	t.Logf("bursty-reorder simulation: n=%d received=%d lost=%d late=%d duplicates=%d evicted=%d played=%d",
		n, stats.Received, stats.Lost, stats.Late, stats.Duplicates, stats.EvictedForCapacity, len(played))

	assertPacketAccounting(t, played, lostCount, stats.Lost, n)

	if got := len(played); got < n-int(float64(n)*0.05) {
		t.Errorf("played %d of %d packets, expected nearly all of them to survive multi-position reordering given a generous TargetDelay", got, n)
	}
	if lostCount > int(float64(n)*0.05) {
		t.Errorf("lostCount = %d, implausibly high for a no-real-loss, reordering-only stream", lostCount)
	}
}

// simulateJitterSpikeStream generates n packets at nominalInterval with
// calm steady-state jitter (jitterStdDev), except for a run of spikeLen
// packets starting at spikeStart which are additionally delayed by
// spikeExtra -- simulating a sudden mid-call network hiccup (e.g. a brief
// Wi-Fi or backhaul stall) rather than steady jitter alone.
func simulateJitterSpikeStream(rng *rand.Rand, n int, start time.Time, nominalInterval, jitterStdDev time.Duration, spikeStart, spikeLen int, spikeExtra time.Duration) []simulatedPacket {
	pkts := make([]simulatedPacket, 0, n)
	for i := 0; i < n; i++ {
		nominal := start.Add(time.Duration(i) * nominalInterval)
		jitter := time.Duration(rng.NormFloat64() * float64(jitterStdDev))
		arrival := nominal.Add(jitter)
		if i >= spikeStart && i < spikeStart+spikeLen {
			arrival = arrival.Add(spikeExtra)
		}
		if arrival.Before(start) {
			arrival = start
		}
		pkts = append(pkts, simulatedPacket{seq: uint16(i), arrival: arrival, present: true})
	}

	for i := 1; i < len(pkts); i++ {
		for j := i; j > 0 && pkts[j].arrival.Before(pkts[j-1].arrival); j-- {
			pkts[j], pkts[j-1] = pkts[j-1], pkts[j]
		}
	}
	return pkts
}

// TestJitterBufferSuddenJitterSpikeMidStream models a sudden network
// hiccup partway through an otherwise-calm call: a fixed-delay jitter
// buffer (this package is explicitly non-adaptive, see jitter.go's package
// doc comment) cannot absorb an arbitrarily large one-off spike without
// dropping the packets caught in it, but it must not panic, wedge, or
// corrupt playout order -- it should keep making forward, in-order
// progress once the hiccup passes.
func TestJitterBufferSuddenJitterSpikeMidStream(t *testing.T) {
	const (
		n               = 400
		nominalInterval = 20 * time.Millisecond
		jitterStdDev    = 8 * time.Millisecond // calm steady-state jitter
		spikeStart      = 200
		spikeLen        = 15
		spikeExtra      = 300 * time.Millisecond // a sudden ~300ms hiccup
	)
	start := time.Unix(1_700_000_300, 0)

	rng := rand.New(rand.NewSource(303))
	stream := simulateJitterSpikeStream(rng, n, start, nominalInterval, jitterStdDev, spikeStart, spikeLen, spikeExtra)

	cfg := Config{
		TargetDelay:        100 * time.Millisecond, // deliberately can't fully absorb a 300ms spike
		PacketInterval:     nominalInterval,
		MaxPacketsBuffered: 64,
	}
	b := NewJitterBuffer(cfg)

	totalTicks := n + int((spikeExtra+cfg.TargetDelay)/nominalInterval) + int(5*jitterStdDev/nominalInterval) + 20
	played, lostCount := driveJitterBufferSimulation(t, b, stream, start, nominalInterval, totalTicks, n)

	assertMonotonicNoDuplicates(t, played)

	stats := b.Stats()
	t.Logf("jitter-spike simulation: n=%d received=%d lost=%d late=%d duplicates=%d evicted=%d played=%d",
		n, stats.Received, stats.Lost, stats.Late, stats.Duplicates, stats.EvictedForCapacity, len(played))

	assertPacketAccounting(t, played, lostCount, stats.Lost, n)

	if len(played) == 0 {
		t.Fatal("nothing played at all - buffer wedged during the jitter spike")
	}
	// The spike itself is expected to cause loss right around it (a fixed
	// TargetDelay can't absorb a 300ms hiccup on top of its normal 100ms
	// budget), but the buffer must recover and keep delivering most
	// non-spike packets, both before and after the hiccup.
	if got, want := len(played), n-spikeLen*3; got < want {
		t.Errorf("played %d of %d packets, expected the buffer to recover and deliver most non-spike packets after the hiccup (want at least %d)", got, n, want)
	}
	if lostCount == 0 {
		t.Error("expected some PullLost from the deliberately-unabsorbable jitter spike")
	}
}

// simulateHighLossSevereReorderStream combines simulateBurstyReorderStream's
// severe multi-position reordering (sequence labels shuffled within
// non-overlapping windows of windowSize packets, so a packet can arrive
// many positions out of order, not just swapped with its immediate
// neighbor) with simulatePSTNStream's independent per-packet loss --
// simultaneously, not in isolation the way
// TestJitterBufferUnderHarshPacketLoss (loss only, in-order-ish arrivals)
// and TestJitterBufferBurstyMultiPositionReordering (reordering only, no
// loss) each exercise separately. A real bad last-mile/Wi-Fi leg during
// network congestion routinely produces both at once, and the two
// failure modes can interact: a lost packet changes which sequence number
// the buffer is currently waiting on, which changes how "out of order" a
// later, reordered arrival looks relative to that wait -- a combination
// this package's existing scenarios never actually drive.
//
// As in simulateBurstyReorderStream, the earliest-arriving *surviving*
// (non-dropped) packet is guaranteed to carry the globally lowest
// surviving sequence number: JitterBuffer establishes its base
// sequence/arrival time from whichever packet is pushed first (see
// jitter.go's package doc comment), and any packet that arrives "before"
// that base in sequence-number terms is immediately rejected PushLate
// (see jitter.go's Push, the dist<0 branch) rather than legitimately
// reordered. Loss makes this trickier than in the no-loss reordering
// case: dropping the packet that would otherwise have arrived first can
// promote a later, higher-numbered packet into that "first arrival"
// slot, which is exactly the impossible-for-a-real-stream case that swap
// avoids.
func simulateHighLossSevereReorderStream(rng *rand.Rand, n int, start time.Time, nominalInterval time.Duration, windowSize int, lossP float64) []simulatedPacket {
	pkts := make([]simulatedPacket, n)
	for i := 0; i < n; i++ {
		pkts[i] = simulatedPacket{seq: uint16(i), present: true}
	}

	for ws := 0; ws < n; ws += windowSize {
		we := ws + windowSize
		if we > n {
			we = n
		}
		window := pkts[ws:we]
		rng.Shuffle(len(window), func(i, j int) { window[i], window[j] = window[j], window[i] })
	}

	for i := range pkts {
		if rng.Float64() < lossP {
			pkts[i].present = false
		}
	}

	// Guarantee the earliest-arriving present packet carries the globally
	// lowest surviving sequence number (see the doc comment above for
	// why), by swapping sequence labels between the first present slot
	// and whichever present slot holds the minimum surviving sequence
	// number.
	firstPresent := -1
	minSeqIdx := -1
	for i, p := range pkts {
		if !p.present {
			continue
		}
		if firstPresent == -1 {
			firstPresent = i
		}
		if minSeqIdx == -1 || p.seq < pkts[minSeqIdx].seq {
			minSeqIdx = i
		}
	}
	if firstPresent != -1 && minSeqIdx != -1 && firstPresent != minSeqIdx {
		pkts[firstPresent].seq, pkts[minSeqIdx].seq = pkts[minSeqIdx].seq, pkts[firstPresent].seq
	}

	present := pkts[:0:0]
	for i, p := range pkts {
		if !p.present {
			continue
		}
		p.arrival = start.Add(time.Duration(i) * nominalInterval)
		present = append(present, p)
	}
	return present
}

// TestJitterBufferSimultaneousHighLossAndSevereReordering drives
// simulateHighLossSevereReorderStream's combined ~15% loss + up-to-a-
// full-window severe reordering through the same
// driveJitterBufferSimulation harness as every other harsh scenario
// above, asserting the same invariants (monotonic in-order playout with
// no duplicates, the exact played+lost == n packet-accounting invariant,
// and staying within MaxPacketsBuffered) hold when both harsh conditions
// hit the buffer at once, not just when tested in isolation.
func TestJitterBufferSimultaneousHighLossAndSevereReordering(t *testing.T) {
	const (
		n               = 350
		nominalInterval = 20 * time.Millisecond
		windowSize      = 10   // packets can arrive up to ~9 positions out of order within a burst
		lossP           = 0.15 // ~15% loss, on top of the reordering
	)
	start := time.Unix(1_700_000_400, 0)

	rng := rand.New(rand.NewSource(404))
	stream := simulateHighLossSevereReorderStream(rng, n, start, nominalInterval, windowSize, lossP)

	cfg := Config{
		TargetDelay:        time.Duration(windowSize+2) * nominalInterval, // generous enough to absorb a full window's worth of reordering
		PacketInterval:     nominalInterval,
		MaxPacketsBuffered: 64,
	}
	b := NewJitterBuffer(cfg)

	totalTicks := n + int(cfg.TargetDelay/nominalInterval) + 10
	played, lostCount := driveJitterBufferSimulation(t, b, stream, start, nominalInterval, totalTicks, n)

	assertMonotonicNoDuplicates(t, played)

	stats := b.Stats()
	t.Logf("high-loss+severe-reorder simulation: n=%d received=%d lost=%d late=%d duplicates=%d evicted=%d played=%d",
		n, stats.Received, stats.Lost, stats.Late, stats.Duplicates, stats.EvictedForCapacity, len(played))

	assertPacketAccounting(t, played, lostCount, stats.Lost, n)

	if stats.Lost == 0 {
		t.Error("expected substantial PullLost given ~15% simulated packet loss combined with severe reordering")
	}
	// Bounded generously above the raw lossP since severe reordering on
	// top of loss can itself push a few additional close-to-the-margin
	// packets past their deadline (unlike the loss-only or reorder-only
	// scenarios above, which each budget for only one effect).
	if float64(stats.Lost) < float64(n)*0.08 || float64(stats.Lost) > float64(n)*0.35 {
		t.Errorf("Lost = %d is implausible for %d packets at %.0f%% simulated loss plus severe reordering with a %v target delay",
			stats.Lost, n, lossP*100, cfg.TargetDelay)
	}
	if len(played)+lostCount == 0 {
		t.Fatal("nothing played and nothing lost - buffer made no progress")
	}
}
