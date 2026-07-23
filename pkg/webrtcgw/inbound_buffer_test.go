package webrtcgw

import (
	"testing"
	"time"
)

// TestInboundBuffer_FlushesOnceThresholdReached is the direct unit test
// for the real bug fix described in inboundBufferDuration's doc comment:
// small (RTP-packet-sized) additions must accumulate rather than being
// forwarded one at a time, and onFull must fire exactly once the
// configured duration's worth of bytes has arrived.
func TestInboundBuffer_FlushesOnceThresholdReached(t *testing.T) {
	var calls [][]byte
	// 100ms @ 8kHz/16-bit mono = 1600 bytes.
	b := newInboundBuffer(100*time.Millisecond, func(pcm []byte) {
		calls = append(calls, append([]byte{}, pcm...))
	})

	// Five 20ms-equivalent additions of 320 bytes each = 1600 bytes exactly.
	frame := make([]byte, 320)
	for i := 0; i < 4; i++ {
		b.add(frame)
		if len(calls) != 0 {
			t.Fatalf("onFull called after %d frames (%d bytes), want no call before reaching 1600 bytes", i+1, (i+1)*320)
		}
	}
	b.add(frame) // 5th frame crosses 1600 bytes.
	if len(calls) != 1 {
		t.Fatalf("onFull called %d times after crossing the threshold, want exactly 1", len(calls))
	}
	if len(calls[0]) != 1600 {
		t.Errorf("flushed buffer was %d bytes, want 1600", len(calls[0]))
	}
}

// TestInboundBuffer_ResetsAfterFlush verifies a second round of additions
// after a flush starts a fresh buffer rather than accumulating on top of
// stale data.
func TestInboundBuffer_ResetsAfterFlush(t *testing.T) {
	var calls [][]byte
	b := newInboundBuffer(100*time.Millisecond, func(pcm []byte) {
		calls = append(calls, append([]byte{}, pcm...))
	})

	full := make([]byte, 1600)
	for i := range full {
		full[i] = 0xAB
	}
	b.add(full)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call after the first full buffer, got %d", len(calls))
	}

	second := make([]byte, 800)
	for i := range second {
		second[i] = 0xCD
	}
	b.add(second)
	if len(calls) != 1 {
		t.Fatalf("a half-full second buffer should not have flushed yet, got %d calls", len(calls))
	}
	b.add(second)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls once the second buffer also reached 1600 bytes, got %d", len(calls))
	}
	for _, b := range calls[1] {
		if b != 0xCD {
			t.Fatal("second flush contained bytes from the first (stale) buffer -- buffer was not reset after flush")
		}
	}
}

// TestInboundBuffer_FlushForcesPartialDelivery is the other half of the
// real-world fix: when a call hangs up (or a track otherwise ends)
// mid-utterance, whatever has accumulated so far -- even if under the
// configured threshold -- must still be delivered via an explicit flush()
// call, not silently dropped. This is what prevents losing the last
// few hundred milliseconds of a real hung-up call's speech.
func TestInboundBuffer_FlushForcesPartialDelivery(t *testing.T) {
	var calls [][]byte
	b := newInboundBuffer(1*time.Second, func(pcm []byte) {
		calls = append(calls, append([]byte{}, pcm...))
	})

	partial := make([]byte, 500)
	b.add(partial)
	if len(calls) != 0 {
		t.Fatal("onFull fired before reaching the threshold and before any explicit flush")
	}

	b.flush()
	if len(calls) != 1 {
		t.Fatalf("flush() should force delivery of the partial buffer, got %d calls", len(calls))
	}
	if len(calls[0]) != 500 {
		t.Errorf("flushed partial buffer was %d bytes, want 500", len(calls[0]))
	}
}

// TestInboundBuffer_FlushOnEmptyBufferIsNoop verifies calling flush() with
// nothing buffered (e.g. a track that ends immediately, or a second
// flush() call in a row) doesn't call onFull with an empty/zero-length
// payload.
func TestInboundBuffer_FlushOnEmptyBufferIsNoop(t *testing.T) {
	calls := 0
	b := newInboundBuffer(100*time.Millisecond, func(pcm []byte) { calls++ })

	b.flush()
	if calls != 0 {
		t.Errorf("flush() on an empty buffer called onFull %d times, want 0", calls)
	}

	frame := make([]byte, 1600)
	b.add(frame)
	if calls != 1 {
		t.Fatalf("expected exactly 1 call after a full buffer, got %d", calls)
	}
	b.flush() // buffer is empty again after the automatic flush above.
	if calls != 1 {
		t.Errorf("flush() on an already-empty buffer called onFull again (now %d times total), want still 1", calls)
	}
}

// TestInboundBuffer_FlushDeliversUnalignedLength is the pinning
// regression test for the 2026-07-23 frame-alignment audit documented in
// inbound_buffer.go's doc comment: unlike ClearStream's InjectBotAudio
// (see pkg/rtp/duplex.go's ttsFrameBytes doc comment, and the
// 2026-07-22 feedTTSPacer chunk-boundary fix it describes), nothing
// downstream of inboundBuffer requires onFull's payload to land on any
// particular sample/frame boundary -- so inboundBuffer must never
// silently pad or truncate to one either. This drives both the
// threshold-triggered flush and the forced flush() path with byte counts
// that are deliberately not multiples of 320 (160 samples @ 16-bit) or
// even 2 (one sample), and asserts the exact byte count survives
// unchanged in both cases.
func TestInboundBuffer_FlushDeliversUnalignedLength(t *testing.T) {
	var calls [][]byte
	// 125ms @ 8kHz/16-bit mono = 2000 bytes -- not a multiple of 320
	// (one 160-sample/20ms ClearStream InjectBotAudio frame), so crossing
	// it lands on a genuinely unaligned boundary.
	const target = 2000
	b := newInboundBuffer(125*time.Millisecond, func(pcm []byte) {
		calls = append(calls, append([]byte{}, pcm...))
	})

	first := make([]byte, target-3) // deliberately 3 bytes short of target
	b.add(first)
	if len(calls) != 0 {
		t.Fatal("onFull fired before reaching target, want no call yet")
	}

	second := make([]byte, 7) // odd length; pushes total 4 bytes past target
	b.add(second)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 onFull call after crossing target, got %d", len(calls))
	}
	wantLen := len(first) + len(second)
	if len(calls[0]) != wantLen {
		t.Fatalf("flushed buffer was %d bytes, want %d (unaligned length preserved exactly, not padded/truncated)", len(calls[0]), wantLen)
	}

	// Forced flush() (track end/room teardown) of a deliberately
	// odd-length partial buffer must also come through byte-for-byte.
	partial := make([]byte, 501) // not a multiple of 320 or of 2
	b.add(partial)
	b.flush()
	if len(calls) != 2 {
		t.Fatalf("expected 2 total onFull calls after the forced flush, got %d", len(calls))
	}
	if len(calls[1]) != len(partial) {
		t.Fatalf("forced flush delivered %d bytes, want %d (unaligned length preserved exactly, not padded/truncated)", len(calls[1]), len(partial))
	}
}
