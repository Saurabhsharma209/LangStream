// Package langstream_test - QA's independent verification of Tech's TTS
// chunk-boundary fix (pkg/rtp/duplex.go's feedTTSPacer/ttsFrameBytes),
// which Tech's own sprint work attributes to the live-pilot "voice
// distortion" complaint: before the fix, feedTTSPacer handed each raw
// tts.AudioChunk straight to ClearStream's InjectBotAudio, and
// InjectBotAudio silence-pads (discarding the true continuation) any
// trailing partial 160-sample/320-byte frame on *every single call* - so
// any TTS chunk whose byte length wasn't an exact multiple of 320 got
// truncated/padded at a non-sample-aligned point on every chunk boundary.
// The fix accumulates PCM to clean 320-byte boundaries before ever
// pushing a packet, carrying any remainder forward, and only flushing a
// true partial tail at utterance end (IsFinal) or feed-channel close.
//
// Tech's own pkg/rtp/tts_pacing_test.go
// (TestTTSPacer_AccumulatesPartialFramesAcrossChunkBoundaries) already
// proves this at the feedTTSPacer level directly: it manually feeds
// hand-built tts.AudioChunks into feedTTSPacer's input channel and
// inspects what reaches a fake in-package injectRecorder standing in for
// InjectBotAudio. What that test cannot do, because it lives inside
// package rtp and deliberately bypasses both langstream.Session and any
// real ClearStream Session (see its own package doc comment), is prove
// the fix holds when driven by the *real* ASR->MT->TTS pipeline and
// bridged through a *real* ClearStream Session all the way out onto real
// RTP packets on the wire - which is exactly what a live call actually
// exercises.
//
// This file closes that gap from an external, exported-API-only vantage
// point (matching this repo's established root-level integration-test
// convention - see dead_leg_drain_integration_test.go's and
// asr_permanent_failure_integration_test.go's package doc comments for
// the same pattern): a real *langstream.Session (real
// translate.MockTranslator, a small local fake ASR that hands the
// session one controlled final transcript, and a local fake TTS backend
// that deliberately emits realistic-but-inconvenient, non-320-byte-
// aligned chunk sizes such as 137 and 501 bytes) bridged through a real
// *rtp.DuplexSession with two real ClearStream leg Sessions on loopback
// UDP, reading the actual RTP packets that land on the agent leg's
// forward socket.
//
// Because ClearStream's InjectBotAudio always encodes outbound PCM
// through G.711 mu-law before putting it on the wire (there is no linear/
// passthrough codec option reachable via pkg/rtp.LegConfig), an exact
// byte-for-byte comparison against the original PCM is not directly
// possible once audio has gone out over real RTP - but G.711 mu-law is a
// deterministic, standard (RFC 3551), well-defined companding codec, so
// this file independently reimplements the same encode/decode formulas
// (testLinearToMulaw/testMulawToLinear below) to compute exactly what
// quantized sample value each original sample must decode back to, and
// checks the real, received wire bytes against that - giving a genuine
// sample-for-sample correctness check of what fed into InjectBotAudio,
// not just "some non-empty audio arrived".
package langstream_test

import (
	"context"
	"encoding/binary"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/rtp"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// wireFrameBytes mirrors pkg/rtp's unexported ttsFrameBytes constant
// (160 samples * 2 bytes/sample @ 8kHz = 320 bytes): the frame size
// ClearStream's InjectBotAudio encodes outbound bot audio in, and thus
// the boundary feedTTSPacer's accumulator now aligns to before ever
// calling InjectBotAudio. Duplicated here (not imported - it is
// unexported) because this file deliberately only uses pkg/rtp's
// exported surface.
const wireFrameBytes = 320

// --- local fake ASR: hands the Session exactly one controlled final
// transcript on demand, independent of any audio actually being pushed
// or of asr.MockRecognizer's own byte-accumulation-based flush timing. ---

// fixedTranscriptStream is a minimal asr.StreamSession test double whose
// Transcripts() channel is only ever written to by an explicit call to
// emitFinal, giving this file precise control over exactly when (and
// with what text) langstream.Session's runLeg proceeds to
// Translate/SynthesizeStream - the real code path this file needs to
// drive, not any particular ASR vendor's timing quirks.
type fixedTranscriptStream struct {
	mu     sync.Mutex
	out    chan asr.Transcript
	closed bool
}

func newFixedTranscriptStream() *fixedTranscriptStream {
	return &fixedTranscriptStream{out: make(chan asr.Transcript, 4)}
}

func (s *fixedTranscriptStream) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *fixedTranscriptStream) Transcripts() <-chan asr.Transcript { return s.out }

func (s *fixedTranscriptStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.out)
	}
	return nil
}

// emitFinal sends one IsFinal=true transcript with a comfortably-above-
// threshold confidence, so runLeg's ConfidenceThreshold fallback check
// never triggers and this always proceeds straight to Translate/TTS.
func (s *fixedTranscriptStream) emitFinal(text string) {
	s.out <- asr.Transcript{Text: text, IsFinal: true, Confidence: 0.95}
}

// fixedTranscriptRecognizer is a minimal asr.Recognizer test double that
// hands out fixedTranscriptStream instances and records them in call
// order - matching langstream.NewSession's own StartStream call order
// (caller leg first, then agent leg; see session.go's NewSession), same
// convention as asr_permanent_failure_integration_test.go's
// permaFailRecognizer.
type fixedTranscriptRecognizer struct {
	mu      sync.Mutex
	streams []*fixedTranscriptStream
}

func (r *fixedTranscriptRecognizer) Name() string { return "chunk-boundary-fake-asr" }

func (r *fixedTranscriptRecognizer) SupportedLanguages() []asr.Language {
	return []asr.Language{"hi", "en"}
}

func (r *fixedTranscriptRecognizer) StartStream(ctx context.Context, hint asr.Language) (asr.StreamSession, error) {
	s := newFixedTranscriptStream()
	r.mu.Lock()
	r.streams = append(r.streams, s)
	r.mu.Unlock()
	return s, nil
}

// callerStream returns the caller leg's stream (always index 0, per
// NewSession's StartStream call order).
func (r *fixedTranscriptRecognizer) callerStream() *fixedTranscriptStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[0]
}

var _ asr.Recognizer = (*fixedTranscriptRecognizer)(nil)
var _ asr.StreamSession = (*fixedTranscriptStream)(nil)

// --- local fake TTS: emits a pre-scripted sequence of deliberately
// non-320-byte-aligned PCM chunks, ignoring text/persona entirely. ---

// scriptedChunk describes one PCM chunk a scriptedSynthesizer emits: n
// bytes, every byte set to fill (so ordering/provenance stays
// identifiable after accumulation reshuffles chunk boundaries), with
// isFinal marking the true end of the synthesized utterance.
type scriptedChunk struct {
	n       int
	fill    byte
	isFinal bool
}

// scriptedSynthesizer is a tts.Synthesizer test double that always emits
// the same pre-scripted chunk sequence, each separated by a small
// non-zero delay (so this exercises real producer/consumer timing rather
// than an instantaneous slice handoff, the same shape a real streaming
// vendor's WebSocket chunks would arrive in).
type scriptedSynthesizer struct {
	script []scriptedChunk
}

func (s *scriptedSynthesizer) Name() string { return "chunk-boundary-fake-tts" }

func (s *scriptedSynthesizer) SupportedLanguages() []tts.Language {
	return []tts.Language{"en", "hi"}
}

func (s *scriptedSynthesizer) SynthesizeStream(ctx context.Context, text string, persona tts.Persona) (<-chan tts.AudioChunk, error) {
	out := make(chan tts.AudioChunk, len(s.script))
	go func() {
		defer close(out)
		for _, c := range s.script {
			pcm := make([]byte, c.n)
			for i := range pcm {
				pcm[i] = c.fill
			}
			chunk := tts.AudioChunk{PCM: pcm, SampleRate: 8000, IsFinal: c.isFinal}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(2 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

var _ tts.Synthesizer = (*scriptedSynthesizer)(nil)

// --- independent G.711 mu-law reimplementation (RFC 3551) -------------

// testLinearToMulaw/testMulawToLinear reimplement the standard G.711
// mu-law codec independently of ClearStream's own (unexported,
// unreachable from here) encodeG711U/decodeG711U in
// github.com/Saurabhsharma209/ClearStream's pkg/rtp/session.go. Both
// implement the identical, standard, bit-exact RFC 3551 algorithm, so
// this file can compute - on its own, without importing anything
// ClearStream-internal - exactly what quantized sample value a given
// original 16-bit PCM sample must come back as after passing through
// ClearStream's real encoder on the wire and being decoded back here.
// This is what makes a genuine sample-for-sample correctness check of
// real, received RTP payloads possible despite the encode being lossy.
func testLinearToMulaw(sample int16) byte {
	const bias = 0x84
	sign := byte(0)
	if sample < 0 {
		sample = -sample
		sign = 0x80
	}
	if sample > 32635 {
		sample = 32635
	}
	s := int(sample) + bias
	var exp byte
	switch {
	case s&0x4000 != 0:
		exp = 7
	case s&0x2000 != 0:
		exp = 6
	case s&0x1000 != 0:
		exp = 5
	case s&0x0800 != 0:
		exp = 4
	case s&0x0400 != 0:
		exp = 3
	case s&0x0200 != 0:
		exp = 2
	case s&0x0100 != 0:
		exp = 1
	default:
		exp = 0
	}
	mantissa := byte((s >> uint(exp+3)) & 0x0F)
	return ^(sign | (exp << 4) | mantissa)
}

func testMulawToLinear(mulaw byte) int16 {
	mulaw = ^mulaw
	t := int32((mulaw&0x0F)<<3) + 132
	t <<= (mulaw & 0x70) >> 4
	if mulaw&0x80 != 0 {
		return int16(132 - t)
	}
	return int16(t - 132)
}

// testQuantize applies testLinearToMulaw then testMulawToLinear, giving
// the exact lossy-but-deterministic value an original PCM sample must
// come back as once round-tripped through the real G.711 mu-law encoder
// ClearStream's InjectBotAudio always uses.
func testQuantize(sample int16) int16 {
	return testMulawToLinear(testLinearToMulaw(sample))
}

// --- RTP receive helpers -----------------------------------------------

// bindLoopbackUDP opens a UDP socket on 127.0.0.1 with an OS-assigned
// port, for use as a ClearStream leg's ForwardAddr (to observe what it
// sends) or as an inbound listen target.
func bindLoopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind loopback UDP socket: %v", err)
	}
	return conn
}

// freeLoopbackAddr learns a concrete, currently-free loopback UDP address
// by binding then immediately closing a probe socket - needed because
// ClearStream's Session does not expose its own bound listen address, so
// a LegConfig.ListenAddr must be chosen up front by the caller.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("freeLoopbackAddr: %v", err)
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatalf("freeLoopbackAddr: closing probe socket: %v", err)
	}
	return addr
}

// recvRTPPacket is one received RTP packet's sequence number (for
// reordering-safe reassembly - UDP, even on loopback, does not guarantee
// receive order matches send order) and payload (the RTP header's
// trailing bytes, i.e. the G.711-encoded audio for that packet).
type recvRTPPacket struct {
	seq     uint16
	payload []byte
}

// recvRTPPackets reads exactly want RTP packets from conn (each read
// parsed as a minimal RTP header: sequence number at bytes [2:4], big
// endian, payload starting at byte 12 - matching pkg/rtp's own internal
// RTP framing, which this file cannot import directly since it is
// unexported there too), or fails the test if timeout elapses first.
func recvRTPPackets(t *testing.T, conn *net.UDPConn, want int, timeout time.Duration) []recvRTPPacket {
	t.Helper()
	got := make([]recvRTPPacket, 0, want)
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 2048)
	for len(got) < want {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out after %s waiting for %d RTP packet(s), got %d", timeout, want, len(got))
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("waiting for RTP packet %d/%d: %v", len(got)+1, want, err)
		}
		if n < 12 {
			t.Fatalf("received a packet shorter than a minimal RTP header (%d bytes)", n)
		}
		seq := binary.BigEndian.Uint16(buf[2:4])
		payload := append([]byte(nil), buf[12:n]...)
		got = append(got, recvRTPPacket{seq: seq, payload: payload})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].seq < got[j].seq })
	return got
}

// assertNoMoreRTPPackets confirms conn receives nothing further within
// window - used to prove the accumulator's carry was fully and correctly
// flushed exactly once, not left to dribble out extra stray frames later
// (a duplication bug) or never flushed at all (a leak, checked instead by
// recvRTPPackets's own exact-count requirement above).
func assertNoMoreRTPPackets(t *testing.T, conn *net.UDPConn, window time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err == nil {
		t.Fatalf("received an unexpected extra RTP packet (%d bytes) after the expected sequence - suggests a duplicated/leaked chunk-boundary flush", n)
	}
}

// --- harness -------------------------------------------------------------

// chunkBoundaryHarness bundles the real components this file's tests
// drive audio through: a real *langstream.Session (fixedTranscriptRecognizer
// + real translate.MockTranslator + the given scripted TTS backend)
// bridged through a real *rtp.DuplexSession with two real ClearStream
// Sessions on loopback UDP.
type chunkBoundaryHarness struct {
	rec       *fixedTranscriptRecognizer
	sess      *langstream.Session
	duplex    *rtp.DuplexSession
	agentSink *net.UDPConn
}

func newChunkBoundaryHarness(t *testing.T, ttsBackend tts.Synthesizer) *chunkBoundaryHarness {
	t.Helper()
	logger := zap.NewNop()

	callerSink := bindLoopbackUDP(t)
	t.Cleanup(func() { callerSink.Close() })

	agentSink := bindLoopbackUDP(t)
	t.Cleanup(func() { agentSink.Close() })

	rec := &fixedTranscriptRecognizer{}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     translate.NewMockTranslator(),
		TTS:            ttsBackend,
	})
	if err != nil {
		t.Fatalf("langstream.NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	duplex, err := rtp.NewDuplexSession(rtp.DuplexConfig{
		CallerLeg: rtp.LegConfig{
			ListenAddr:  freeLoopbackAddr(t),
			ForwardAddr: callerSink.LocalAddr().String(),
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		AgentLeg: rtp.LegConfig{
			ListenAddr:  freeLoopbackAddr(t),
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
		t.Fatalf("rtp.NewDuplexSession: %v", err)
	}

	duplex.Start()
	t.Cleanup(func() { duplex.Stop() })

	return &chunkBoundaryHarness{rec: rec, sess: sess, duplex: duplex, agentSink: agentSink}
}

// --- tests ---------------------------------------------------------------

// TestChunkBoundary_NonAlignedTTSChunksReconstructExactlyOnRealWire drives
// a real Session + DuplexSession pipeline with a fake TTS backend that
// emits three deliberately non-320-byte-aligned chunks (137, 501, 322
// bytes - realistic "not a round number" vendor sizes, summing to an
// exact multiple of 320 so this test's expected wire output contains no
// silence-padded partial frame at all, keeping the comparison clean) and
// confirms every single one of the resulting 480 samples that actually
// lands on the real RTP wire matches, exactly, what the same 480 original
// samples must decode back to after a real G.711 mu-law round trip - i.e.
// no chunk-boundary silently drops, duplicates, reorders, or corrupts any
// audio anywhere in the real Session -> DuplexSession -> ClearStream ->
// RTP path.
func TestChunkBoundary_NonAlignedTTSChunksReconstructExactlyOnRealWire(t *testing.T) {
	script := []scriptedChunk{
		{n: 137, fill: 0x11, isFinal: false},
		{n: 501, fill: 0x83, isFinal: false},
		{n: 322, fill: 0x40, isFinal: true},
	}
	totalBytes := 0
	for _, c := range script {
		totalBytes += c.n
	}
	if totalBytes%wireFrameBytes != 0 {
		t.Fatalf("test setup bug: totalBytes=%d must be an exact multiple of wireFrameBytes=%d for this test's clean-boundary assumption", totalBytes, wireFrameBytes)
	}

	h := newChunkBoundaryHarness(t, &scriptedSynthesizer{script: script})

	h.rec.callerStream().emitFinal("namaste")

	wantPackets := totalBytes / wireFrameBytes
	pkts := recvRTPPackets(t, h.agentSink, wantPackets, 5*time.Second)

	// Reassemble the actual wire payload, in sequence-number order, and
	// decode every G.711 byte back to a linear sample.
	var actualPayload []byte
	for _, p := range pkts {
		if len(p.payload) != wireFrameBytes/2 {
			t.Errorf("packet seq=%d payload length = %d bytes, want exactly %d (one G.711 byte per sample, %d samples/frame)", p.seq, len(p.payload), wireFrameBytes/2, wireFrameBytes/2)
		}
		actualPayload = append(actualPayload, p.payload...)
	}
	actual := make([]int16, len(actualPayload))
	for i, b := range actualPayload {
		actual[i] = testMulawToLinear(b)
	}

	// Build the original PCM byte stream exactly as the accumulator must
	// deliver it to InjectBotAudio: the straightforward concatenation of
	// every scripted chunk's bytes, in script order - regardless of how
	// feedTTSPacer internally regroups them across 320-byte boundaries.
	var original []byte
	for _, c := range script {
		for i := 0; i < c.n; i++ {
			original = append(original, c.fill)
		}
	}
	if len(original) != totalBytes {
		t.Fatalf("test setup bug: len(original)=%d, want %d", len(original), totalBytes)
	}
	wantSamples := len(original) / 2
	expected := make([]int16, wantSamples)
	for i := 0; i < wantSamples; i++ {
		raw := int16(binary.LittleEndian.Uint16(original[i*2 : i*2+2]))
		expected[i] = testQuantize(raw)
	}

	if len(actual) != len(expected) {
		t.Fatalf("reconstructed %d samples from the real RTP wire, want exactly %d - audio was dropped or extra audio was fabricated at a chunk boundary", len(actual), len(expected))
	}
	for i := range expected {
		if actual[i] != expected[i] {
			t.Fatalf("sample %d = %d, want %d (real wire audio diverges from the original scripted TTS PCM at this sample - a chunk-boundary corruption/reorder bug)", i, actual[i], expected[i])
		}
	}
}

// TestChunkBoundary_GenuinePartialFinalFrameFlushedNotLeaked is
// TestChunkBoundary_NonAlignedTTSChunksReconstructExactlyOnRealWire's
// sibling for the case where the TTS utterance's *true* total length is
// NOT an exact multiple of wireFrameBytes: 137 + 501 = 638 bytes = one
// full 320-byte frame plus a genuine 318-byte (159-sample) partial
// remainder. This is exactly the case feedTTSPacer's accumulator must
// flush -- unaligned, exactly once -- the moment chunk.IsFinal is seen
// (see duplex.go's feedTTSPacer doc comment), for ClearStream's own
// InjectBotAudio to silence-pad up to a full frame precisely the way it
// always has for a genuine trailing partial frame.
//
// This proves both halves of the fix's remainder-handling contract: (a)
// the 159 real leftover samples are not lost/leaked forever (a bug would
// most likely manifest as only 1 packet ever arriving, with the second
// packet's real audio never flushed) - checked by requiring *exactly* 2
// packets, matching the sample content precisely, and then explicitly
// confirming no further packets ever arrive; and (b) the pad added to
// reach a full frame is genuine silence (decodes to exactly 0), not
// leftover/stale data from a previous utterance or a corrupted encode.
func TestChunkBoundary_GenuinePartialFinalFrameFlushedNotLeaked(t *testing.T) {
	script := []scriptedChunk{
		{n: 137, fill: 0x22, isFinal: false},
		{n: 501, fill: 0x99, isFinal: true},
	}
	totalBytes := 0
	for _, c := range script {
		totalBytes += c.n
	}
	const remainderBytes = 638 % wireFrameBytes
	if totalBytes != 638 || remainderBytes == 0 || remainderBytes%2 != 0 {
		t.Fatalf("test setup bug: totalBytes=%d must be 638 with a non-zero, even remainder mod wireFrameBytes for this test's assumptions", totalBytes)
	}

	h := newChunkBoundaryHarness(t, &scriptedSynthesizer{script: script})

	h.rec.callerStream().emitFinal("namaste")

	// One full aligned frame, plus one final frame carrying the genuine
	// partial remainder (silence-padded by ClearStream's InjectBotAudio,
	// exactly as it always has for a true trailing partial frame) -
	// never more, never fewer.
	const wantPackets = 2
	pkts := recvRTPPackets(t, h.agentSink, wantPackets, 5*time.Second)

	var original []byte
	for _, c := range script {
		for i := 0; i < c.n; i++ {
			original = append(original, c.fill)
		}
	}

	// First packet: the first wireFrameBytes bytes of original,
	// unaligned across the two scripted chunks (137 bytes of 0x22 + 183
	// bytes of 0x99).
	firstFrame := original[:wireFrameBytes]
	// Second packet: the true 318-byte/159-sample remainder, followed by
	// exactly one silence-padded sample (zero) to reach a full
	// wireFrameBytes/2-sample frame.
	remainder := original[wireFrameBytes:]
	if len(remainder) != remainderBytes {
		t.Fatalf("test setup bug: len(remainder)=%d, want %d", len(remainder), remainderBytes)
	}

	checkFrame := func(label string, pkt recvRTPPacket, wantSourceBytes []byte, padSamples int) {
		t.Helper()
		if len(pkt.payload) != wireFrameBytes/2 {
			t.Fatalf("%s: payload length = %d bytes, want exactly %d", label, len(pkt.payload), wireFrameBytes/2)
		}
		actual := make([]int16, len(pkt.payload))
		for i, b := range pkt.payload {
			actual[i] = testMulawToLinear(b)
		}
		wantRealSamples := len(wantSourceBytes) / 2
		for i := 0; i < wantRealSamples; i++ {
			raw := int16(binary.LittleEndian.Uint16(wantSourceBytes[i*2 : i*2+2]))
			want := testQuantize(raw)
			if actual[i] != want {
				t.Errorf("%s: sample %d = %d, want %d", label, i, actual[i], want)
			}
		}
		for i := wantRealSamples; i < wantRealSamples+padSamples; i++ {
			if actual[i] != 0 {
				t.Errorf("%s: padded sample %d = %d, want 0 (genuine silence pad, not stale/corrupted data)", label, i, actual[i])
			}
		}
	}

	checkFrame("packet[0] (full aligned frame)", pkts[0], firstFrame, 0)
	checkFrame("packet[1] (final partial-frame flush)", pkts[1], remainder, (wireFrameBytes/2)-(len(remainder)/2))

	// Confirm the flush happened exactly once: no further packets ever
	// arrive, proving the accumulator's carry was correctly reset (not
	// re-flushed, duplicated, or left to dribble out more stray audio
	// later).
	assertNoMoreRTPPackets(t, h.agentSink, 500*time.Millisecond)
}
