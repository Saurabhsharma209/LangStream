package rtp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// TestDuplexSession_BidirectionalConcurrent is TestDuplexSession_EndToEndLoopback's
// bidirectional counterpart: that test only ever drives audio into the
// caller leg and asserts on the agent leg's forward socket. A real call
// has both parties speaking, often overlapping, so this test drives BOTH
// legs' inbound RTP concurrently (from two goroutines, interleaved) against
// a single DuplexSession and asserts BOTH forward sockets see real,
// non-empty RTP carrying synthesized bot audio -- catching a bug that
// would only show up when both directions are actually live at once (e.g.
// a shared, not-actually-concurrent-safe resource between the caller-leg
// and agent-leg bridging goroutines), which neither the original
// caller-only test nor a "run the same one-directional test twice" would
// ever exercise.
func TestDuplexSession_BidirectionalConcurrent(t *testing.T) {
	logger := zap.NewNop()

	// callerFwdSink observes what the caller leg forwards -- this is
	// where translated audio for the *caller* to hear (bridged from
	// Session.CallerHearsAudio(), itself fed by agent-leg speech) should
	// land.
	callerFwdSink := bindLoopbackSink(t)
	defer callerFwdSink.Close()

	// agentFwdSink observes what the agent leg forwards -- this is where
	// translated audio for the *agent* to hear (bridged from
	// Session.AgentHearsAudio(), fed by caller-leg speech) should land.
	agentFwdSink := bindLoopbackSink(t)
	defer agentFwdSink.Close()

	callerListenAddr := newLoopbackPort(t)
	agentListenAddr := newLoopbackPort(t)

	ctx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()

	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asr.NewMockRecognizer("hi", "en"),
		Translator:     translate.NewMockTranslator([2]translate.Language{"hi", "en"}, [2]translate.Language{"en", "hi"}),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	})
	if err != nil {
		t.Fatalf("langstream.NewSession: %v", err)
	}

	duplex, err := NewDuplexSession(DuplexConfig{
		CallerLeg: LegConfig{
			ListenAddr:  callerListenAddr,
			ForwardAddr: callerFwdSink.LocalAddr().String(),
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		AgentLeg: LegConfig{
			ListenAddr:  agentListenAddr,
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
	defer func() {
		if err := duplex.Stop(); err != nil {
			t.Errorf("DuplexSession.Stop: %v", err)
		}
	}()

	callerSender, err := net.Dial("udp", callerListenAddr)
	if err != nil {
		t.Fatalf("dial caller leg: %v", err)
	}
	defer callerSender.Close()

	agentSender, err := net.Dial("udp", agentListenAddr)
	if err != nil {
		t.Fatalf("dial agent leg: %v", err)
	}
	defer agentSender.Close()

	payload := make([]byte, 160) // 20ms @ 8kHz PCMU
	for i := range payload {
		payload[i] = 0xFF // mu-law silence
	}

	// Drive both legs' inbound RTP concurrently from two goroutines, with
	// interleaved sends (not "caller fully done, then agent starts") so
	// both bridgeCleanAudio goroutines (caller->agent, agent->caller) and
	// both bridgeHears goroutines (agent-hears, caller-hears) are all
	// actually active and racing against each other at the same time --
	// the specific scenario the original single-direction test can't
	// reach.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xC0FFEE, payload)
			if _, err := callerSender.Write(pkt); err != nil {
				t.Errorf("send caller RTP packet %d: %v", i, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xFEEDFACE, payload)
			if _, err := agentSender.Write(pkt); err != nil {
				t.Errorf("send agent RTP packet %d: %v", i, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	wg.Wait()

	// Give both directions' packets time to propagate: ClearStream
	// receive -> denoise -> CleanAudio() -> bridgeCleanAudio ->
	// asr.PushAudio, for both legs concurrently.
	time.Sleep(200 * time.Millisecond)

	// Close the langstream.Session: MockRecognizer flushes a final
	// transcript for BOTH legs from Close(), which (per
	// TestDuplexSession_EndToEndLoopback's own comment) is what actually
	// drives synthesized bot audio out of AgentHearsAudio()/
	// CallerHearsAudio() in this mock setup.
	if err := sess.Close(); err != nil {
		t.Fatalf("langstream.Session.Close: %v", err)
	}

	// Both sockets must have received real, non-empty RTP -- proving
	// both directions of the SAME DuplexSession worked when driven
	// concurrently, not just once at a time.
	assertRTPArrives(t, "agentFwdSink (caller-leg speech translated for agent)", agentFwdSink)
	assertRTPArrives(t, "callerFwdSink (agent-leg speech translated for caller)", callerFwdSink)
}

// assertRTPArrives reads one packet from sink (with a bounded deadline)
// and fails t if it doesn't look like a real RTP packet (header + payload).
func assertRTPArrives(t *testing.T, label string, sink *net.UDPConn) {
	t.Helper()
	if err := sink.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("%s: set read deadline: %v", label, err)
	}
	buf := make([]byte, 2048)
	n, _, err := sink.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("%s: expected synthesized bot audio, got error: %v", label, err)
	}
	if n <= 12 {
		t.Fatalf("%s: expected an RTP packet with header+payload (>12 bytes), got %d bytes", label, n)
	}
}
