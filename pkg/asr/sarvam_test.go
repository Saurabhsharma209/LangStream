package asr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newFakeSarvamServer(t *testing.T, handler func(t *testing.T, conn *websocket.Conn)) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("fake sarvam server: upgrade: %v", err)
			return
		}
		defer conn.Close()
		handler(t, conn)
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"
	return srv, wsURL
}

func TestSarvamRecognizer_MissingAPIKey(t *testing.T) {
	old, had := os.LookupEnv("SARVAM_API_KEY")
	os.Unsetenv("SARVAM_API_KEY")
	defer func() {
		if had {
			os.Setenv("SARVAM_API_KEY", old)
		}
	}()

	if _, err := NewSarvamRecognizer(); err == nil {
		t.Fatal("expected error when SARVAM_API_KEY is unset, got nil")
	}
}

func TestSarvamRecognizer_SupportedLanguagesAndUnsupportedHint(t *testing.T) {
	os.Setenv("SARVAM_API_KEY", "test-key")
	r, err := NewSarvamRecognizer()
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	langs := r.SupportedLanguages()
	want := map[Language]bool{"hi": false, "en": false}
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

	if _, err := r.StartStream(context.Background(), "ta"); err == nil {
		t.Fatal("expected error starting stream with unsupported language hint")
	}
}

// TestSarvamRecognizer_DefaultsToHindiCodemix verifies that an empty
// language hint (auto/code-switch mode) still connects successfully and
// requests Hindi with codemix mode, per this backend's whole purpose.
func TestSarvamRecognizer_DefaultsToHindiCodemix(t *testing.T) {
	receivedQuery := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery <- r.URL.RawQuery
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
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
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"

	os.Setenv("SARVAM_API_KEY", "unit-test-key")
	r, err := NewSarvamRecognizer(WithSarvamBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case q := <-receivedQuery:
		if !strings.Contains(q, "language-code=hi-IN") {
			t.Errorf("query %q does not request hi-IN", q)
		}
		if !strings.Contains(q, "mode=codemix") {
			t.Errorf("query %q does not request codemix mode", q)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connect request")
	}
}

// TestSarvamRecognizer_SendsAudioAndParsesTranscript verifies the client
// base64-encodes pushed PCM into Sarvam's JSON audio message shape and
// parses a Sarvam-shaped {"type":"data",...} response into a Transcript.
func TestSarvamRecognizer_SendsAudioAndParsesTranscript(t *testing.T) {
	receivedAudioB64 := make(chan string, 1)

	srv, wsURL := newFakeSarvamServer(t, func(t *testing.T, conn *websocket.Conn) {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("fake server: ReadMessage: %v", err)
			return
		}
		if mt != websocket.TextMessage {
			t.Errorf("expected client audio message to be a text JSON frame, got type %d", mt)
		}
		var msg sarvamAudioMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Errorf("fake server: could not parse audio message: %v", err)
			return
		}
		receivedAudioB64 <- msg.Audio.Data

		result := `{
			"type": "data",
			"data": {
				"request_id": "req-1",
				"transcript": "मेरा phone number है",
				"metrics": {"audio_duration": 1.2, "processing_latency": 0.1}
			}
		}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(result)); err != nil {
			t.Errorf("fake server: write result: %v", err)
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	os.Setenv("SARVAM_API_KEY", "unit-test-key")
	r, err := NewSarvamRecognizer(WithSarvamBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	frame := AudioFrame{PCM: pcm, SampleRate: 16000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("PushAudio: %v", err)
	}

	select {
	case gotB64 := <-receivedAudioB64:
		want := base64.StdEncoding.EncodeToString(pcm)
		if gotB64 != want {
			t.Errorf("server received base64 audio %q, want %q", gotB64, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake server to receive audio")
	}

	select {
	case tr := <-sess.Transcripts():
		if tr.Text != "मेरा phone number है" {
			t.Errorf("Text = %q, want the code-mixed sample transcript", tr.Text)
		}
		if !tr.IsFinal {
			t.Error("expected IsFinal=true (see assumption 3 in sarvam.go)")
		}
		if tr.Language != "hi" {
			t.Errorf("Language = %q, want hi", tr.Language)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for transcript")
	}
}

// TestSarvamRecognizer_VendorErrorClosesSession verifies that an
// error-shaped response is treated as fatal: the session closes and
// PushAudio returns a real error afterwards.
func TestSarvamRecognizer_VendorErrorClosesSession(t *testing.T) {
	srv, wsURL := newFakeSarvamServer(t, func(t *testing.T, conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		errMsg, _ := json.Marshal(map[string]string{
			"type":  "error",
			"error": "invalid subscription key",
		})
		_ = conn.WriteMessage(websocket.TextMessage, errMsg)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	os.Setenv("SARVAM_API_KEY", "bad-key")
	r, err := NewSarvamRecognizer(WithSarvamBaseURL(wsURL))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}
	if err := sess.PushAudio(context.Background(), frame); err != nil {
		t.Fatalf("first PushAudio should succeed before the error arrives: %v", err)
	}

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

	if err := sess.PushAudio(context.Background(), frame); err == nil {
		t.Fatal("expected PushAudio to fail after a fatal vendor error closed the session")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close after failure should be a no-op returning nil, got: %v", err)
	}
}

// TestSarvamRecognizer_ConnectFailureSurfacesError verifies that when the
// vendor endpoint is unreachable, PushAudio returns a real error rather than
// panicking or hanging.
func TestSarvamRecognizer_ConnectFailureSurfacesError(t *testing.T) {
	os.Setenv("SARVAM_API_KEY", "unit-test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"
	srv.Close()

	r, err := NewSarvamRecognizer(WithSarvamBaseURL(deadURL), WithSarvamMaxReconnectAttempts(1))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}
	sess, err := r.StartStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	frame := AudioFrame{PCM: make([]byte, 320), SampleRate: 16000}
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
