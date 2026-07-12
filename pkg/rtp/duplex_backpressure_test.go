package rtp

import (
	"context"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// slowStreamSession is a asr.StreamSession whose PushAudio artificially
// blocks for delay before returning, simulating a momentarily slow (or
// briefly stalled) ASR backend. It never emits any transcript -- that is
// deliberate: this test only cares about whether DuplexSession's
// bridgeCleanAudio goroutine (and, transitively, ClearStream's own
// CleanAudio() drop-oldest backpressure) survives a slow consumer without
// panicking, deadlocking, or leaking, not about the translate/TTS side of
// the pipeline.
type slowStreamSession struct {
	delay  time.Duration
	out    chan asr.Transcript
	closed atomic.Bool
}

func newSlowStreamSession(delay time.Duration) *slowStreamSession {
	return &slowStreamSession{delay: delay, out: make(chan asr.Transcript)}
}

func (s *slowStreamSession) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowStreamSession) Transcripts() <-chan asr.Transcript { return s.out }

func (s *slowStreamSession) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.out)
	}
	return nil
}

var _ asr.StreamSession = (*slowStreamSession)(nil)

// slowRecognizer is an asr.Recognizer factory for slowStreamSession. Both
// the caller-leg and agent-leg StreamSessions langstream.NewSession opens
// against it are independently slow, but this test only ever drives
// audio into the caller leg.
type slowRecognizer struct {
	delay time.Duration
}

func (r *slowRecognizer) Name() string { return "slow-mock" }

func (r *slowRecognizer) SupportedLanguages() []asr.Language {
	return []asr.Language{"hi", "en"}
}

func (r *slowRecognizer) StartStream(ctx context.Context, languageHint asr.Language) (asr.StreamSession, error) {
	return newSlowStreamSession(r.delay), nil
}

var _ asr.Recognizer = (*slowRecognizer)(nil)

// TestDuplexSession_BackpressureDropOldestNoLeak floods the caller leg
// with far more RTP packets than a deliberately slow "ASR" consumer can
// keep up with, using a small CleanAudioBufferSize so ClearStream's
// documented non-blocking, drop-oldest-on-full CleanAudio() backpressure
// (see clearstream.Config.CleanAudioBufferSize's doc comment) actually
// triggers rather than merely being theoretically reachable. It then
// confirms DuplexSession survives that condition cleanly: no panic
// (a goroutine panic here would crash the whole test binary), no
// deadlock (Stop() is called with a bounded, goroutine-based deadline so
// a hang fails the test instead of hanging `go test` itself), and no
// leaked bridging goroutines (compares runtime.NumGoroutine() before
// Start and after Stop, with a small tolerance for scheduler/runtime
// noise).
func TestDuplexSession_BackpressureDropOldestNoLeak(t *testing.T) {
	logger := zap.NewNop()

	callerFwdSink := bindLoopbackSink(t)
	defer callerFwdSink.Close()
	agentFwdSink := bindLoopbackSink(t)
	defer agentFwdSink.Close()

	callerListenAddr := newLoopbackPort(t)

	ctx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()

	// A deliberately slow "ASR" backend: each PushAudio call blocks for
	// 30ms, far longer than it takes ClearStream's receiveLoop to fill a
	// tiny (4-frame) CleanAudio() buffer at the flood rate below.
	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            &slowRecognizer{delay: 30 * time.Millisecond},
		Translator:     translate.NewMockTranslator([2]translate.Language{"hi", "en"}, [2]translate.Language{"en", "hi"}),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	})
	if err != nil {
		t.Fatalf("langstream.NewSession: %v", err)
	}

	// Baseline goroutine count, taken right before building the duplex
	// session under test (after all the setup above has settled) so the
	// before/after comparison isolates goroutines DuplexSession itself is
	// responsible for.
	runtime.GC()
	before := runtime.NumGoroutine()

	duplex, err := NewDuplexSession(DuplexConfig{
		CallerLeg: LegConfig{
			ListenAddr:           callerListenAddr,
			ForwardAddr:          callerFwdSink.LocalAddr().String(),
			PayloadType:          0,
			JitterDepth:          1,
			Suppressor:           model.NewMockSuppressor(),
			Logger:               logger,
			CleanAudioBufferSize: 4, // tiny on purpose: forces the drop-oldest path fast
		},
		AgentLeg: LegConfig{
			ListenAddr:  "127.0.0.1:0",
			ForwardAddr: agentFwdSink.LocalAddr().String(),
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		Session: sess,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewDuplexSession: %v", err)
	}

	duplex.Start()

	sender, err := net.Dial("udp", callerListenAddr)
	if err != nil {
		t.Fatalf("dial caller leg: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 160) // 20ms @ 8kHz PCMU
	for i := range payload {
		payload[i] = 0xFF
	}

	// Flood: send far more packets, far faster, than the 30ms-per-frame
	// slow consumer (bridgeCleanAudio -> slowStreamSession.PushAudio) can
	// drain, with a CleanAudioBufferSize of 4 -- this reliably exercises
	// the drop-oldest branch in ClearStream's handlePacket many times
	// over, not just once at the margin.
	const floodPackets = 300
	for i := 0; i < floodPackets; i++ {
		pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xC0FFEE, payload)
		if _, err := sender.Write(pkt); err != nil {
			t.Fatalf("send RTP packet %d: %v", i, err)
		}
		// No sleep (or a tiny one): the whole point is to send faster
		// than the consumer can keep up, not to pace with it.
	}

	// Give the flood a moment to actually land and be processed/dropped.
	time.Sleep(300 * time.Millisecond)

	// Stop must return promptly even though bridgeCleanAudio may
	// currently be mid-PushAudio (blocked up to 30ms). Bound the wait
	// ourselves so a real hang fails this test instead of hanging `go
	// test` itself (duplexStopTimeout is an internal 3s backstop inside
	// Stop, but we don't want to just trust that from the outside).
	stopErrCh := make(chan error, 1)
	go func() { stopErrCh <- duplex.Stop() }()
	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Errorf("DuplexSession.Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DuplexSession.Stop did not return within 5s under flood/backpressure -- suspected deadlock")
	}

	// Session.Close() isn't owned by DuplexSession (see DuplexConfig.Session's
	// doc comment); close it ourselves to let slowStreamSession.Close()
	// run and its goroutines (if any were counted in "before") unwind too.
	closeErrCh := make(chan error, 1)
	go func() { closeErrCh <- sess.Close() }()
	select {
	case err := <-closeErrCh:
		if err != nil {
			t.Errorf("langstream.Session.Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("langstream.Session.Close did not return within 5s -- suspected deadlock")
	}

	// Let anything genuinely still unwinding (e.g. GC, runtime bookkeeping)
	// settle before taking the final count.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	const tolerance = 3
	if after > before+tolerance {
		t.Errorf("possible goroutine leak: NumGoroutine before=%d after=%d (tolerance=%d) -- "+
			"DuplexSession's bridging goroutines may not all be exiting cleanly under backpressure",
			before, after, tolerance)
	}
}
