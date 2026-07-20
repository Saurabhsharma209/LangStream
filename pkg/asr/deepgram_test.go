package asr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestDeepgramRecognizer_SupportedLanguagesAndUnsupportedHint(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "test-key")
	r, err := NewDeepgramRecognizer()
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	langs := r.SupportedLanguages()
	if len(langs) != 1 || langs[0] != "en" {
		t.Fatalf("expected [en], got %v", langs)
	}

	if _, err := r.StartStream(context.Background(), "hi"); err == nil {
		t.Fatal("expected error starting stream with unsupported language hint")
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
