package asr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/observability"
	"github.com/gorilla/websocket"
)

// newFakeDeepgramServer starts an httptest server that upgrades to a
// WebSocket and drives the given handler for each connection. It returns
// the server and its ws:// base URL (with a trailing path matching
// Deepgram's "/v1/listen" so query-string handling in deepgram.go is
// exercised the same way it would be against the real endpoint).
func newFakeDeepgramServer(t *testing.T, handler func(t *testing.T, conn *websocket.Conn)) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake deepgram server: upgrade: %v", err)
			return
		}
		defer conn.Close()
		handler(t, conn)
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"
	return srv, wsURL
}

func TestDeepgramRecognizer_MissingAPIKey(t *testing.T) {
	old, had := os.LookupEnv("DEEPGRAM_API_KEY")
	os.Unsetenv("DEEPGRAM_API_KEY")
	defer func() {
		if had {
			os.Setenv("DEEPGRAM_API_KEY", old)
		}
	}()

	if _, err := NewDeepgramRecognizer(); err == nil {
		t.Fatal("expected error when DEEPGRAM_API_KEY is unset, got nil")
	}
}

// TestDeepgramRecognizer_SupportedLanguagesAndUnsupportedHint verifies
// both "en" and "hi" are advertised (see DeepgramRecognizer's doc comment
// for why "hi" now routes to Nova-3/language=multi rather than being
// rejected outright, unlike this backend's original English-only pilot
// scope), and that a genuinely unsupported hint (neither "en" nor "hi")
// is still rejected.
func TestDeepgramRecognizer_SupportedLanguagesAndUnsupportedHint(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "test-key")
	r, err := NewDeepgramRecognizer()
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	langs := r.SupportedLanguages()
	want := map[Language]bool{"en": false, "hi": false}
	for _, l := range langs {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, found := range want {
		if !found {
			t.Errorf("expected SupportedLanguages() to include %q, got %v", l, langs)
		}
	}
	if len(langs) != 2 {
		t.Errorf("expected exactly [en hi] (in some order), got %v", langs)
	}

	if _, err := r.StartStream(context.Background(), "fr"); err == nil {
		t.Fatal("expected error starting stream with a genuinely unsupported language hint (\"fr\")")
	}
}

// TestDeepgramRecognizer_SendsAudioAndParsesTranscript verifies the client
// authenticates correctly, sends pushed PCM as a binary frame, and parses a
// Deepgram-shaped "Results" JSON message back into a Transcript.
func TestDeepgramRecognizer_SendsAudioAndParsesTranscript(t *testing.T) {
	receivedAuth := make(chan string, 1)
	receivedAudio := make(chan []byte, 1)

	srv, wsURL := newFakeDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("fake server: ReadMessage: %v", err)
			return
		}
		if mt != websocket.BinaryMessage {
			t.Errorf("expected first client message to be binary audio, got type %d", mt)
		}
		receivedAudio <- data

		result := `{
			"type": "Results",
			"is_final": true,
			"speech_final": true,
			"start": 0.0,
			"duration": 1.5,
			"channel": {"alternatives": [{"transcript": "hello world", "confidence": 0.97}]}
		}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(result)); err != nil {
			t.Errorf("fake server: write result: %v", err)
		}

		// Drain any further messages (e.g. KeepAlive) until the client closes.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	// Capture the auth header via a thin wrapping RoundTripper-less trick:
	// gorilla's dialer doesn't expose the request post hoc, so instead we
	// assert on the header by re-deriving it from the recognizer's own
	// construction (unit-level check) and rely on the round trip succeeding
	// (a bad/missing Authorization header would still connect against this
	// permissive fake server, so this test's real job is audio + transcript
	// framing, not server-side auth enforcement).
	close(receivedAuth)

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	pcm := make([]byte, 320) // 160 samples @ 16-bit
	frame := AudioFrame{PCM: pcm, SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case got := <-receivedAudio:
		if len(got) != len(pcm) {
			t.Errorf("server received %d bytes of audio, want %d", len(got), len(pcm))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake server to receive audio")
	}

	select {
	case tr := <-sess.Transcripts():
		if tr.Text != "hello world" {
			t.Errorf("Text = %q, want %q", tr.Text, "hello world")
		}
		if !tr.IsFinal {
			t.Error("expected IsFinal=true")
		}
		if tr.Language != "en" {
			t.Errorf("Language = %q, want en", tr.Language)
		}
		if tr.EndMS != 1500 {
			t.Errorf("EndMS = %d, want 1500", tr.EndMS)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for transcript")
	}
}

// TestDeepgramRecognizer_VendorErrorClosesSession verifies that a Deepgram
// error frame ({"err_code","err_msg"}) is treated as fatal: the session
// closes and PushAudio returns a real error afterwards.
func TestDeepgramRecognizer_VendorErrorClosesSession(t *testing.T) {
	srv, wsURL := newFakeDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		errMsg, _ := json.Marshal(map[string]string{
			"err_code": "INVALID_AUTH",
			"err_msg":  "Invalid credentials.",
		})
		_ = conn.WriteMessage(websocket.TextMessage, errMsg)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "bad-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("first PushAudio should succeed before the error arrives: %v", err)
	}

	// The channel must close once the fatal error is processed.
	closedInTime := false
	deadline := time.After(2 * time.Second)
	for !closedInTime {
		select {
		case _, ok := <-sess.Transcripts():
			if !ok {
				closedInTime = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for Transcripts channel to close after vendor error")
		}
	}

	// A subsequent PushAudio must return a real error, not panic or block.
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail after a fatal vendor error closed the session")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close after failure should be a no-op returning nil, got: %v", err)
	}
}

// TestDeepgramRecognizer_ConnectFailureSurfacesError verifies that when the
// vendor endpoint is unreachable, PushAudio returns a real error rather than
// panicking or hanging.
func TestDeepgramRecognizer_ConnectFailureSurfacesError(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	// Port 0 combined with an already-closed listener guarantees a refused
	// connection: start and immediately close a server to grab a URL that
	// nothing is listening on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"
	srv.Close()

	r, err := NewDeepgramRecognizer(WithBaseURL(deadURL), WithMaxReconnectAttempts(1))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	done := make(chan error, 1)
	go func() { done <- sess.PushAudio(context.Background(), frame) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected PushAudio to fail against an unreachable endpoint")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PushAudio hung instead of returning a connect error")
	}
}

// TestDeepgramRecognizer_RecordsCostPerAudioMinute verifies that a
// successfully pushed audio frame attributes cost to the "deepgram"
// vendor via WithMetrics, proportional to the frame's duration and
// deepgramCostPerMinuteUSD.
func TestDeepgramRecognizer_RecordsCostPerAudioMinute(t *testing.T) {
	srv, wsURL := newFakeDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	// 320 bytes @ 8kHz/16-bit mono = 160 samples = 20ms of audio.
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	wantMinutes := 0.02 / 60.0
	want := wantMinutes * deepgramCostPerMinuteUSD
	got := metrics.CostTotal("deepgram")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(deepgram) = %v, want %v", got, want)
	}
	if n := metrics.CostEventCount("deepgram"); n != 1 {
		t.Errorf("CostEventCount(deepgram) = %d, want 1", n)
	}
}

// TestDeepgramRecognizer_NoMetricsConfiguredNoOp verifies PushAudio never
// panics when no metrics recorder was configured (the common case for
// existing callers that predate WithMetrics).
func TestDeepgramRecognizer_NoMetricsConfiguredNoOp(t *testing.T) {
	srv, wsURL := newFakeDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}
}

// --- Circuit breaker wiring: DeepgramRecognizer ---------------------------

// newRefusingDeepgramServer starts an httptest server that never upgrades
// the WebSocket handshake -- every connection attempt gets a plain 500 --
// so every ensureConnected dial against it fails. attempts counts how many
// times the handler ran (i.e. how many dial attempts were actually made).
func newRefusingDeepgramServer(t *testing.T) (*httptest.Server, string, *int32) {
	t.Helper()
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"
	return srv, wsURL, &attempts
}

// TestDeepgramRecognizer_CircuitBreaker_TripsAndFailsFast covers (a) and
// (b) from the ASR circuit-breaker task: N consecutive sessions that each
// fail to ever establish their initial connection trip the breaker, and
// the next StartStream call after that is rejected immediately with an
// error wrapping ErrCircuitOpen, making zero dial attempts.
func TestDeepgramRecognizer_CircuitBreaker_TripsAndFailsFast(t *testing.T) {
	srv, wsURL, attempts := newRefusingDeepgramServer(t)
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	const threshold = 2
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(1),
		WithCircuitBreaker(threshold, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	// Each of these sessions fails to ever connect: StartStream itself
	// must succeed (breaker still closed), but the first PushAudio call
	// (which triggers the initial connect) must fail.
	for i := 0; i < threshold; i++ {
		sess, err := r.StartStream(context.Background(), "en")
		if err != nil {
			t.Fatalf("session %d: StartStream should succeed while breaker is closed, got: %v", i, err)
		}
		frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
		if err := sess.PushAudio(context.Background(), frame); err == nil {
			t.Fatalf("session %d: expected PushAudio to fail (vendor refuses every connect)", i)
		}
		_ = sess.Close()
	}
	attemptsAfterTrips := atomic.LoadInt32(attempts)
	if attemptsAfterTrips == 0 {
		t.Fatal("expected at least one real dial attempt across the failing sessions")
	}

	// The breaker should now be open: the next StartStream call must be
	// rejected immediately, without ever constructing a session that
	// would dial the vendor.
	sess, err := r.StartStream(context.Background(), "en")
	if err == nil {
		t.Fatal("expected StartStream to fail while the breaker is open")
	}
	if sess != nil {
		t.Error("expected a nil session when the breaker rejects StartStream")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("errors.Is(err, ErrCircuitOpen) = false for err = %v, want true", err)
	}
	if got := atomic.LoadInt32(attempts); got != attemptsAfterTrips {
		t.Errorf("dial attempts after breaker-rejected StartStream = %d, want unchanged %d (zero dial attempts)", got, attemptsAfterTrips)
	}
}

// TestDeepgramRecognizer_CircuitBreaker_RecordsErrorReason verifies that a
// circuit-open rejection is tagged via RecordErrorReason(stage="asr_connect",
// vendor="deepgram", reason="circuit_open") when a metrics recorder is
// configured, reusing the same WithMetrics recorder already used for cost.
func TestDeepgramRecognizer_CircuitBreaker_RecordsErrorReason(t *testing.T) {
	srv, wsURL, _ := newRefusingDeepgramServer(t)
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(1),
		WithCircuitBreaker(1, 10*time.Second),
		WithMetrics(metrics),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail (vendor refuses every connect)")
	}
	_ = sess.Close()

	if _, err := r.StartStream(context.Background(), "en"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second StartStream to be rejected by the (now open) breaker, got: %v", err)
	}

	if got := metrics.ReasonCount("asr_connect", "deepgram", "circuit_open"); got != 1 {
		t.Errorf("ErrorReasonCount(asr_connect, deepgram, circuit_open) = %d, want 1", got)
	}
}

// TestDeepgramRecognizer_CircuitBreaker_ProbeAfterCooldownSucceeds covers
// (c): after the cooldown elapses, exactly one probe StartStream is let
// through, and a successful initial connect on that probe session closes
// the breaker.
func TestDeepgramRecognizer_CircuitBreaker_ProbeAfterCooldownSucceeds(t *testing.T) {
	var down int32 = 1 // 1 = refuse every connect, 0 = accept
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if atomic.LoadInt32(&down) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake deepgram server: upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	const cooldown = 40 * time.Millisecond
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(1),
		WithCircuitBreaker(1, cooldown),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	// Trip the breaker with one fully-failed session.
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail (vendor refuses every connect)")
	}
	_ = sess.Close()

	// Immediately after tripping, StartStream must fail fast.
	attemptsBeforeProbe := atomic.LoadInt32(&attempts)
	if _, err := r.StartStream(context.Background(), "en"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected StartStream to be rejected while cooling down, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != attemptsBeforeProbe {
		t.Errorf("dial attempts while cooling down = %d, want unchanged %d", got, attemptsBeforeProbe)
	}

	// Let the cooldown elapse and flip the vendor back to healthy.
	time.Sleep(cooldown + 30*time.Millisecond)
	atomic.StoreInt32(&down, 0)

	probeSess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("expected the post-cooldown probe StartStream to be let through, got: %v", err)
	}
	if err := probeSess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("expected the probe session's initial connect to succeed, got: %v", err)
	}
	defer probeSess.Close()

	// Breaker should now be fully closed: a subsequent session must not
	// be fail-fast even against a now-failing vendor -- it should attempt
	// a real dial.
	atomic.StoreInt32(&down, 1)
	attemptsBeforeNext := atomic.LoadInt32(&attempts)
	nextSess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream after breaker closed: %v", err)
	}
	if err := nextSess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail against the now-down vendor")
	}
	if got := atomic.LoadInt32(&attempts) - attemptsBeforeNext; got == 0 {
		t.Error("expected a real dial attempt after the breaker closed, got none (still fail-fast)")
	}
}

// TestDeepgramRecognizer_CircuitBreaker_MidStreamReconnectDoesNotTrip
// covers (d): a session that connects successfully at least once, then
// later drops and reconnects mid-stream via the existing reconnectBackoff
// path, must never trip or otherwise interact with the circuit breaker --
// even if that later mid-stream reconnect itself fails.
func TestDeepgramRecognizer_CircuitBreaker_MidStreamReconnectDoesNotTrip(t *testing.T) {
	var connNum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connNum, 1)
		if n >= 2 {
			// Every reconnect after the first successful connection is
			// refused, simulating a vendor outage that only starts after
			// the session's initial connect already succeeded.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake deepgram server: upgrade: %v", err)
			return
		}
		// Read exactly one audio frame (the session's first successful
		// PushAudio), then drop the connection to force a mid-stream
		// reconnect on the next PushAudio call.
		_, _, _ = conn.ReadMessage()
		conn.Close()
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	// threshold=1 means a single breaker-counted failure would trip it
	// immediately, making this a strict test that the mid-stream
	// reconnect failure below truly never reaches the breaker.
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(1),
		WithCircuitBreaker(1, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("first PushAudio (initial connect) should succeed, got: %v", err)
	}

	// Give the readLoop goroutine time to observe the server-initiated
	// drop and clear the stale connection before the next PushAudio call,
	// so that call deterministically takes the "needConnect" mid-stream
	// reconnect path rather than racing a write against a half-closed
	// socket.
	time.Sleep(150 * time.Millisecond)

	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected the mid-stream reconnect to fail (vendor refuses every reconnect)")
	}
	_ = sess.Close()

	if r.breaker.open {
		t.Error("breaker should not be open after only a mid-stream reconnect failure (post initial-connect)")
	}
	if r.breaker.consecutiveFails != 0 {
		t.Errorf("breaker.consecutiveFails = %d, want 0 (mid-stream reconnects must not touch the breaker)", r.breaker.consecutiveFails)
	}

	// The breaker should still let ordinary new sessions through.
	if _, err := r.StartStream(context.Background(), "en"); err != nil {
		t.Errorf("expected a fresh StartStream to still succeed (breaker untouched), got: %v", err)
	}
}

// TestDeepgramRecognizer_MidStreamReconnectSucceeds_CostRecordedOncePerPush
// is part of the 2026-07-21 PE cost-tracking-under-retry/reconnect audit
// (see DEVLOG.md): it pins invariant (3) from that audit -- a mid-stream
// reconnect must not cause RecordCost to fire twice (or zero times) for
// the audio pushed in the PushAudio call that triggered/absorbed the
// reconnect. Unlike TestDeepgramRecognizer_CircuitBreaker_MidStreamReconnectDoesNotTrip
// above (which forces the reconnect to fail, to prove the breaker is
// untouched), this test forces the reconnect to SUCCEED and continues
// pushing audio afterward, so it can assert on the resulting cost totals:
// recordAudioCost is only ever called from the single call site at the
// end of PushAudio (see deepgram.go), reached exactly once per successful
// PushAudio call regardless of whether that call happened to need a
// reconnect internally -- this test drives that path for real against a
// fake server and checks the actual CostEventCount/CostTotal output
// rather than just reading the source.
func TestDeepgramRecognizer_MidStreamReconnectSucceeds_CostRecordedOncePerPush(t *testing.T) {
	var connNum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connNum, 1)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake deepgram server: upgrade: %v", err)
			return
		}
		defer conn.Close()
		if n == 1 {
			// The first connection accepts exactly one audio frame, then
			// drops -- forcing a mid-stream reconnect on the next
			// PushAudio call.
			_, _, _ = conn.ReadMessage()
			return
		}
		// The reconnect (and anything after it) stays up and silently
		// drains whatever audio arrives, so every later PushAudio call
		// succeeds directly.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(3),
		WithMetrics(metrics),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	// 320 bytes @ 8kHz/16-bit mono = 160 samples = 20ms of audio.
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}

	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio[0] (initial connect): %v", err)
	}

	// Give the readLoop goroutine time to observe the server's drop of
	// the first connection before the next PushAudio call, so that call
	// deterministically takes the mid-stream reconnect path (same
	// synchronization convention as
	// TestDeepgramRecognizer_CircuitBreaker_MidStreamReconnectDoesNotTrip
	// above).
	time.Sleep(150 * time.Millisecond)

	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio[1] (mid-stream reconnect, should succeed against the new connection): %v", err)
	}

	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio[2] (post-reconnect): %v", err)
	}

	const wantPushes = 3
	if got := metrics.CostEventCount("deepgram"); got != wantPushes {
		t.Errorf("CostEventCount(deepgram) = %d, want %d (exactly one RecordCost per successful PushAudio call, even though PushAudio[1] triggered a mid-stream reconnect internally -- a mismatch would mean the reconnect either double-billed or dropped a billing event)", got, wantPushes)
	}
	wantMinutes := (0.02 / 60.0) * float64(wantPushes)
	wantCost := wantMinutes * deepgramCostPerMinuteUSD
	if got := metrics.CostTotal("deepgram"); got < wantCost*0.999 || got > wantCost*1.001 {
		t.Errorf("CostTotal(deepgram) = %v, want %v (%d frames' worth of 20ms audio, no double-count introduced by the reconnect)", got, wantCost, wantPushes)
	}
}

// TestDeepgramRecognizer_CircuitBreaker_OpenRejectionNeverRecordsCost is
// part of the 2026-07-21 PE cost-tracking-under-retry/reconnect audit: it
// pins invariant (4) -- a call rejected fail-fast by an open circuit
// breaker (zero dial attempts, zero audio ever sent) must never record a
// nonzero cost. TestDeepgramRecognizer_CircuitBreaker_RecordsErrorReason
// above already checks the rejection is tagged via RecordErrorReason;
// this test checks the orthogonal cost-recording seam explicitly, which
// nothing previously asserted directly.
func TestDeepgramRecognizer_CircuitBreaker_OpenRejectionNeverRecordsCost(t *testing.T) {
	srv, wsURL, _ := newRefusingDeepgramServer(t)
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(
		WithBaseURL(wsURL),
		WithMaxReconnectAttempts(1),
		WithCircuitBreaker(1, 10*time.Second),
		WithMetrics(metrics),
	)
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail (vendor refuses every connect)")
	}
	_ = sess.Close()

	if _, err := r.StartStream(context.Background(), "en"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second StartStream to be rejected by the (now open) breaker, got: %v", err)
	}

	if got := metrics.CostEventCount("deepgram"); got != 0 {
		t.Errorf("CostEventCount(deepgram) = %d, want 0: a circuit-open rejection must never record cost (no vendor call was ever attempted, no audio was ever sent)", got)
	}
	if got := metrics.CostTotal("deepgram"); got != 0 {
		t.Errorf("CostTotal(deepgram) = %v, want 0 for a circuit-open rejection", got)
	}
}

// TestDeepgramRecognizer_CircuitBreaker_DefaultEnabledWithoutOption
// verifies a default breaker is always active, matching pkg/translate and
// pkg/tts's convention.
func TestDeepgramRecognizer_CircuitBreaker_DefaultEnabledWithoutOption(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer()
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	if r.breaker == nil {
		t.Fatal("expected a non-nil default circuit breaker when WithCircuitBreaker is not used")
	}
	if r.breaker.threshold != defaultBreakerFailureThreshold {
		t.Errorf("default threshold = %d, want %d", r.breaker.threshold, defaultBreakerFailureThreshold)
	}
	if r.breaker.cooldown != defaultBreakerCooldown {
		t.Errorf("default cooldown = %v, want %v", r.breaker.cooldown, defaultBreakerCooldown)
	}
}

// --- Hindi (Nova-3 code-switching) wiring ---------------------------------

// newFakeDeepgramServerCapturingURL is like newFakeDeepgramServer but also
// publishes the exact request URL (query string and all) the client
// connected with, so tests can assert on which model/language/endpointing
// wire params a given languageHint actually produced.
func newFakeDeepgramServerCapturingURL(t *testing.T, handler func(t *testing.T, conn *websocket.Conn)) (*httptest.Server, string, chan *url.URL) {
	t.Helper()
	urls := make(chan *url.URL, 1)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls <- r.URL
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake deepgram server: upgrade: %v", err)
			return
		}
		defer conn.Close()
		handler(t, conn)
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"
	return srv, wsURL, urls
}

func drainConn(t *testing.T, conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// TestDeepgramRecognizer_HindiConnectsWithNova3MultiWireParams verifies
// that languageHint "hi" connects Deepgram with model=nova-3 and the
// wire-level language=multi parameter (Nova-3's real-time Hindi-English
// code-switching mode -- see DeepgramRecognizer's doc comment), NOT
// model=nova-2 with language=hi (Nova-2 has no Hindi code-switching
// support at all, only Spanish+English -- this is exactly the bug this
// fix avoids relative to just widening the old English-only language
// check). It also checks the code-switching-specific endpointing=100
// hint is set.
func TestDeepgramRecognizer_HindiConnectsWithNova3MultiWireParams(t *testing.T) {
	srv, wsURL, urls := newFakeDeepgramServerCapturingURL(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case gotURL := <-urls:
		q := gotURL.Query()
		if got := q.Get("model"); got != "nova-3" {
			t.Errorf("model = %q, want %q", got, "nova-3")
		}
		if got := q.Get("language"); got != "multi" {
			t.Errorf("language = %q, want %q (Nova-3's code-switching wire mode, not \"hi\")", got, "multi")
		}
		if got := q.Get("endpointing"); got != "100" {
			t.Errorf("endpointing = %q, want %q for a code-switching session", got, "100")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake server to receive a connection")
	}
}

// TestDeepgramRecognizer_EnglishConnectsWithNova2Params verifies "en"
// still connects with model=nova-2 and language=en, unchanged from before
// this fix, and does not set the code-switching-specific endpointing
// override.
func TestDeepgramRecognizer_EnglishConnectsWithNova2Params(t *testing.T) {
	srv, wsURL, urls := newFakeDeepgramServerCapturingURL(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case gotURL := <-urls:
		q := gotURL.Query()
		if got := q.Get("model"); got != "nova-2" {
			t.Errorf("model = %q, want %q", got, "nova-2")
		}
		if got := q.Get("language"); got != "en" {
			t.Errorf("language = %q, want %q", got, "en")
		}
		if got := q.Get("endpointing"); got != "" {
			t.Errorf("endpointing = %q, want unset for a plain English session", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake server to receive a connection")
	}
}

// TestDeepgramRecognizer_EmptyLanguageHintDefaultsToEnglishNova2 verifies
// StartStream's "" -> "en" default still resolves to the Nova-2 (not
// Nova-3) wire params.
func TestDeepgramRecognizer_EmptyLanguageHintDefaultsToEnglishNova2(t *testing.T) {
	srv, wsURL, urls := newFakeDeepgramServerCapturingURL(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case gotURL := <-urls:
		q := gotURL.Query()
		if got := q.Get("model"); got != "nova-2" {
			t.Errorf("model = %q, want %q", got, "nova-2")
		}
		if got := q.Get("language"); got != "en" {
			t.Errorf("language = %q, want %q", got, "en")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake server to receive a connection")
	}
}

// TestDeepgramRecognizer_WithDeepgramHindiModelOverride verifies
// WithDeepgramHindiModel overrides the Hindi-path model independently of
// WithDeepgramModel (which only affects the English path).
func TestDeepgramRecognizer_WithDeepgramHindiModelOverride(t *testing.T) {
	srv, wsURL, urls := newFakeDeepgramServerCapturingURL(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL), WithDeepgramHindiModel("nova-3-custom"))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case gotURL := <-urls:
		if got := gotURL.Query().Get("model"); got != "nova-3-custom" {
			t.Errorf("model = %q, want %q", got, "nova-3-custom")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake server to receive a connection")
	}
}

// TestDeepgramRecognizer_RecordsHindiCostPerAudioMinute verifies Hindi
// (Nova-3 "multi") sessions bill at deepgramHindiCostPerMinuteUSD, not
// the English Nova-2 rate.
func TestDeepgramRecognizer_RecordsHindiCostPerAudioMinute(t *testing.T) {
	srv, wsURL := newFakeDeepgramServer(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	// 320 bytes @ 8kHz/16-bit mono = 160 samples = 20ms of audio.
	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	wantMinutes := 0.02 / 60.0
	want := wantMinutes * deepgramHindiCostPerMinuteUSD
	got := metrics.CostTotal("deepgram")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(deepgram) = %v, want %v (Hindi/nova-3-multi rate)", got, want)
	}
	if got == metrics.CostTotal("deepgram") && want == wantMinutes*deepgramCostPerMinuteUSD {
		t.Fatal("test setup bug: Hindi and English rates must differ for this test to be meaningful")
	}
}

// TestDeepgramRecognizer_EnglishSessionUnaffectedByHindiOnSameRecognizer
// closes a gap in PE's own Hindi/Nova-3 test coverage above: every one of
// PE's new tests
// (TestDeepgramRecognizer_HindiConnectsWithNova3MultiWireParams/
// TestDeepgramRecognizer_EnglishConnectsWithNova2Params/
// TestDeepgramRecognizer_WithDeepgramHindiModelOverride/
// TestDeepgramRecognizer_RecordsHindiCostPerAudioMinute) starts exactly
// one StreamSession, of one language, per *DeepgramRecognizer instance --
// none of them prove that a single, long-lived DeepgramRecognizer (the
// real, production shape: one Recognizer instance shared by
// langstream.NewSession for *both* the caller and agent legs, see
// session.go's NewSession) produces fully independent, non-cross-
// contaminated behavior when it is actually used for both an English and
// a Hindi session at once, which is exactly how the Hindi feature is
// meant to be used (a bilingual call: e.g. Hindi-speaking caller,
// English-speaking agent).
//
// This test builds one DeepgramRecognizer configured with a Hindi-only
// model override (WithDeepgramHindiModel) and no English override at all,
// starts an English StreamSession first and confirms it connects with
// plain, unmodified Nova-2 English params, then starts a second, Hindi
// StreamSession from that *same* recognizer and confirms it independently
// gets the Nova-3/"multi"/endpointing=100 code-switching params --
// proving the Hindi-specific fields (hindiModel/wireLanguage, resolved
// once per StartStream call in deepgram.go's StartStream) never bleed
// into or get bled into by the English path on a shared Recognizer. It
// also pushes audio on both sessions and confirms their recorded costs
// land at each language's own distinct per-minute rate on one shared
// metrics recorder, with no bleed-over of one session's rate into the
// other's.
func TestDeepgramRecognizer_EnglishSessionUnaffectedByHindiOnSameRecognizer(t *testing.T) {
	srv, wsURL, urls := newFakeDeepgramServerCapturingURL(t, func(t *testing.T, conn *websocket.Conn) {
		drainConn(t, conn)
	})
	defer srv.Close()

	os.Setenv("DEEPGRAM_API_KEY", "unit-test-key")
	metrics := observability.NewLatencyRecorder()
	r, err := NewDeepgramRecognizer(WithBaseURL(wsURL), WithDeepgramHindiModel("hindi-only-override"), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}

	// English session first -- started, connected, and its wire params
	// captured entirely before the Hindi session (configured with its own
	// model override on this same recognizer) is even started.
	enSess, err := r.StartStream(context.Background(), "en")
	if err != nil {
		t.Fatalf("StartStream(en): %v", err)
	}
	defer enSess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 8000} // 20ms @ 8kHz
	if err := enSess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio (en): %v", err)
	}

	select {
	case gotURL := <-urls:
		q := gotURL.Query()
		if got := q.Get("model"); got != "nova-2" {
			t.Errorf("english session model = %q, want %q (must be unaffected by this recognizer's Hindi-only model override)", got, "nova-2")
		}
		if got := q.Get("language"); got != "en" {
			t.Errorf("english session language = %q, want %q", got, "en")
		}
		if got := q.Get("endpointing"); got != "" {
			t.Errorf("english session endpointing = %q, want unset (code-switching endpointing must not leak into the English path)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the english session's connection")
	}

	// Now start the Hindi session on the *same* recognizer instance.
	hiSess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream(hi): %v", err)
	}
	defer hiSess.Close()

	if err := hiSess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio (hi): %v", err)
	}

	select {
	case gotURL := <-urls:
		q := gotURL.Query()
		if got := q.Get("model"); got != "hindi-only-override" {
			t.Errorf("hindi session model = %q, want %q", got, "hindi-only-override")
		}
		if got := q.Get("language"); got != "multi" {
			t.Errorf("hindi session language = %q, want %q", got, "multi")
		}
		if got := q.Get("endpointing"); got != "100" {
			t.Errorf("hindi session endpointing = %q, want %q", got, "100")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hindi session's connection")
	}

	// Cost: 20ms of audio pushed on each session. Total recorded cost
	// across this one shared metrics recorder must equal the sum of each
	// session's *own* rate -- if the two sessions' costs were ever
	// cross-contaminated (e.g. a shared/last-write-wins rate instead of
	// each session's own s.wireLanguage), this sum would come out wrong
	// even though each individual session's URL params (checked above)
	// looked correct.
	wantMinutes := 0.02 / 60.0
	wantTotal := wantMinutes*deepgramCostPerMinuteUSD + wantMinutes*deepgramHindiCostPerMinuteUSD
	gotTotal := metrics.CostTotal("deepgram")
	if diff := gotTotal - wantTotal; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(deepgram) = %v, want %v (english rate + hindi rate, no cross-contamination)", gotTotal, wantTotal)
	}
}
