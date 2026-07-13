// Tech's own basic unit coverage for the TTS-pacing wiring added to
// duplex.go (feedTTSPacer/runTTSPacer): confirms synthesized audio pushed
// through the pacer still arrives, unmodified and in order, and that it
// is actually spread out over time rather than injected in one
// instantaneous burst -- the two properties the EM's repurposing decision
// (see jitter.go's package doc comment) depends on. QA is expected to add
// further independent integration coverage on top of this, per the
// sprint charter; this file only needs to prove the wiring itself works.
package rtp

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/langstream/pkg/tts"
)

// newTestPacingDuplexSession builds a *DuplexSession populated with only
// the fields feedTTSPacer/runTTSPacer (and Stop's context-cancellation
// backstop) actually touch -- no real ClearStream Session or
// langstream.Session is constructed, since those are already exercised
// end-to-end by TestDuplexSession_EndToEndLoopback in duplex_test.go. This
// lets the pacing bridge itself be tested in isolation, deterministically
// and quickly.
func newTestPacingDuplexSession(t *testing.T, pacingCfg Config) *DuplexSession {
	t.Helper()
	cfg := DuplexConfig{TTSPacing: pacingCfg}
	ctx, cancel := context.WithCancel(context.Background())
	d := &DuplexSession{
		cfg:         cfg,
		callerPacer: newTTSPacer(cfg.ttsPacingConfig()),
		agentPacer:  newTTSPacer(cfg.ttsPacingConfig()),
		logger:      zap.NewNop(),
		ctx:         ctx,
		cancel:      cancel,
	}
	t.Cleanup(cancel)
	return d
}

// injectRecorder stands in for a ClearStream leg's InjectBotAudio for
// these tests: it records every PCM payload handed to it (copied, since
// runTTSPacer reuses/frees Packet.Payload backing arrays across pulls)
// along with the wall-clock time it arrived, and always reports success.
type injectRecorder struct {
	mu  sync.Mutex
	got [][]byte
	at  []time.Time
}

func (r *injectRecorder) inject(pcm []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	r.got = append(r.got, cp)
	r.at = append(r.at, time.Now())
	return true
}

func (r *injectRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *injectRecorder) snapshot() ([][]byte, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.got...), append([]time.Time(nil), r.at...)
}

// waitForCount polls rec until it has recorded at least n injections, or
// fails the test after timeout.
func waitForCount(t *testing.T, rec *injectRecorder, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %d injected chunk(s), got %d", timeout, n, rec.count())
}

// TestTTSPacer_DeliversChunksInOrderUnmodified pushes a burst of
// synthesized chunks (all queued essentially at once, simulating bursty
// TTS generation) through feedTTSPacer/runTTSPacer and confirms every
// chunk still arrives at inject, unmodified, and in the same order they
// were synthesized in -- the pacing buffer must smooth *timing*, not drop
// or reorder content (see jitter.go's package doc comment: a single Go
// channel feeds feedTTSPacer, so there is no real reordering to do here,
// only release-timing smoothing).
func TestTTSPacer_DeliversChunksInOrderUnmodified(t *testing.T) {
	d := newTestPacingDuplexSession(t, Config{
		TargetDelay:    10 * time.Millisecond,
		PacketInterval: 5 * time.Millisecond,
	})

	in := make(chan tts.AudioChunk, 8)
	rec := &injectRecorder{}

	d.wg.Add(2)
	go d.feedTTSPacer(in, d.agentPacer, "agent")
	go d.runTTSPacer(d.agentPacer, rec.inject, "agent")

	const n = 5
	for i := 0; i < n; i++ {
		in <- tts.AudioChunk{PCM: []byte{byte(i)}, SampleRate: 16000, IsFinal: i == n-1}
	}
	close(in)

	waitForCount(t, rec, n, 2*time.Second)

	got, _ := rec.snapshot()
	if len(got) != n {
		t.Fatalf("got %d injected chunk(s), want exactly %d", len(got), n)
	}
	for i, pcm := range got {
		if len(pcm) != 1 || pcm[0] != byte(i) {
			t.Errorf("chunk[%d] = %v, want [%d] (chunks must arrive unmodified and in order)", i, pcm, i)
		}
	}

	d.cancel()
	waitGroupDone(t, &d.wg, 2*time.Second)
}

// TestTTSPacer_SpreadsBurstyChunksOverTime is the actual pacing/smoothing
// assertion: chunks pushed in a tight burst (all before the pacer's first
// tick) must not all be injected at once -- they must be spread out over
// at least roughly (n-1)*PacketInterval, proving feedTTSPacer/runTTSPacer
// actually paces release rather than just passing chunks through
// immediately (which would defeat the entire point of the EM's repurposing
// decision).
func TestTTSPacer_SpreadsBurstyChunksOverTime(t *testing.T) {
	const packetInterval = 15 * time.Millisecond
	d := newTestPacingDuplexSession(t, Config{
		TargetDelay:    packetInterval,
		PacketInterval: packetInterval,
	})

	in := make(chan tts.AudioChunk, 8)
	rec := &injectRecorder{}

	d.wg.Add(2)
	go d.feedTTSPacer(in, d.agentPacer, "agent")
	go d.runTTSPacer(d.agentPacer, rec.inject, "agent")

	const n = 4
	for i := 0; i < n; i++ {
		in <- tts.AudioChunk{PCM: []byte{byte(i)}, SampleRate: 16000, IsFinal: i == n-1}
	}
	close(in)

	waitForCount(t, rec, n, 3*time.Second)

	_, at := rec.snapshot()
	if len(at) != n {
		t.Fatalf("got %d timestamps, want %d", len(at), n)
	}
	spread := at[n-1].Sub(at[0])
	// Require at least half of the nominal (n-1)*packetInterval spread,
	// rather than the full amount, to keep this test robust against
	// scheduling jitter in a loaded CI/sandbox environment while still
	// clearly distinguishing "paced" from "instantaneous" (an
	// unpaced/bypassed implementation would have a spread on the order of
	// microseconds, not milliseconds).
	minSpread := time.Duration(n-1) * packetInterval / 2
	if spread < minSpread {
		t.Errorf("first-to-last injection spread = %s, want at least %s (chunks were not paced -- they arrived in a near-instantaneous burst)", spread, minSpread)
	}

	d.cancel()
	waitGroupDone(t, &d.wg, 2*time.Second)
}

// TestTTSPacer_StillDeliversBufferedChunkAfterFeedChannelCloses confirms
// runTTSPacer's documented behavior: it does not exit merely because its
// feedTTSPacer counterpart's input channel closed (e.g. because
// Session.Close() finished shutting down ASR/MT/TTS while a chunk was
// still sitting in the pacing buffer, not yet past its TargetDelay) --
// only d.ctx being cancelled (DuplexSession.Stop's backstop) should stop
// it, so a chunk pushed just before the channel closes is still released
// and injected rather than silently dropped.
func TestTTSPacer_StillDeliversBufferedChunkAfterFeedChannelCloses(t *testing.T) {
	d := newTestPacingDuplexSession(t, Config{
		TargetDelay:    30 * time.Millisecond,
		PacketInterval: 5 * time.Millisecond,
	})

	in := make(chan tts.AudioChunk, 1)
	rec := &injectRecorder{}

	d.wg.Add(2)
	go d.feedTTSPacer(in, d.agentPacer, "agent")
	go d.runTTSPacer(d.agentPacer, rec.inject, "agent")

	in <- tts.AudioChunk{PCM: []byte{0xAB}, SampleRate: 16000, IsFinal: true}
	close(in)

	// feedTTSPacer should exit almost immediately once the channel closes
	// (well before TargetDelay elapses), while runTTSPacer must still be
	// running and eventually deliver the buffered chunk.
	waitForCount(t, rec, 1, 2*time.Second)

	got, _ := rec.snapshot()
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != 0xAB {
		t.Fatalf("got %v, want a single [0xAB] chunk delivered after the feed channel closed", got)
	}

	d.cancel()
	waitGroupDone(t, &d.wg, 2*time.Second)
}

// waitGroupDone waits for wg.Wait() to return, failing the test if it
// does not within timeout -- used after d.cancel() to confirm
// feedTTSPacer/runTTSPacer actually exit instead of leaking.
func waitGroupDone(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("feedTTSPacer/runTTSPacer did not exit within %s of context cancellation -- possible goroutine leak", timeout)
	}
}
