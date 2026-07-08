package tts

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
