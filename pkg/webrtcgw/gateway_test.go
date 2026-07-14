// gateway_test.go is the closest thing this package has to the
// fresh-clone rebuild sanity check other subsystems in this repo run
// after a push: it drives two real, independent pion/webrtc
// PeerConnections (standing in for two real browsers -- everything about
// the protocol and media path is real; only the browser's own JS engine
// and actual microphone hardware are out of scope for a Go test) all the
// way through this package's actual HTTP server (SignalingHandler +
// StaticHandler), proving the whole gateway works end to end: WebSocket
// signaling, SDP offer/answer, trickle ICE, real PCMA RTP both
// directions, and a real (mock-backed) langstream.Session translating
// between them.
package webrtcgw

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// roomCounter hands out unique room IDs across this file's tests.
// Necessary because a room's Manager-side cleanup (Manager.leave,
// triggered asynchronously from OnConnectionStateChange once both peers
// disconnect) is not guaranteed to have completed by the time a *later*
// call reuses the same literal room-ID string -- reusing a fixed string
// like "room-1" caused a real, observed flake under `go test -count=N`
// (repeated runs within the same process): a later iteration's Join
// could race the previous iteration's still-in-flight cleanup and land
// in stale room state. See DEVLOG.md's 2026-07-14 entry.
var roomCounter atomic.Int64

func uniqueRoom(base string) string {
	return fmt.Sprintf("%s-%d", base, roomCounter.Add(1))
}

// mockSessionFactory builds a langstream.Session against the repo's
// existing deterministic mock MT/TTS backends (translate.MockTranslator,
// tts.MockSynthesizer -- the same "protocol-accurate fakes, not live
// vendors" convention every other integration test in this repo follows,
// see ROADMAP.md's Week 2 decision) plus a test-local finalizingRecognizer
// in place of asr.MockRecognizer.
//
// Why not asr.MockRecognizer directly: its own doc comment is explicit
// that it only ever emits a final transcript from Close() ("Close() is
// what flushes it as a final transcript") -- deliberate Week 1 scaffolding
// for one-shot demos/tests, not a bug. langstream.Session.runLeg only
// translates/synthesizes *final* transcripts (see its own "Week 1 scope"
// comment), so a live, ongoing WebRTC call -- which must never need to
// end the whole Session just to get one utterance translated -- needs an
// ASR double that finalizes mid-stream the way a real vendor's VAD
// actually does (Sarvam, Deepgram). finalizingRecognizer below is exactly
// that: still fully deterministic and offline, just closer to how a real
// streaming ASR backend behaves for this test's purposes.
func mockSessionFactory(ctx context.Context) (*langstream.Session, error) {
	return langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            &finalizingRecognizer{},
		Translator:     translate.NewMockTranslator(),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	})
}

// finalizingRecognizer is a minimal, deterministic asr.Recognizer for
// this package's own tests: once at least finalizeAfterBytes of PCM have
// been pushed to one of its StreamSessions, it emits exactly one final
// Transcript (a canned phrase, mirroring asr.MockRecognizer's own
// mockPhrases) and then goes quiet -- simulating one real utterance
// finalizing mid-call, not requiring the whole Session to close first.
type finalizingRecognizer struct{}

func (r *finalizingRecognizer) Name() string { return "test-finalizing" }

func (r *finalizingRecognizer) SupportedLanguages() []asr.Language {
	return []asr.Language{"hi", "en"}
}

func (r *finalizingRecognizer) StartStream(ctx context.Context, languageHint asr.Language) (asr.StreamSession, error) {
	lang := languageHint
	if lang == "" {
		lang = "hi"
	}
	sessCtx, cancel := context.WithCancel(ctx)
	return &finalizingStreamSession{lang: lang, ctx: sessCtx, cancel: cancel, out: make(chan asr.Transcript, 4)}, nil
}

// finalizeAfterBytes mirrors asr.MockRecognizer's mockFlushBytes (roughly
// 500ms @ 8kHz/16-bit mono): enough that a real test "utterance" (see
// testBrowser.speak) reliably crosses it well within the frame counts
// this package's tests already use.
const finalizeAfterBytes = 8000

type finalizingStreamSession struct {
	mu       sync.Mutex
	lang     asr.Language
	buffered int
	emitted  bool
	closed   bool

	ctx    context.Context
	cancel context.CancelFunc
	out    chan asr.Transcript
}

func (s *finalizingStreamSession) PushAudio(ctx context.Context, frame asr.AudioFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.buffered += len(frame.PCM)
	if !s.emitted && s.buffered >= finalizeAfterBytes {
		s.emitted = true
		phrase := "नमस्ते, यह एक परीक्षण कॉल है"
		if s.lang == "en" {
			phrase = "hello, this is a test call"
		}
		select {
		case s.out <- asr.Transcript{Text: phrase, Language: s.lang, IsFinal: true, Confidence: 1}:
		case <-ctx.Done():
		}
	}
	return nil
}

func (s *finalizingStreamSession) Transcripts() <-chan asr.Transcript { return s.out }

func (s *finalizingStreamSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	close(s.out)
	return nil
}

var _ asr.Recognizer = (*finalizingRecognizer)(nil)
var _ asr.StreamSession = (*finalizingStreamSession)(nil)

// testBrowser is a headless stand-in for one real browser participant:
// its own PeerConnection (PCMA-only, matching newMediaEngine exactly, the
// same way a real browser negotiates down to whatever this gateway's
// answer restricts it to), a WebSocket connection to the gateway's /ws
// signaling endpoint, and a channel of decoded PCM16 audio it received on
// its remote track (the "translated audio the user would hear").
type testBrowser struct {
	t    *testing.T
	pc   *webrtc.PeerConnection
	ws   *websocket.Conn
	send *webrtc.TrackLocalStaticSample

	// writeMu serializes writes to ws: gorilla/websocket connections are
	// not safe for concurrent writers, and this test client has two
	// independent sources of outbound messages (the OnICECandidate
	// callback, invoked from pion's own internal ICE-agent goroutine, and
	// the main test goroutine sending "join"/"offer") that can race
	// without this -- caught live by `go test -race` on the first real
	// run of this test (see DEVLOG.md's 2026-07-14 entry).
	writeMu sync.Mutex

	receivedPCM chan []byte
}

func (b *testBrowser) writeJSON(v wireMessage) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.ws.WriteJSON(v)
}

func newTestBrowser(t *testing.T, wsURL, room, role string) *testBrowser {
	t.Helper()

	mediaEngine, err := newMediaEngine()
	if err != nil {
		t.Fatalf("newMediaEngine: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: sampleRate, Channels: 1},
		"audio", "test-"+role,
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	b := &testBrowser{t: t, pc: pc, send: track, receivedPCM: make(chan []byte, 256)}

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				if len(packet.Payload) == 0 {
					continue
				}
				select {
				case b.receivedPCM <- alawToPCM16(packet.Payload):
				default:
				}
			}
		}()
	})

	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dialing signaling websocket: %v", err)
	}
	b.ws = wsConn

	// Trickle our own ICE candidates to the gateway.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand := c.ToJSON()
		_ = b.writeJSON(wireMessage{Type: "candidate", Candidate: &cand})
	})

	if err := b.writeJSON(wireMessage{Type: "join", Room: room, Role: role}); err != nil {
		t.Fatalf("sending join: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	if err := b.writeJSON(wireMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		t.Fatalf("sending offer: %v", err)
	}

	// Read the answer (and apply any candidates that arrive first/interleaved)
	// in a background goroutine for the rest of the test's lifetime.
	go func() {
		for {
			var msg wireMessage
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "answer":
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}); err != nil {
					t.Errorf("%s: SetRemoteDescription(answer): %v", role, err)
				}
			case "candidate":
				if msg.Candidate != nil {
					_ = pc.AddICECandidate(*msg.Candidate)
				}
			case "error":
				t.Errorf("%s: gateway reported error: %s", role, msg.Message)
			}
		}
	}()

	return b
}

// waitConnected blocks until this browser's PeerConnection reaches the
// "connected" ICE state, or fails the test after timeout.
func (b *testBrowser) waitConnected(timeout time.Duration) {
	b.t.Helper()
	deadline := time.After(timeout)
	for {
		state := b.pc.ICEConnectionState()
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			b.t.Fatalf("ICE connection not established within %s (last state: %s)", timeout, state)
		}
	}
}

// speak sends n 20ms frames of the given tone (or silence if amplitude is
// 0) as PCMA RTP samples, paced in real time so the gateway's mock ASR
// sees a realistic stream (not a burst) -- exactly what a real mic would
// produce, just synthetic instead of a real recording.
func (b *testBrowser) speak(n int, amplitude int16) {
	pcm := make([]byte, 320) // 160 samples, 20ms @ 8kHz, 16-bit mono
	for frame := 0; frame < n; frame++ {
		for i := 0; i < 160; i++ {
			v := amplitude
			if amplitude != 0 && i%2 == 1 {
				v = -amplitude
			}
			pcm[i*2] = byte(v)
			pcm[i*2+1] = byte(v >> 8)
		}
		alaw := pcm16ToALaw(pcm)
		_ = b.send.WriteSample(media.Sample{Data: alaw, Duration: 20 * time.Millisecond})
		time.Sleep(20 * time.Millisecond)
	}
}

func (b *testBrowser) close() {
	_ = b.ws.Close()
	_ = b.pc.Close()
}

// TestGateway_TwoBrowsers_EndToEndTranslation is the real end-to-end
// proof: two independent PeerConnections join the same room over this
// package's actual HTTP server, both reach a connected ICE state, the
// caller "speaks" (enough synthetic audio to cross MockRecognizer's
// mockFlushBytes threshold), and the translated (mock) audio is asserted
// to actually arrive on the agent's remote track -- and the same in the
// other direction.
func TestGateway_TwoBrowsers_EndToEndTranslation(t *testing.T) {
	mgr := NewManager(mockSessionFactory)
	mux := http.NewServeMux()
	mux.Handle("/ws", SignalingHandler(mgr, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	room := uniqueRoom("room")

	caller := newTestBrowser(t, wsURL, room, "caller")
	defer caller.close()
	agent := newTestBrowser(t, wsURL, room, "agent")
	defer agent.close()

	caller.waitConnected(10 * time.Second)
	agent.waitConnected(10 * time.Second)

	// Caller speaks (enough frames to cross finalizeAfterBytes: 8000 bytes
	// / 320 bytes-per-frame = 25 frames minimum. 80 frames gives generous
	// slack -- comfortably more than double -- so a few dropped/delayed
	// packets during ICE/DTLS setup under real (sometimes loaded) CI
	// conditions can't flake this test by leaving the total just short of
	// the threshold; this margin was tightened after an observed flake
	// under sandbox load, see DEVLOG.md's 2026-07-14 entry).
	go caller.speak(80, 8000)

	select {
	case pcm := <-agent.receivedPCM:
		if len(pcm) == 0 {
			t.Error("agent received an empty translated audio payload")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent never received translated audio from the caller's speech")
	}

	// And the other direction: agent speaks, caller should hear the
	// translation. Same generous margin as above.
	go agent.speak(80, 6000)

	select {
	case pcm := <-caller.receivedPCM:
		if len(pcm) == 0 {
			t.Error("caller received an empty translated audio payload")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller never received translated audio from the agent's speech")
	}

	if got := mgr.RoomCount(); got != 1 {
		t.Errorf("RoomCount() = %d, want 1 while both peers are still connected", got)
	}
}

// TestGateway_RoleAlreadyTaken verifies a room enforces exactly one
// "caller" and one "agent" -- a second participant trying to join the
// same room with a role that's already occupied gets a clear error over
// the signaling channel, not a silent takeover or a crash.
func TestGateway_RoleAlreadyTaken(t *testing.T) {
	mgr := NewManager(mockSessionFactory)
	mux := http.NewServeMux()
	mux.Handle("/ws", SignalingHandler(mgr, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	room := uniqueRoom("room")

	first := newTestBrowser(t, wsURL, room, "caller")
	defer first.close()
	first.waitConnected(10 * time.Second)

	// A second "caller" in the same room should be rejected; dial our own
	// raw websocket here (rather than the full testBrowser helper) since
	// we expect an error message, not a successful offer/answer.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(wireMessage{Type: "join", Room: room, Role: "caller"}); err != nil {
		t.Fatalf("sending join: %v", err)
	}

	var msg wireMessage
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if msg.Type != "error" {
		t.Errorf("Type = %q, want \"error\" for a duplicate role join", msg.Type)
	}
}

// TestManager_RoomCleanupOnBothPeersLeaving verifies a room's Session is
// actually torn down (and the room removed from the Manager) once both
// participants disconnect -- a room with nobody in it must not leak a
// live ASR/MT/TTS session forever.
func TestManager_RoomCleanupOnBothPeersLeaving(t *testing.T) {
	mgr := NewManager(mockSessionFactory)
	mux := http.NewServeMux()
	mux.Handle("/ws", SignalingHandler(mgr, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	room := uniqueRoom("room")

	caller := newTestBrowser(t, wsURL, room, "caller")
	agent := newTestBrowser(t, wsURL, room, "agent")
	caller.waitConnected(10 * time.Second)
	agent.waitConnected(10 * time.Second)

	if got := mgr.RoomCount(); got != 1 {
		t.Fatalf("RoomCount() = %d, want 1 once both peers have joined", got)
	}

	caller.close()
	agent.close()

	deadline := time.After(5 * time.Second)
	for {
		if mgr.RoomCount() == 0 {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("RoomCount() never reached 0 after both peers disconnected (still %d)", mgr.RoomCount())
		}
	}
}
