// Package langstream_test - vendor round-trip integration tests.
//
// FAKE-SERVER ROUND TRIP, NOT A LIVE-VENDOR TEST. Every test in this file
// drives the *real* vendor client code PE landed today
// (asr.NewSarvamRecognizer, translate.NewGPT4oTranslator,
// tts.NewCartesiaSynthesizer) against small in-process fake HTTP/WebSocket
// servers standing in for Sarvam, GPT-4o, and Cartesia, wired into a real
// langstream.Session exactly the way cmd/langstream/main.go's runDemo does
// via the backend registry. No network call in this file ever leaves the
// test process, and none of it exercises the actual sarvam.ai/openai.com/
// cartesia.ai endpoints. That's intentional and matches ROADMAP.md's Week 2
// decision (2026-07-07, see ROADMAP.md "Week 2 - Real Pipeline"): no vendor
// API keys exist yet, so the pilot proceeds on fake-server tests that prove
// the client code itself is correct, deferring a real-network smoke test
// until keys are available. This file's first test
// (TestVendorRoundTrip_HindiToEnglish_FakeServers) is QA's implementation
// of that Week 2 checklist item, "First real Hindi<->English round-trip on
// recorded test audio" - "recorded test audio" here means the fixed
// synthetic PCM frame pushed below, standing in for a recorded caller
// utterance, since no real microphone/telephony input exists in CI.
//
// Sarvam (not Deepgram) is used for the ASR leg specifically because
// langstream.SessionConfig.ASR is a single Recognizer shared by both the
// caller leg (language hint "hi") and the agent leg (language hint "en");
// Sarvam's SupportedLanguages() advertises both "hi" and "en" (its whole
// purpose is Hindi-English code-switching), while Deepgram only advertises
// "en" and would fail StartStream(ctx, "hi") for the caller leg. This
// mirrors a real deployment decision, not a test-only shortcut.
package langstream_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// --- fake vendor servers -------------------------------------------------
//
// These are deliberately independent of (not a reuse of) each vendor
// package's own _test.go fake-server helpers: those helpers are
// unexported and package-scoped (pkg/asr, pkg/translate, pkg/tts), and
// this file lives in the external langstream_test package at repo root
// per QA's charter (see langstream_integration_test.go), so it cannot
// import them. The wire shapes below are deliberately kept as small
// literal JSON as possible - just enough to satisfy each real client's
// parser - rather than re-deriving the vendor packages' full unexported
// request/response structs.

// newFakeSarvamASRServer starts a local WebSocket server standing in for
// Sarvam's streaming speech-to-text endpoint. Every connection follows the
// same script: wait for the client's first audio message, then reply with
// one final ({"type":"data",...}) transcript carrying transcriptText -
// mirroring pkg/asr/sarvam_test.go's newFakeSarvamServer/
// TestSarvamRecognizer_SendsAudioAndParsesTranscript pattern, but written
// locally here since that helper is unexported in package asr.
func newFakeSarvamASRServer(t *testing.T, transcriptText string) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		result, _ := json.Marshal(map[string]interface{}{
			"type": "data",
			"data": map[string]interface{}{
				"request_id": "vendor-roundtrip-1",
				"transcript": transcriptText,
				"metrics":    map[string]float64{"audio_duration": 1.0, "processing_latency": 0.05},
			},
		})
		if err := conn.WriteMessage(websocket.TextMessage, result); err != nil {
			return
		}

		// Drain further messages (flush signals, more audio from a leg
		// this test never exercises) until the connection closes.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"
	return srv, wsURL
}

// newFakeSarvamErrorServer starts a fake Sarvam server that, after reading
// one audio message, replies with an error-shaped response
// ({"type":"error",...}), which the real SarvamRecognizer treats as fatal
// (see pkg/asr/sarvam_test.go's TestSarvamRecognizer_VendorErrorClosesSession).
// Used by the adversarial test below to prove a mid-stream vendor error
// doesn't hang or panic the orchestrator.
func newFakeSarvamErrorServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		errMsg, _ := json.Marshal(map[string]string{
			"type":  "error",
			"error": "invalid subscription key",
		})
		if err := conn.WriteMessage(websocket.TextMessage, errMsg); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"
	return srv, wsURL
}

// newFakeGPT4oServer starts a local HTTP server standing in for OpenAI's
// POST /chat/completions (streaming) endpoint: it always replies with one
// SSE chunk carrying translatedText followed by the "[DONE]" sentinel,
// regardless of the request body, which is enough for
// translate.GPT4oTranslator's SSE parser (see pkg/translate/gpt4o.go's
// readStream) to assemble it into a single Chunk.
func newFakeGPT4oServer(t *testing.T, translatedText string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		payload, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": translatedText}},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// newFakeCartesiaTTSServer starts a local WebSocket server standing in for
// Cartesia's /tts/websocket endpoint: it reads the client's one JSON
// generation request, then streams back pcmChunks as
// {"type":"chunk","data":"<base64>","done":bool,"context_id":...} messages,
// marking only the last chunk done:true. Since
// tts.CartesiaSynthesizer implements the WebSocket wire protocol itself
// (see pkg/tts/cartesia_ws.go) rather than depending on gorilla/websocket,
// and RFC 6455 is a real interoperable protocol, a gorilla/websocket
// *server* here talks to it just fine - this is simpler than replicating
// pkg/tts/cartesia_test.go's raw-hijack fake server, which exists there
// only because pkg/tts itself has zero external dependencies and can't use
// gorilla/websocket even in its own tests.
func newFakeCartesiaTTSServer(t *testing.T, pcmChunks ...[]byte) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ContextID string `json:"context_id"`
		}
		_ = json.Unmarshal(payload, &req)

		for i, pcm := range pcmChunks {
			msg, _ := json.Marshal(map[string]interface{}{
				"type":       "chunk",
				"data":       base64.StdEncoding.EncodeToString(pcm),
				"done":       i == len(pcmChunks)-1,
				"context_id": req.ContextID,
			})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	})
	return httptest.NewServer(mux)
}

// newFakeCartesiaMalformedServer starts a fake Cartesia server that sends
// one good chunk and then a chunk whose "data" field is not valid base64 -
// mirroring pkg/tts/cartesia_test.go's
// TestCartesiaSynthesizer_MalformedChunkClosesChannel - to drive the
// adversarial TTS-degrades-gracefully test below.
func newFakeCartesiaMalformedServer(t *testing.T, goodPCM []byte) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ContextID string `json:"context_id"`
		}
		_ = json.Unmarshal(payload, &req)

		good, _ := json.Marshal(map[string]interface{}{
			"type": "chunk", "data": base64.StdEncoding.EncodeToString(goodPCM),
			"done": false, "context_id": req.ContextID,
		})
		if err := conn.WriteMessage(websocket.TextMessage, good); err != nil {
			return
		}
		bad, _ := json.Marshal(map[string]interface{}{
			"type": "chunk", "data": "not-valid-base64!!",
			"done": false, "context_id": req.ContextID,
		})
		_ = conn.WriteMessage(websocket.TextMessage, bad)
		// Deliberately never sends a done/final message: the real
		// CartesiaSynthesizer.readLoop must give up on the malformed
		// frame rather than hang waiting for one.
	})
	return httptest.NewServer(mux)
}

// wsURL converts an httptest.Server's http:// URL to a ws:// base URL
// suitable for asr/tts's WithBaseURL-style options.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// newVendorSession builds a langstream.Session out of real Sarvam/GPT-4o/
// Cartesia clients pointed at the given fake servers, for the pilot's one
// supported language pair (hi caller, en agent) - the same shape
// cmd/langstream/main.go's runDemo builds via the backend registry, just
// constructed directly here since this file's whole point is exercising
// the vendor client code, not the registry (pkg/langstream/backends_test.go
// already covers the registry itself).
func newVendorSession(t *testing.T, ctx context.Context, sarvamURL, gpt4oURL, cartesiaURL string) *langstream.Session {
	t.Helper()

	t.Setenv("SARVAM_API_KEY", "fake-test-key")
	t.Setenv("CARTESIA_API_KEY", "fake-test-key")

	rec, err := asr.NewSarvamRecognizer(asr.WithSarvamBaseURL(sarvamURL))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	tr, err := translate.NewGPT4oTranslator(translate.WithBaseURL(gpt4oURL), translate.WithAPIKey("fake-test-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	syn, err := tts.NewCartesiaSynthesizer(tts.WithBaseURL(cartesiaURL), tts.WithDialTimeout(3*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	})
	if err != nil {
		t.Fatalf("NewSession with real vendor clients against fake servers: %v", err)
	}
	return sess
}

// --- the round trip -------------------------------------------------------

// TestVendorRoundTrip_HindiToEnglish_FakeServers is Week 2's "first real
// Hindi<->English round-trip on recorded test audio" (ROADMAP.md), run
// against fake vendor servers per the Week 2 decision documented at the
// top of this file. It pushes one fixed synthetic PCM frame representing
// caller audio, drives it through the real Sarvam ASR client, the real
// GPT-4o translation client, and the real Cartesia TTS client (each
// talking to a fake local server), orchestrated by the real
// langstream.Session, and asserts the agent leg receives synthesized
// audio - all under a bounded timeout so a stalled fake server or a stuck
// pipeline fails the test instead of hanging CI.
func TestVendorRoundTrip_HindiToEnglish_FakeServers(t *testing.T) {
	const hindiUtterance = "मुझे रिफंड चाहिए"
	const englishTranslation = "I need a refund"

	sarvamSrv, sarvamURL := newFakeSarvamASRServer(t, hindiUtterance)
	defer sarvamSrv.Close()

	gpt4oSrv := newFakeGPT4oServer(t, englishTranslation)
	defer gpt4oSrv.Close()

	pcm1 := []byte{10, 20, 30, 40}
	pcm2 := []byte{50, 60, 70, 80, 90}
	cartesiaSrv := newFakeCartesiaTTSServer(t, pcm1, pcm2)
	defer cartesiaSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess := newVendorSession(t, ctx, sarvamURL, gpt4oSrv.URL, wsURL(cartesiaSrv))
	defer sess.Close()

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	var gotChunks []tts.AudioChunk
	deadline := time.After(8 * time.Second)
drain:
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				break drain
			}
			gotChunks = append(gotChunks, chunk)
			if chunk.IsFinal {
				break drain
			}
		case <-deadline:
			t.Fatal("timed out waiting for the agent leg to receive synthesized audio via the real vendor clients")
		}
	}

	if len(gotChunks) == 0 {
		t.Fatal("agent leg received no synthesized audio through the fake-server-backed real vendor pipeline")
	}
	for i, c := range gotChunks {
		if len(c.PCM) == 0 {
			t.Errorf("chunk[%d] has empty PCM", i)
		}
		if c.SampleRate == 0 {
			t.Errorf("chunk[%d] has zero SampleRate", i)
		}
	}
	if last := gotChunks[len(gotChunks)-1]; !last.IsFinal {
		t.Error("expected the last chunk delivered to the agent leg to be IsFinal")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- adversarial: vendor failures must degrade, not hang or panic --------

// TestVendorRoundTrip_ASRFatalErrorDoesNotHangSession is the adversarial
// counterpart to the happy-path round trip above: the fake Sarvam server
// responds to the caller's audio with a fatal error frame instead of a
// transcript (exactly what pkg/asr/sarvam_test.go's
// TestSarvamRecognizer_VendorErrorClosesSession exercises at the ASR-package
// level, which closes the real sarvamSession's Transcripts() channel on its
// own, mid-call). This test asserts the *orchestrator's* behavior on top of
// that real failure mode, and critically, that Session.Close() returns
// promptly rather than hanging - go test's -race build also makes this a
// background check for data races in the shutdown path when a leg's
// Transcripts() channel closes on error rather than via the normal
// Close()-triggered flush.
//
// Updated 2026-07-20 (QA) for Tech's same-day ASR-permanent-failure leg-
// visibility fix (pkg/langstream/session.go's runLeg + fallback.go's
// reasonASRStreamClosed - see pkg/langstream/session_test.go's
// TestSessionASRStreamPermanentClosureDegradesLegAndForwardsBufferedAudio
// and this repo's root-level asr_permanent_failure_integration_test.go for
// the equivalent coverage against a local fake). Before that fix, this
// exact scenario (a real vendor's StreamSession dying mid-call) made
// runLeg silently drop the buffered caller audio and return with no
// observable signal - which is what this test used to assert as the
// expected behavior ("no audio is ever delivered to the agent leg"). That
// was the bug, not a feature: this test now asserts the *fixed* contract
// instead - the caller leg is marked permanently degraded, and the
// buffered caller audio it already had in flight is forwarded to the
// agent as a passthrough chunk (preceded by the degrade tone, per
// FallbackConfig.DegradeToneEnabled's default) rather than silently
// dropped - while Close() must still never hang.
func TestVendorRoundTrip_ASRFatalErrorDoesNotHangSession(t *testing.T) {
	sarvamSrv, sarvamURL := newFakeSarvamErrorServer(t)
	defer sarvamSrv.Close()

	// GPT-4o/Cartesia fakes exist only so construction succeeds; the ASR
	// leg fails before ever producing a transcript to translate, so
	// neither should be hit.
	gpt4oSrv := newFakeGPT4oServer(t, "should never be requested")
	defer gpt4oSrv.Close()
	cartesiaSrv := newFakeCartesiaTTSServer(t, []byte{1, 2, 3})
	defer cartesiaSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess := newVendorSession(t, ctx, sarvamURL, gpt4oSrv.URL, wsURL(cartesiaSrv))

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio should succeed - the vendor error arrives asynchronously after this frame is sent: %v", err)
	}

	// The buffered caller audio must be forwarded to the agent as a final
	// passthrough chunk once the caller leg's real Sarvam ASR stream dies
	// on the fatal vendor error, instead of being silently dropped.
	var sawOriginal, sawFinal bool
	deadline := time.After(5 * time.Second)
	for !sawFinal {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				t.Fatal("AgentHearsAudio closed unexpectedly before the passthrough chunk arrived")
			}
			if string(chunk.PCM) == string(frame.PCM) {
				sawOriginal = true
			}
			sawFinal = chunk.IsFinal
		case <-deadline:
			t.Fatal("timed out waiting for the buffered caller audio to be forwarded as passthrough after the fatal ASR vendor error")
		}
	}
	if !sawOriginal {
		t.Fatal("expected the buffered original caller audio to be forwarded as a passthrough chunk after the fatal ASR vendor error, but it never appeared on AgentHearsAudio()")
	}
	if !sess.CallerLegDegraded() {
		t.Fatal("expected CallerLegDegraded() to be true after the caller leg's real Sarvam ASR stream died on a fatal vendor error")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- sess.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Session.Close() hung after a fatal ASR vendor error mid-stream")
	}
}

// TestVendorRoundTrip_MalformedTTSFrameDoesNotHangSession is a second
// adversarial cross-package test, this time failing late in the pipeline
// (ASR and MT both succeed for real, against real fake servers) rather
// than at the first hop: the fake Cartesia server sends one good audio
// chunk and then a malformed one (invalid base64), never sending a
// done/final message. tts.CartesiaSynthesizer's readLoop is documented to
// give up and close its channel without ever emitting IsFinal=true rather
// than hang waiting for a final chunk that will never come (see
// pkg/tts/cartesia_test.go's TestCartesiaSynthesizer_MalformedChunkClosesChannel).
// This test proves that contract holds through the *whole* orchestrator:
// Session.forwardAudio must treat that early channel close as "leg done,
// not a hang", the agent leg must receive the one good chunk it was sent
// and no corrupted data, and Close() must still return within a bounded
// timeout.
func TestVendorRoundTrip_MalformedTTSFrameDoesNotHangSession(t *testing.T) {
	const hindiUtterance = "मुझे मदद चाहिए"
	const englishTranslation = "I need help"

	sarvamSrv, sarvamURL := newFakeSarvamASRServer(t, hindiUtterance)
	defer sarvamSrv.Close()

	gpt4oSrv := newFakeGPT4oServer(t, englishTranslation)
	defer gpt4oSrv.Close()

	goodPCM := []byte{7, 7, 7, 7}
	cartesiaSrv := newFakeCartesiaMalformedServer(t, goodPCM)
	defer cartesiaSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess := newVendorSession(t, ctx, sarvamURL, gpt4oSrv.URL, wsURL(cartesiaSrv))

	if err := sess.PushCallerAudio(asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}

	// Unlike the happy-path round trip, AgentHearsAudio() (the
	// Session-lifetime outbound channel) does NOT close here: only
	// Session.Close() closes it. What must happen instead is that the
	// per-request TTS channel closes (readLoop's documented give-up
	// behavior) without ever handing forwardAudio a final chunk, so no
	// more chunks arrive after the one good one. Detect that as a quiet
	// period rather than a channel close, bounded overall so a real hang
	// still fails the test instead of blocking CI.
	var gotChunks []tts.AudioChunk
	overallDeadline := time.After(8 * time.Second)
	quiet := time.NewTimer(2 * time.Second)
	defer quiet.Stop()
drain:
	for {
		select {
		case chunk, ok := <-sess.AgentHearsAudio():
			if !ok {
				break drain
			}
			gotChunks = append(gotChunks, chunk)
			if !quiet.Stop() {
				<-quiet.C
			}
			quiet.Reset(2 * time.Second)
		case <-quiet.C:
			break drain
		case <-overallDeadline:
			t.Fatal("no quiet period observed on AgentHearsAudio() within the overall deadline - the malformed TTS frame may have hung the pipeline")
		}
	}

	if len(gotChunks) != 1 {
		t.Fatalf("got %d chunks, want exactly 1 (the good chunk sent before the malformed one); channel should have closed without a final chunk, not delivered corrupted/extra data", len(gotChunks))
	}
	if gotChunks[0].IsFinal {
		t.Error("the only delivered chunk has IsFinal=true, but the stream ended abnormally (malformed frame, no done message) - this should never be marked final")
	}
	if string(gotChunks[0].PCM) != string(goodPCM) {
		t.Errorf("delivered chunk PCM = %v, want the good chunk %v unmodified", gotChunks[0].PCM, goodPCM)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- sess.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Session.Close() hung after a malformed mid-stream TTS frame")
	}
}
