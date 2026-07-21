package tts

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/observability"
)

// --- fake Cartesia WebSocket server -----------------------------------
//
// These helpers speak just enough server-side RFC 6455 to exercise
// CartesiaSynthesizer against a local httptest.Server instead of the real
// Cartesia API: accept the handshake, read the client's one JSON
// generation request, then let the test's handler decide what to write
// back (chunks/done/error/garbage/nothing).

// fakeCartesiaServer wraps an httptest.Server whose /tts/websocket
// handler hands the caller the raw connection plus the decoded generation
// request, so each test can script exactly the responses it wants.
type fakeCartesiaServer struct {
	*httptest.Server

	mu           sync.Mutex
	lastRequest  cartesiaGenerationRequest
	lastAPIKey   string
	lastVersion  string
	gotRequest   chan struct{}
	gotRequestCh sync.Once
}

// newFakeCartesiaServer starts a server whose handler is invoked once the
// client's generation request has been received and parsed, on its own
// goroutine, with the hijacked connection.
func newFakeCartesiaServer(t *testing.T, handle func(conn net.Conn, req cartesiaGenerationRequest)) *fakeCartesiaServer {
	t.Helper()
	fs := &fakeCartesiaServer{gotRequest: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}

		fs.mu.Lock()
		fs.lastAPIKey = r.Header.Get("X-API-Key")
		fs.lastVersion = r.Header.Get("Cartesia-Version")
		fs.mu.Unlock()

		accept := computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if _, err := rw.Writer.WriteString(resp); err != nil {
			conn.Close()
			return
		}
		if err := rw.Writer.Flush(); err != nil {
			conn.Close()
			return
		}

		_, _, payload, err := readWSFrame(rw.Reader)
		if err != nil {
			conn.Close()
			return
		}
		var req cartesiaGenerationRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			conn.Close()
			return
		}

		fs.mu.Lock()
		fs.lastRequest = req
		fs.mu.Unlock()
		fs.gotRequestCh.Do(func() { close(fs.gotRequest) })

		handle(conn, req)
	})

	fs.Server = httptest.NewServer(mux)
	return fs
}

// wsURL returns fs's address as a ws:// base URL suitable for
// WithBaseURL.
func (fs *fakeCartesiaServer) wsURL() string {
	return "ws://" + strings.TrimPrefix(fs.Server.URL, "http://")
}

// writeServerFrame writes an unmasked frame (as the real Cartesia server
// would: RFC 6455 requires server->client frames be unmasked).
func writeServerFrame(w interface{ Write([]byte) (int, error) }, opcode byte, payload []byte) error {
	n := len(payload)
	header := []byte{0x80 | opcode}
	switch {
	case n <= 125:
		header = append(header, byte(n))
	case n <= 65535:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		t := make([]byte, 9)
		t[0] = 127
		for i := 0; i < 8; i++ {
			t[8-i] = byte(n >> (8 * i))
		}
		header = append(header, t[1:]...)
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// newTestSynthesizer builds a CartesiaSynthesizer pointed at the fake
// server, setting CARTESIA_API_KEY on the current test's environment
// since the constructor requires it.
func newTestSynthesizer(t *testing.T, fs *fakeCartesiaServer, opts ...Option) *CartesiaSynthesizer {
	t.Helper()
	t.Setenv("CARTESIA_API_KEY", "test-api-key")
	allOpts := append([]Option{WithBaseURL(fs.wsURL()), WithDialTimeout(2 * time.Second)}, opts...)
	c, err := NewCartesiaSynthesizer(allOpts...)
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	return c
}

func drainChunks(t *testing.T, ch <-chan AudioChunk, timeout time.Duration) []AudioChunk {
	t.Helper()
	var chunks []AudioChunk
	deadline := time.After(timeout)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return chunks
			}
			chunks = append(chunks, c)
		case <-deadline:
			t.Fatalf("timed out draining channel; got %d chunks so far", len(chunks))
		}
	}
}

// --- tests --------------------------------------------------------------

func TestCartesiaSynthesizer_ConstructionRequiresAPIKey(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "")
	if _, err := NewCartesiaSynthesizer(); err == nil {
		t.Fatal("expected error when CARTESIA_API_KEY is unset, got nil")
	}
}

func TestCartesiaSynthesizer_NameAndLanguages(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "test-key")
	c, err := NewCartesiaSynthesizer()
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	if got := c.Name(); got != "cartesia" {
		t.Errorf("Name() = %q, want %q", got, "cartesia")
	}
	langs := c.SupportedLanguages()
	want := map[Language]bool{LanguageEnglish: false, LanguageHindi: false}
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
}

// TestCartesiaSynthesizer_RequestShapeAndChunkAssembly verifies that
// SynthesizeStream sends the correct text/voice/language in its
// generation request, and correctly assembles the server's streamed
// base64 chunks into the Synthesizer interface's AudioChunk type, ending
// with exactly one IsFinal chunk.
func TestCartesiaSynthesizer_RequestShapeAndChunkAssembly(t *testing.T) {
	const wantText = "namaste, how can I help you today?"
	persona := Persona{VoiceID: "default-hi", Language: LanguageHindi, Gender: "neutral"}

	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()

		wantVoice := cartesiaVoices[LanguageHindi]["default-hi"]
		if req.Voice.ID != wantVoice {
			t.Errorf("request voice.id = %q, want %q", req.Voice.ID, wantVoice)
		}
		if req.Voice.Mode != "id" {
			t.Errorf("request voice.mode = %q, want %q", req.Voice.Mode, "id")
		}
		if req.Language != "hi" {
			t.Errorf("request language = %q, want %q", req.Language, "hi")
		}
		if req.Transcript != wantText {
			t.Errorf("request transcript = %q, want %q", req.Transcript, wantText)
		}
		if req.ContextID == "" {
			t.Errorf("request context_id is empty")
		}
		if req.OutputFormat.Encoding != "pcm_s16le" || req.OutputFormat.Container != "raw" {
			t.Errorf("request output_format = %+v, want raw/pcm_s16le", req.OutputFormat)
		}

		pcm1 := []byte{1, 2, 3, 4}
		pcm2 := []byte{5, 6, 7, 8, 9, 10}

		chunk1 := mustMarshal(t, cartesiaMessage{
			Type:      "chunk",
			Data:      base64.StdEncoding.EncodeToString(pcm1),
			Done:      false,
			ContextID: req.ContextID,
		})
		if err := writeServerFrame(conn, wsOpText, chunk1); err != nil {
			t.Errorf("writing chunk1: %v", err)
			return
		}

		chunk2 := mustMarshal(t, cartesiaMessage{
			Type:      "chunk",
			Data:      base64.StdEncoding.EncodeToString(pcm2),
			Done:      true,
			ContextID: req.ContextID,
		})
		if err := writeServerFrame(conn, wsOpText, chunk2); err != nil {
			t.Errorf("writing chunk2: %v", err)
			return
		}
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs)
	ch, err := c.SynthesizeStream(context.Background(), wantText, persona)
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	chunks := drainChunks(t, ch, 5*time.Second)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if string(chunks[0].PCM) != string([]byte{1, 2, 3, 4}) {
		t.Errorf("chunk[0].PCM = %v, want [1 2 3 4]", chunks[0].PCM)
	}
	if chunks[0].IsFinal {
		t.Errorf("chunk[0].IsFinal = true, want false")
	}
	if string(chunks[1].PCM) != string([]byte{5, 6, 7, 8, 9, 10}) {
		t.Errorf("chunk[1].PCM = %v, want [5 6 7 8 9 10]", chunks[1].PCM)
	}
	if !chunks[1].IsFinal {
		t.Errorf("chunk[1].IsFinal = false, want true")
	}
	for i, c := range chunks {
		if c.SampleRate != cartesiaDefaultSampleRate {
			t.Errorf("chunk[%d].SampleRate = %d, want %d", i, c.SampleRate, cartesiaDefaultSampleRate)
		}
	}
}

// TestCartesiaSynthesizer_DoneMessageEndsStreamWithoutTrailingChunk covers
// the case where the server signals completion via a bare {"type":"done"}
// message rather than a chunk with done:true.
func TestCartesiaSynthesizer_DoneMessageEndsStream(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()
		chunk := mustMarshal(t, cartesiaMessage{Type: "chunk", Data: base64.StdEncoding.EncodeToString([]byte{9}), Done: false, ContextID: req.ContextID})
		_ = writeServerFrame(conn, wsOpText, chunk)
		done := mustMarshal(t, cartesiaMessage{Type: "done", Done: true, ContextID: req.ContextID})
		_ = writeServerFrame(conn, wsOpText, done)
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs)
	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	chunks := drainChunks(t, ch, 5*time.Second)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
}

func TestCartesiaSynthesizer_DefaultVoicePerLanguage(t *testing.T) {
	tests := []struct {
		lang Language
		want string
	}{
		{LanguageEnglish, cartesiaVoices[LanguageEnglish]["default-en"]},
		{LanguageHindi, cartesiaVoices[LanguageHindi]["default-hi"]},
	}
	for _, tc := range tests {
		var gotVoice string
		fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
			defer conn.Close()
			gotVoice = req.Voice.ID
			done := mustMarshal(t, cartesiaMessage{Type: "done", Done: true, ContextID: req.ContextID})
			_ = writeServerFrame(conn, wsOpText, done)
		})

		c := newTestSynthesizer(t, fs)
		// Persona.VoiceID intentionally left blank: this exercises the
		// "no persona specified" default-per-language path.
		ch, err := c.SynthesizeStream(context.Background(), "hi", Persona{Language: tc.lang})
		if err != nil {
			fs.Close()
			t.Fatalf("SynthesizeStream(%s): %v", tc.lang, err)
		}
		drainChunks(t, ch, 5*time.Second)
		fs.Close()

		if gotVoice != tc.want {
			t.Errorf("language %s: default voice = %q, want %q", tc.lang, gotVoice, tc.want)
		}
	}
}

func TestCartesiaSynthesizer_EmptyText(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "test-key")
	c, err := NewCartesiaSynthesizer()
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	if _, err := c.SynthesizeStream(context.Background(), "", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected error for empty text, got nil")
	}
}

func TestCartesiaSynthesizer_UnsupportedLanguage(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "test-key")
	c, err := NewCartesiaSynthesizer()
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	if _, err := c.SynthesizeStream(context.Background(), "bonjour", Persona{Language: "fr"}); err == nil {
		t.Fatal("expected error for unsupported language, got nil")
	}
}

// TestCartesiaSynthesizer_ConnectionError is the error-path test for
// connection failure: pointing the client at an address nothing is
// listening on must return a synchronous error from SynthesizeStream, not
// a channel that hangs or panics.
func TestCartesiaSynthesizer_ConnectionError(t *testing.T) {
	t.Setenv("CARTESIA_API_KEY", "test-key")
	// Reserve a port, then close it immediately so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	c, err := NewCartesiaSynthesizer(
		WithBaseURL("ws://"+addr),
		WithDialTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	if ch != nil {
		t.Errorf("expected a nil channel on error, got %v", ch)
	}
}

// TestCartesiaSynthesizer_MalformedChunkClosesChannel is the malformed-
// response error path: the server sends a chunk whose "data" field is not
// valid base64. The client must not panic and must close the channel
// without ever emitting an IsFinal chunk (per readLoop's documented
// contract), rather than emitting corrupted PCM.
func TestCartesiaSynthesizer_MalformedChunkClosesChannel(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()
		good := mustMarshal(t, cartesiaMessage{Type: "chunk", Data: base64.StdEncoding.EncodeToString([]byte{1, 2}), Done: false, ContextID: req.ContextID})
		if err := writeServerFrame(conn, wsOpText, good); err != nil {
			return
		}
		bad := mustMarshal(t, cartesiaMessage{Type: "chunk", Data: "not-valid-base64!!", Done: false, ContextID: req.ContextID})
		_ = writeServerFrame(conn, wsOpText, bad)
		// Deliberately never send a done/final message: readLoop should
		// give up on the malformed frame rather than hang waiting for one.
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs)
	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	chunks := drainChunks(t, ch, 5*time.Second)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want exactly 1 (the good chunk before the malformed one)", len(chunks))
	}
	if chunks[0].IsFinal {
		t.Errorf("chunks[0].IsFinal = true, want false (stream ended abnormally, not via a final chunk)")
	}
}

// TestCartesiaSynthesizer_ServerErrorMessageClosesChannel covers the
// {"type":"error"} response path: the channel must close cleanly (no
// panic, no final chunk) rather than surface the error as a chunk.
func TestCartesiaSynthesizer_ServerErrorMessageClosesChannel(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()
		errMsg := mustMarshal(t, cartesiaMessage{
			Type:       "error",
			Title:      "Invalid voice",
			Message:    "voice id not found",
			ErrorCode:  "voice_not_found",
			StatusCode: 400,
			ContextID:  req.ContextID,
		})
		_ = writeServerFrame(conn, wsOpText, errMsg)
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs)
	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	chunks := drainChunks(t, ch, 5*time.Second)
	if len(chunks) != 0 {
		t.Fatalf("got %d chunks, want 0 after a server error message", len(chunks))
	}
}

// TestCartesiaSynthesizer_ContextCancelDoesNotHang mirrors
// mock.go's equivalent test: cancelling ctx mid-stream must close the
// channel promptly instead of leaking the reader goroutine forever.
func TestCartesiaSynthesizer_ContextCancelDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()
		chunk := mustMarshal(t, cartesiaMessage{Type: "chunk", Data: base64.StdEncoding.EncodeToString([]byte{1}), Done: false, ContextID: req.ContextID})
		if err := writeServerFrame(conn, wsOpText, chunk); err != nil {
			return
		}
		// Hold the connection open (no more data) until the test is done,
		// simulating a server that never finishes; the client's context
		// cancellation -- not the server -- must be what ends the stream.
		<-release
	})
	defer fs.Close()
	defer close(release)

	c := newTestSynthesizer(t, fs)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.SynthesizeStream(ctx, "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			// A second value arriving is fine as long as the channel then
			// closes promptly; keep draining until it does.
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-time.After(2 * time.Second):
					t.Fatal("channel did not close after context cancellation")
				}
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after context cancellation")
	}
}

func TestCartesiaSynthesizer_SendsAuthHeaders(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		defer conn.Close()
		done := mustMarshal(t, cartesiaMessage{Type: "done", Done: true, ContextID: req.ContextID})
		_ = writeServerFrame(conn, wsOpText, done)
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs, WithAPIVersion("2099-01-01"))
	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.lastAPIKey != "test-api-key" {
		t.Errorf("X-API-Key sent = %q, want %q", fs.lastAPIKey, "test-api-key")
	}
	if fs.lastVersion != "2099-01-01" {
		t.Errorf("Cartesia-Version sent = %q, want %q", fs.lastVersion, "2099-01-01")
	}
}

// sanity check that the fake server's un-exported frame helper compiles
// against bufio.Reader too, since readWSFrame is shared with production
// code and takes exactly that type.
var _ = bufio.NewReader
var _ = fmt.Sprintf

// --- retry-with-backoff and cost-recording tests ------------------------

// newCountingCartesiaServer starts a server whose /tts/websocket handler
// is invoked via respond(attempt, w, r) for every request (1-indexed), so
// tests can script "reject the handshake N times, then succeed" sequences
// to exercise SynthesizeStream's retry loop deterministically.
func newCountingCartesiaServer(t *testing.T, respond func(attempt int, w http.ResponseWriter, r *http.Request)) (*httptest.Server, *int32) {
	t.Helper()
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		respond(int(n), w, r)
	})
	srv := httptest.NewServer(mux)
	return srv, &attempts
}

// acceptCartesiaHandshakeAndFinish hijacks the connection, completes the
// WebSocket handshake, drains the client's one generation request frame,
// and immediately sends a {"type":"done"} message so the caller's
// readLoop finishes right away.
func acceptCartesiaHandshakeAndFinish(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	accept := computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.Writer.WriteString(resp); err != nil {
		return
	}
	if err := rw.Writer.Flush(); err != nil {
		return
	}

	if _, _, _, err := readWSFrame(rw.Reader); err != nil {
		return
	}
	doneMsg, _ := json.Marshal(cartesiaMessage{Type: "done"})
	_ = writeServerFrame(rw.Writer, wsOpText, doneMsg)
	_ = rw.Writer.Flush()
}

func TestCartesiaSynthesizer_RetriesOn429ThenSucceeds(t *testing.T) {
	srv, attempts := newCountingCartesiaServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		acceptCartesiaHandshakeAndFinish(t, w, r)
	})
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}
}

func TestCartesiaSynthesizer_RetriesOn5xxThenSucceeds(t *testing.T) {
	srv, attempts := newCountingCartesiaServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("bad gateway"))
			return
		}
		acceptCartesiaHandshakeAndFinish(t, w, r)
	})
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); int(got) != cartesiaMaxAttempts {
		t.Errorf("attempts = %d, want cartesiaMaxAttempts = %d", got, cartesiaMaxAttempts)
	}
}

func TestCartesiaSynthesizer_DoesNotRetryOn400(t *testing.T) {
	srv, attempts := newCountingCartesiaServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	})
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	_, err = c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error for a 400 handshake rejection, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (400 must fail fast, not retry)", got)
	}
}

func TestCartesiaSynthesizer_RecordsCostPerCharacter(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		doneMsg := mustMarshal(t, cartesiaMessage{Type: "done"})
		_ = writeServerFrame(conn, wsOpText, doneMsg)
	})
	defer fs.Close()

	metrics := observability.NewLatencyRecorder()
	c := newTestSynthesizer(t, fs, WithMetrics(metrics))

	const text = "namaste, how can I help you today?"
	ch, err := c.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	want := float64(len(text)) * cartesiaCostPerCharUSD
	got := metrics.CostTotal("cartesia")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(cartesia) = %v, want %v", got, want)
	}
	if n := metrics.CostEventCount("cartesia"); n != 1 {
		t.Errorf("CostEventCount(cartesia) = %d, want 1", n)
	}
}

func TestCartesiaSynthesizer_NoMetricsConfiguredNoOp(t *testing.T) {
	fs := newFakeCartesiaServer(t, func(conn net.Conn, req cartesiaGenerationRequest) {
		doneMsg := mustMarshal(t, cartesiaMessage{Type: "done"})
		_ = writeServerFrame(conn, wsOpText, doneMsg)
	})
	defer fs.Close()

	c := newTestSynthesizer(t, fs)
	ch, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)
}

// TestCartesiaSynthesizer_RetrySucceeds_RecordsCostExactlyOnce is part of
// the 2026-07-21 PE cost-tracking-under-retry/reconnect audit (see
// DEVLOG.md): it strengthens
// TestCartesiaSynthesizer_RetriesOn429ThenSucceeds's scenario (a transient
// 429 on the first handshake attempt, success on the retry) with an
// explicit cost-recording assertion -- pinning invariant (1) from that
// audit: a retry-then-succeed call must record cost exactly once, using a
// fresh context_id/attempt each time, not once per handshake attempt.
func TestCartesiaSynthesizer_RetrySucceeds_RecordsCostExactlyOnce(t *testing.T) {
	srv, attempts := newCountingCartesiaServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		acceptCartesiaHandshakeAndFinish(t, w, r)
	})
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	metrics := observability.NewLatencyRecorder()
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	const text = "hello"
	ch, err := c.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}

	if n := metrics.CostEventCount("cartesia"); n != 1 {
		t.Errorf("CostEventCount(cartesia) = %d, want exactly 1 for a call whose handshake failed once (429) then succeeded on retry -- a mismatch would mean RecordCost fired once per handshake attempt instead of once per SynthesizeStream call", n)
	}
	want := float64(len(text)) * cartesiaCostPerCharUSD
	if got := metrics.CostTotal("cartesia"); got < want*0.999 || got > want*1.001 {
		t.Errorf("CostTotal(cartesia) = %v, want %v (one character-based charge, not doubled by the retried handshake)", got, want)
	}
}

// TestCartesiaSynthesizer_MidStreamDropAfterAccept_CostStillBilledOnce is
// part of the 2026-07-21 PE cost-tracking-under-retry/reconnect audit: it
// specifically targets invariant (2) -- "no cost recorded for content not
// delivered" -- for Cartesia's design. See cartesiaCostPerCharUSD's doc
// comment and SynthesizeStream's RecordCost call site: Cartesia bills per
// character of *accepted* input text once the generation request's
// handshake succeeds, not per second of audio actually streamed back
// afterward, because that is how Cartesia's real vendor billing works
// (the "cost" tracked here is real dollars already owed once Cartesia has
// accepted the request, independent of whether the resulting audio later
// reaches the caller). This test drives a real mid-stream connection drop
// -- the handshake succeeds and the generation request is accepted, but
// the server then closes the raw connection without ever sending a
// "chunk" or "done" message -- and confirms this is NOT a cost-tracking
// bug: the channel closes with no IsFinal=true chunk ever delivered (a
// failed synthesis by this package's own documented contract; see
// readLoop's doc comment), while the cost recorded at accept time is
// still exactly one character-based charge, neither zeroed out nor
// doubled by the later failure.
func TestCartesiaSynthesizer_MidStreamDropAfterAccept_CostStillBilledOnce(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		accept := computeAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if _, err := rw.Writer.WriteString(resp); err != nil {
			return
		}
		if err := rw.Writer.Flush(); err != nil {
			return
		}

		// Drain the client's one generation request frame (so
		// connectAndSend's write succeeds and SynthesizeStream proceeds
		// to record cost), then close the raw connection immediately
		// without ever writing a "chunk" or "done" message back --
		// simulating a real mid-stream drop after Cartesia has already
		// accepted the request.
		if _, _, _, err := readWSFrame(rw.Reader); err != nil {
			return
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	metrics := observability.NewLatencyRecorder()
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	const text = "namaste, how can I help you today?"
	ch, err := c.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	chunks := drainChunks(t, ch, 5*time.Second)

	for i, chunk := range chunks {
		if chunk.IsFinal {
			t.Errorf("chunk[%d].IsFinal = true, want false: no audio was ever delivered before the connection dropped", i)
		}
	}

	want := float64(len(text)) * cartesiaCostPerCharUSD
	got := metrics.CostTotal("cartesia")
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostTotal(cartesia) = %v, want %v (billed once at accept time; unaffected by the later mid-stream drop, per this package's documented per-character-on-accept billing design)", got, want)
	}
	if n := metrics.CostEventCount("cartesia"); n != 1 {
		t.Errorf("CostEventCount(cartesia) = %d, want exactly 1 (recorded once at accept time, neither dropped nor doubled by the later mid-stream failure)", n)
	}
}

// TestCartesiaCircuitBreaker_OpenRejectionNeverRecordsCost is part of the
// 2026-07-21 PE cost-tracking-under-retry/reconnect audit: it pins
// invariant (4) -- a call rejected fail-fast by an open circuit breaker
// (zero handshake attempts) must never record a nonzero cost.
func TestCartesiaCircuitBreaker_OpenRejectionNeverRecordsCost(t *testing.T) {
	var failing int32 = 1
	srv := newToggleCartesiaServer(t, &failing)
	defer srv.Close()

	t.Setenv("CARTESIA_API_KEY", "test-key")
	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	metrics := observability.NewLatencyRecorder()
	const threshold = 1
	c, err := NewCartesiaSynthesizer(WithBaseURL(wsURL), WithDialTimeout(2*time.Second), WithCircuitBreaker(threshold, 10*time.Second), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}

	if _, err := c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail (server returns 500)")
	}

	_, err = c.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second call to be rejected by the (now open) breaker, got: %v", err)
	}

	if got := metrics.CostEventCount("cartesia"); got != 0 {
		t.Errorf("CostEventCount(cartesia) = %d, want 0: a circuit-open rejection must never record cost (no handshake was ever attempted)", got)
	}
	if got := metrics.CostTotal("cartesia"); got != 0 {
		t.Errorf("CostTotal(cartesia) = %v, want 0 for a circuit-open rejection", got)
	}
}
