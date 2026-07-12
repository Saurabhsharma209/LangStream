package rtp

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// --- PCM conversion helper tests -------------------------------------------

// TestInt16PCMToBytesEndianness pins down the exact byte layout
// int16PCMToBytes produces, so a future refactor can't silently flip to
// big-endian (which would corrupt every asr.AudioFrame this package
// builds, since asr.AudioFrame.PCM is documented as "16-bit LE, mono").
func TestInt16PCMToBytesEndianness(t *testing.T) {
	in := []int16{0, 1, -1, 32767, -32768}
	got := int16PCMToBytes(in)

	want := []byte{
		0x00, 0x00, // 0
		0x01, 0x00, // 1
		0xFF, 0xFF, // -1
		0xFF, 0x7F, // 32767
		0x00, 0x80, // -32768
	}
	if len(got) != len(want) {
		t.Fatalf("int16PCMToBytes(%v): got %d bytes, want %d", in, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("int16PCMToBytes(%v)[%d] = 0x%02X, want 0x%02X", in, i, got[i], want[i])
		}
	}
}

// TestPCMBytesToInt16Endianness is int16PCMToBytesEndianness's mirror for
// the reverse conversion.
func TestPCMBytesToInt16Endianness(t *testing.T) {
	in := []byte{0x00, 0x00, 0x01, 0x00, 0xFF, 0xFF, 0xFF, 0x7F, 0x00, 0x80}
	want := []int16{0, 1, -1, 32767, -32768}

	got := pcmBytesToInt16(in)
	if len(got) != len(want) {
		t.Fatalf("pcmBytesToInt16(%v): got %d samples, want %d", in, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pcmBytesToInt16(%v)[%d] = %d, want %d", in, i, got[i], want[i])
		}
	}
}

// TestPCMRoundTrip checks int16PCMToBytes and pcmBytesToInt16 are true
// inverses across a range of values, including negative numbers and both
// amplitude extremes, not just the small worked example above.
func TestPCMRoundTrip(t *testing.T) {
	samples := make([]int16, 0, 512)
	for v := -32768; v <= 32767; v += 137 { // odd stride to exercise varied bit patterns
		samples = append(samples, int16(v))
	}

	bytes := int16PCMToBytes(samples)
	if len(bytes) != len(samples)*2 {
		t.Fatalf("int16PCMToBytes: got %d bytes for %d samples, want %d", len(bytes), len(samples), len(samples)*2)
	}

	back := pcmBytesToInt16(bytes)
	if len(back) != len(samples) {
		t.Fatalf("round trip: got %d samples back, want %d", len(back), len(samples))
	}
	for i, want := range samples {
		if back[i] != want {
			t.Errorf("round trip[%d] = %d, want %d", i, back[i], want)
		}
	}
}

// TestPCMBytesToInt16OddLength checks the documented "drop a trailing odd
// byte" behavior instead of panicking or reading out of bounds.
func TestPCMBytesToInt16OddLength(t *testing.T) {
	in := []byte{0x01, 0x00, 0xAA} // one full sample + one dangling byte
	got := pcmBytesToInt16(in)
	if len(got) != 1 {
		t.Fatalf("pcmBytesToInt16(odd length): got %d samples, want 1", len(got))
	}
	if got[0] != 1 {
		t.Errorf("pcmBytesToInt16(odd length)[0] = %d, want 1", got[0])
	}
}

// --- lifecycle / end-to-end test --------------------------------------------

// buildRawRTPPacket creates a minimal valid RTP packet for testing,
// mirroring ClearStream's own pkg/rtp session_test.go helper of the same
// name (that one is unexported and in ClearStream's package, so it isn't
// reachable from here) -- this package intentionally doesn't reimplement
// real RTP framing anywhere outside this _test.go file.
func buildRawRTPPacket(seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	buf := make([]byte, 12+len(payload))
	buf[0] = 0x80 // Version=2, no padding, no extension, CSRC count=0
	buf[1] = 0x00 // marker=0, payload type=0 (PCMU)
	binary.BigEndian.PutUint16(buf[2:4], seq)
	binary.BigEndian.PutUint32(buf[4:8], ts)
	binary.BigEndian.PutUint32(buf[8:12], ssrc)
	copy(buf[12:], payload)
	return buf
}

// bindLoopbackSink opens a UDP socket on 127.0.0.1 with an OS-assigned
// port, for use either as a ClearStream leg's ForwardAddr (to observe what
// it sends out) or as the address a test sends inbound RTP to.
func bindLoopbackSink(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind loopback UDP socket: %v", err)
	}
	return conn
}

// TestDuplexSession_EndToEndLoopback is the lifecycle test: it builds two
// real ClearStream rtp.Sessions (caller leg, agent leg) on loopback UDP
// with a mock suppressor, wraps them in a DuplexSession bridged to a real
// langstream.Session built from this repo's existing mock ASR/Translate/
// TTS backends, starts everything, sends a few real RTP packets into the
// caller leg's socket, then closes the langstream.Session (flushing a
// final transcript on both legs, since MockRecognizer only ever emits a
// final transcript from its Close() flush -- see pkg/asr/mock.go) and
// confirms real RTP packets carrying synthesized "bot" audio come out the
// agent leg's ForwardAddr socket, proving the whole
//
//	real RTP in -> ClearStream denoise -> CleanAudio() -> ASR -> MT -> TTS
//	-> InjectBotAudio -> ClearStream playback -> real RTP out
//
// path is wired correctly end-to-end, not just that the pieces compile
// together.
func TestDuplexSession_EndToEndLoopback(t *testing.T) {
	logger := zap.NewNop()

	// Sink for the caller leg's forwarded (denoised, pass-through)
	// telephony-side RTP -- we don't assert on this, but the caller leg
	// still needs somewhere to forward to.
	callerSink := bindLoopbackSink(t)
	defer callerSink.Close()

	// Sink for the agent leg's forwarded RTP -- this is what we actually
	// assert on: synthesized bot audio (translated from the caller's
	// speech) injected via InjectBotAudio should be encoded and sent here.
	agentSink := bindLoopbackSink(t)
	defer agentSink.Close()

	// ClearStream's Session does not expose its bound listen address to
	// callers outside its own package (see NewDuplexSession's doc comment
	// on the Stop-before-Start socket-release gap for the same kind of
	// limitation). To give this test's own RTP sender something concrete
	// to dial, learn a free loopback port up front via newLoopbackPort and
	// pass that literal address to both the caller leg's config and the
	// sender below.
	callerListenAddr := newLoopbackPort(t)

	// Build the real langstream.Session with this repo's mock backends.
	// hi<->en is MockTranslator's default supported pair.
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
			ForwardAddr: callerSink.LocalAddr().String(),
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		AgentLeg: LegConfig{
			ListenAddr:  "127.0.0.1:0",
			ForwardAddr: agentSink.LocalAddr().String(),
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

	// Send a handful of real RTP packets (PCMU mu-law silence, matching
	// ClearStream's own test fixtures) into the caller leg's socket, so
	// real audio actually flows: real RTP in -> ClearStream receive ->
	// denoise -> CleanAudio() -> bridgeCleanAudio -> asr.PushAudio.
	sender, err := net.Dial("udp", callerListenAddr)
	if err != nil {
		t.Fatalf("dial caller leg: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 160) // 20ms @ 8kHz PCMU
	for i := range payload {
		payload[i] = 0xFF // mu-law silence
	}
	for i := 0; i < 6; i++ {
		pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xC0FFEE, payload)
		if _, err := sender.Write(pkt); err != nil {
			t.Fatalf("send RTP packet %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give the packets time to propagate: ClearStream's receive loop ->
	// denoise -> CleanAudio() channel -> bridgeCleanAudio -> asr.PushAudio.
	time.Sleep(200 * time.Millisecond)

	// Close the langstream.Session now: MockRecognizer only ever emits a
	// *final* transcript (the kind langstream.Session's runLeg actually
	// translates/synthesizes -- partial transcripts are intentionally
	// skipped, see session.go's runLeg) from its StreamSession.Close()
	// flush (see pkg/asr/mock.go). Session.Close() closes both legs' ASR
	// streams (triggering that flush), waits for each leg's translate +
	// synthesize + forward to complete, then closes AgentHearsAudio()/
	// CallerHearsAudio() -- exactly the chunk bridgeHears (already running
	// since duplex.Start() above) is waiting to forward to the agent
	// leg's InjectBotAudio.
	if err := sess.Close(); err != nil {
		t.Fatalf("langstream.Session.Close: %v", err)
	}

	// Finally: read from agentSink and confirm real, non-empty RTP
	// packets carrying synthesized bot audio actually arrived -- proving
	// the full real-RTP-in -> ... -> real-RTP-out path, not just that the
	// pieces compile together.
	if err := agentSink.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := agentSink.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected synthesized bot audio on agentSink, got error: %v", err)
	}
	if n <= 12 {
		t.Fatalf("expected an RTP packet with header+payload (>12 bytes), got %d bytes", n)
	}
}

// newLoopbackPort binds a UDP socket on 127.0.0.1 with an OS-assigned
// port, immediately closes it, and returns that address -- used to learn
// a concrete port number to pass to *both* a ClearStream leg's ListenAddr
// and this test's own RTP sender, since ClearStream's Session does not
// expose its actual bound address to callers outside its own package.
// This has a small, unavoidable close-then-rebind race in principle
// (another process could grab the port first), but it is the standard
// pragmatic pattern for this kind of test.
func newLoopbackPort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("newLoopbackPort: %v", err)
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatalf("newLoopbackPort: closing probe socket: %v", err)
	}
	return addr
}
