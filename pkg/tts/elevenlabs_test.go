package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// --- fake ElevenLabs HTTP server -----------------------------------
//
// ElevenLabs' streaming TTS endpoint is a plain HTTP POST whose response
// body is a raw, headerless, chunked PCM stream (see elevenlabs.go's
// package doc comment) -- much simpler to fake than Cartesia's WebSocket
// protocol: an httptest.Server handler that decodes the JSON request body,
// then writes+flushes raw bytes (or a non-200 status) is enough.

// fakeElevenLabsRequest captures what SynthesizeStream sent, for tests to
// assert against.
type fakeElevenLabsRequest struct {
	voiceID string
	apiKey  string
	ctype   string
	query   string
	body    elevenlabsRequest
}

// newFakeElevenLabsServer starts a server whose /v1/text-to-speech/
// handler decodes the request and hands it, plus the ResponseWriter, to
// handle so each test can script exactly the response it wants (chunks,
// a non-200 error, etc).
func newFakeElevenLabsServer(t *testing.T, handle func(w http.ResponseWriter, req fakeElevenLabsRequest)) *fakeElevenLabsServerHandle {
	t.Helper()
	fs := &fakeElevenLabsServerHandle{}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/text-to-speech/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/text-to-speech/")
		voiceID := strings.TrimSuffix(path, "/stream")

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body", http.StatusInternalServerError)
			return
		}
		var reqBody elevenlabsRequest
		if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		fakeReq := fakeElevenLabsRequest{
			voiceID: voiceID,
			apiKey:  r.Header.Get("xi-api-key"),
			ctype:   r.Header.Get("Content-Type"),
			query:   r.URL.RawQuery,
			body:    reqBody,
		}

		fs.mu.Lock()
		fs.lastRequest = fakeReq
		fs.mu.Unlock()

		handle(w, fakeReq)
	})

	fs.Server = httptest.NewServer(mux)
	return fs
}

type fakeElevenLabsServerHandle struct {
	*httptest.Server
	mu          sync.Mutex
	lastRequest fakeElevenLabsRequest
}

func (fs *fakeElevenLabsServerHandle) request() fakeElevenLabsRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.lastRequest
}

// newTestElevenLabsSynthesizer builds an ElevenLabsSynthesizer pointed at
// the fake server, setting ELEVENLABS_API_KEY on the current test's
// environment since the constructor requires it.
func newTestElevenLabsSynthesizer(t *testing.T, fs *fakeElevenLabsServerHandle, opts ...ElevenLabsOption) *ElevenLabsSynthesizer {
	t.Helper()
	t.Setenv("ELEVENLABS_API_KEY", "test-api-key")
	allOpts := append([]ElevenLabsOption{WithElevenLabsBaseURL(fs.URL), WithElevenLabsDialTimeout(2 * time.Second)}, opts...)
	e, err := NewElevenLabsSynthesizer(allOpts...)
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	return e
}

func drainElevenLabsChunks(t *testing.T, ch <-chan AudioChunk, timeout time.Duration) []AudioChunk {
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

// flushWriter is satisfied by httptest's ResponseWriter (which implements
// http.Flusher), used so fake handlers can force each Write onto the wire
// immediately rather than being buffered until the handler returns.
type flushWriter interface {
	io.Writer
	http.Flusher
}

// --- tests --------------------------------------------------------------

func TestElevenLabsSynthesizer_ConstructionRequiresAPIKey(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "")
	if _, err := NewElevenLabsSynthesizer(); err == nil {
		t.Fatal("expected error when ELEVENLABS_API_KEY is unset, got nil")
	}
}

func TestElevenLabsSynthesizer_NameAndLanguages(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	if got := e.Name(); got != "elevenlabs" {
		t.Errorf("Name() = %q, want %q", got, "elevenlabs")
	}
	langs := e.SupportedLanguages()
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

// TestElevenLabsSynthesizer_RequestShapeAndChunkAssembly verifies that
// SynthesizeStream sends the right URL/headers/body to ElevenLabs, and
// correctly assembles the server's raw PCM stream into AudioChunk values,
// ending with exactly one IsFinal chunk whose PCM bytes match exactly
// what the fake server sent.
func TestElevenLabsSynthesizer_RequestShapeAndChunkAssembly(t *testing.T) {
	const wantText = "namaste, how can I help you today?"
	persona := Persona{VoiceID: "default-hi", Language: LanguageHindi, Gender: "neutral"}

	pcm1 := []byte{1, 2, 3, 4}
	pcm2 := []byte{5, 6, 7, 8, 9, 10}

	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		wantVoice := elevenlabsVoices[LanguageHindi]["default-hi"]
		if req.voiceID != wantVoice {
			t.Errorf("voice id in URL = %q, want %q", req.voiceID, wantVoice)
		}
		if req.apiKey != "test-api-key" {
			t.Errorf("xi-api-key = %q, want %q", req.apiKey, "test-api-key")
		}
		if req.ctype != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", req.ctype)
		}
		if req.query != "output_format=pcm_8000" {
			t.Errorf("query = %q, want output_format=pcm_8000", req.query)
		}
		if req.body.Text != wantText {
			t.Errorf("request text = %q, want %q", req.body.Text, wantText)
		}
		if req.body.ModelID != elevenlabsDefaultModel {
			t.Errorf("request model_id = %q, want %q", req.body.ModelID, elevenlabsDefaultModel)
		}
		if req.body.LanguageCode != "hi" {
			t.Errorf("request language_code = %q, want %q", req.body.LanguageCode, "hi")
		}

		fw, ok := w.(flushWriter)
		if !ok {
			t.Fatalf("ResponseWriter does not support flushing")
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fw.Write(pcm1); err != nil {
			t.Errorf("writing pcm1: %v", err)
			return
		}
		fw.Flush()
		time.Sleep(50 * time.Millisecond) // force a separate read on the client side
		if _, err := fw.Write(pcm2); err != nil {
			t.Errorf("writing pcm2: %v", err)
			return
		}
		fw.Flush()
	})
	defer fs.Close()

	e := newTestElevenLabsSynthesizer(t, fs)
	ch, err := e.SynthesizeStream(context.Background(), wantText, persona)
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	// Whether the server's EOF arrives attached to the last data read or
	// as a separate zero-length read (both happen in practice against a
	// real *http.Response.Body depending on timing -- see readLoop's doc
	// comment) is a race this test must tolerate: assert on the
	// reassembled byte stream and the IsFinal invariant rather than an
	// exact chunk count.
	chunks := drainElevenLabsChunks(t, ch, 5*time.Second)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2: %+v", len(chunks), chunks)
	}

	var gotPCM []byte
	for _, c := range chunks {
		gotPCM = append(gotPCM, c.PCM...)
	}
	wantPCM := append(append([]byte{}, pcm1...), pcm2...)
	if string(gotPCM) != string(wantPCM) {
		t.Errorf("reassembled PCM = %v, want %v", gotPCM, wantPCM)
	}

	for i, c := range chunks {
		wantFinal := i == len(chunks)-1
		if c.IsFinal != wantFinal {
			t.Errorf("chunk[%d].IsFinal = %v, want %v (exactly the last chunk should be final)", i, c.IsFinal, wantFinal)
		}
		if c.SampleRate != elevenlabsDefaultSampleRate {
			t.Errorf("chunk[%d].SampleRate = %d, want %d", i, c.SampleRate, elevenlabsDefaultSampleRate)
		}
	}
	if !chunks[0].IsFinal && string(chunks[0].PCM) != string(pcm1) {
		t.Errorf("chunk[0].PCM = %v, want %v", chunks[0].PCM, pcm1)
	}
}

// TestElevenLabsSynthesizer_SingleChunkStreamIsFinal covers the simpler
// case of a single write from the server: the one chunk delivered must
// still be marked IsFinal.
func TestElevenLabsSynthesizer_SingleChunkStreamIsFinal(t *testing.T) {
	pcm := []byte{42, 42, 42, 42}
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pcm)
	})
	defer fs.Close()

	e := newTestElevenLabsSynthesizer(t, fs)
	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	chunks := drainElevenLabsChunks(t, ch, 5*time.Second)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if !chunks[0].IsFinal {
		t.Errorf("chunks[0].IsFinal = false, want true")
	}
	if string(chunks[0].PCM) != string(pcm) {
		t.Errorf("chunks[0].PCM = %v, want %v", chunks[0].PCM, pcm)
	}
}

func TestElevenLabsSynthesizer_EmptyText(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	if _, err := e.SynthesizeStream(context.Background(), "", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected error for empty text, got nil")
	}
}

func TestElevenLabsSynthesizer_UnsupportedLanguage(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	if _, err := e.SynthesizeStream(context.Background(), "bonjour", Persona{Language: "fr"}); err == nil {
		t.Fatal("expected error for unsupported language, got nil")
	}
}

// TestElevenLabsSynthesizer_NonOKStatusSurfacesAsError is the error-path
// test for a non-200 response: it must return an error (with the response
// body's content included) rather than a channel, and must never return
// both a non-nil channel and a non-nil error.
func TestElevenLabsSynthesizer_NonOKStatusSurfacesAsError(t *testing.T) {
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_voice_id","message":"voice not found"}}`))
	})
	defer fs.Close()

	e := newTestElevenLabsSynthesizer(t, fs)
	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error for a non-200 response, got nil")
	}
	if ch != nil {
		t.Errorf("expected a nil channel on error, got %v", ch)
	}
	if !strings.Contains(err.Error(), "voice not found") {
		t.Errorf("error = %v, want it to include the response body's message", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want it to include the status code", err)
	}
}

// TestElevenLabsSynthesizer_ConnectionError is the error-path test for
// connection failure: pointing the client at an address nothing is
// listening on must return a synchronous error from SynthesizeStream, not
// a channel that hangs or panics.
func TestElevenLabsSynthesizer_ConnectionError(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	// Reserve a port, then close it immediately so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	e, err := NewElevenLabsSynthesizer(
		WithElevenLabsBaseURL("http://"+addr),
		WithElevenLabsDialTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	if ch != nil {
		t.Errorf("expected a nil channel on error, got %v", ch)
	}
}

// TestElevenLabsSynthesizer_ContextCancelDoesNotHang mirrors Cartesia's
// equivalent test: cancelling ctx mid-stream must close the channel
// promptly instead of leaking the reader goroutine or blocking forever on
// a server that never finishes sending.
func TestElevenLabsSynthesizer_ContextCancelDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		fw, ok := w.(flushWriter)
		if !ok {
			t.Fatalf("ResponseWriter does not support flushing")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fw.Write([]byte{1, 2, 3, 4})
		fw.Flush()
		// Hold the connection open (no more data, no close) until the
		// test releases it, simulating a server that never finishes; the
		// client's context cancellation -- not the server -- must be
		// what ends the stream.
		<-release
	})
	defer fs.Close()
	defer close(release)

	e := newTestElevenLabsSynthesizer(t, fs)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := e.SynthesizeStream(ctx, "hello", Persona{Language: LanguageEnglish})
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

// --- voiceFor fallback tests ---------------------------------------

func TestElevenLabsSynthesizer_VoiceFor_KnownVoiceID(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	got := e.voiceFor(Persona{VoiceID: "agent-female-en", Language: LanguageEnglish})
	want := elevenlabsVoices[LanguageEnglish]["agent-female-en"]
	if got != want {
		t.Errorf("voiceFor(agent-female-en) = %q, want %q", got, want)
	}
}

func TestElevenLabsSynthesizer_VoiceFor_UnknownVoiceIDFallsBackToLanguageDefault(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	got := e.voiceFor(Persona{VoiceID: "totally-unknown-voice-slot", Language: LanguageHindi})
	want := elevenlabsVoices[LanguageHindi]["default-hi"]
	if got != want {
		t.Errorf("voiceFor(unknown, hi) = %q, want default-hi %q", got, want)
	}
}

func TestElevenLabsSynthesizer_VoiceFor_UnknownLanguageFallsBackToEnglish(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer()
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}
	got := e.voiceFor(Persona{VoiceID: "default-fr", Language: "fr"})
	want := elevenlabsVoices[LanguageEnglish]["default-en"]
	if got != want {
		t.Errorf("voiceFor(default-fr, fr) = %q, want English default %q", got, want)
	}
}

func TestElevenLabsSynthesizer_DefaultVoicePerLanguage(t *testing.T) {
	tests := []struct {
		lang Language
		want string
	}{
		{LanguageEnglish, elevenlabsVoices[LanguageEnglish]["default-en"]},
		{LanguageHindi, elevenlabsVoices[LanguageHindi]["default-hi"]},
	}
	for _, tc := range tests {
		var gotVoice string
		fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
			gotVoice = req.voiceID
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{9, 9})
		})

		e := newTestElevenLabsSynthesizer(t, fs)
		// Persona.VoiceID intentionally left blank: this exercises the
		// "no persona specified" default-per-language path.
		ch, err := e.SynthesizeStream(context.Background(), "hi", Persona{Language: tc.lang})
		if err != nil {
			fs.Close()
			t.Fatalf("SynthesizeStream(%s): %v", tc.lang, err)
		}
		drainElevenLabsChunks(t, ch, 5*time.Second)
		fs.Close()

		if gotVoice != tc.want {
			t.Errorf("language %s: default voice = %q, want %q", tc.lang, gotVoice, tc.want)
		}
	}
}

// --- retry-with-backoff and cost-recording tests ------------------------

// newCountingElevenLabsServer starts a server whose /v1/text-to-speech/
// handler is invoked via respond(attempt, w) for every request
// (1-indexed), so tests can script "fail N times, then succeed"
// sequences to exercise SynthesizeStream's retry loop deterministically.
func newCountingElevenLabsServer(t *testing.T, respond func(attempt int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/text-to-speech/", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		respond(int(n), w)
	})
	srv := httptest.NewServer(mux)
	return srv, &attempts
}

func TestElevenLabsSynthesizer_RetriesOn429ThenSucceeds(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	srv, attempts := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":{"status":"rate_limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pcm)
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}
}

func TestElevenLabsSynthesizer_RetriesOn5xxThenSucceeds(t *testing.T) {
	pcm := []byte{9, 9, 9, 9}
	srv, attempts := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pcm)
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); int(got) != elevenlabsMaxAttempts {
		t.Errorf("attempts = %d, want elevenlabsMaxAttempts = %d", got, elevenlabsMaxAttempts)
	}
}

func TestElevenLabsSynthesizer_DoesNotRetryOn400(t *testing.T) {
	srv, attempts := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_voice_id"}}`))
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	_, err = e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error for a 400 response, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (400 must fail fast, not retry)", got)
	}
}

func TestElevenLabsSynthesizer_ExhaustsRetriesOnPersistentFailure(t *testing.T) {
	srv, attempts := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	_, err = e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(attempts); int(got) != elevenlabsMaxAttempts {
		t.Errorf("attempts = %d, want elevenlabsMaxAttempts = %d", got, elevenlabsMaxAttempts)
	}
}

func TestElevenLabsSynthesizer_RecordsCostPerCharacter(t *testing.T) {
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})
	defer fs.Close()

	metrics := observability.NewLatencyRecorder()
	e := newTestElevenLabsSynthesizer(t, fs, WithElevenLabsMetrics(metrics))

	const text = "namaste, how can I help you today?"
	ch, err := e.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)

	want := float64(len(text)) * elevenlabsCostPerCharUSD
	got := metrics.CostTotal("elevenlabs")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(elevenlabs) = %v, want %v", got, want)
	}
	if n := metrics.CostEventCount("elevenlabs"); n != 1 {
		t.Errorf("CostEventCount(elevenlabs) = %d, want 1", n)
	}
}

func TestElevenLabsSynthesizer_FailedRequestDoesNotRecordCost(t *testing.T) {
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_voice_id"}}`))
	})
	defer fs.Close()

	metrics := observability.NewLatencyRecorder()
	e := newTestElevenLabsSynthesizer(t, fs, WithElevenLabsMetrics(metrics))

	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected an error, got nil")
	}
	if n := metrics.CostEventCount("elevenlabs"); n != 0 {
		t.Errorf("CostEventCount(elevenlabs) = %d, want 0 for a failed request", n)
	}
}

func TestElevenLabsSynthesizer_NoMetricsConfiguredNoOp(t *testing.T) {
	fs := newFakeElevenLabsServer(t, func(w http.ResponseWriter, req fakeElevenLabsRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{5, 6, 7, 8})
	})
	defer fs.Close()

	e := newTestElevenLabsSynthesizer(t, fs)
	ch, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)
}

// TestElevenLabsSynthesizer_RetrySucceeds_RecordsCostExactlyOnce is part
// of the 2026-07-21 PE cost-tracking-under-retry/reconnect audit (see
// DEVLOG.md): it strengthens
// TestElevenLabsSynthesizer_RetriesOn429ThenSucceeds's scenario (a
// transient 429 on the first request, success on the retry) with an
// explicit cost-recording assertion -- pinning invariant (1) from that
// audit: a retry-then-succeed call must record cost exactly once, not
// once per HTTP attempt.
func TestElevenLabsSynthesizer_RetrySucceeds_RecordsCostExactlyOnce(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	srv, attempts := newCountingElevenLabsServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":{"status":"rate_limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pcm)
	})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	metrics := observability.NewLatencyRecorder()
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsMetrics(metrics))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	const text = "hello"
	ch, err := e.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	drainElevenLabsChunks(t, ch, 5*time.Second)

	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}

	if n := metrics.CostEventCount("elevenlabs"); n != 1 {
		t.Errorf("CostEventCount(elevenlabs) = %d, want exactly 1 for a call that failed once (429) then succeeded on retry -- a mismatch would mean RecordCost fired once per HTTP attempt instead of once per SynthesizeStream call", n)
	}
	want := float64(len(text)) * elevenlabsCostPerCharUSD
	if got := metrics.CostTotal("elevenlabs"); got < want*0.999 || got > want*1.001 {
		t.Errorf("CostTotal(elevenlabs) = %v, want %v (one character-based charge, not doubled by the retried request)", got, want)
	}
}

// TestElevenLabsSynthesizer_MidStreamDropAfterAccept_CostStillBilledOnce
// is part of the 2026-07-21 PE cost-tracking-under-retry/reconnect audit:
// it specifically targets invariant (2) -- "no cost recorded for content
// not delivered" -- for ElevenLabs' design. See elevenlabsCostPerCharUSD's
// doc comment and SynthesizeStream's RecordCost call site: ElevenLabs
// bills per character of input text once the request is accepted (HTTP
// 200), not per second of audio actually streamed back afterward, because
// that is how ElevenLabs' real vendor billing works (the "cost" tracked
// here is real dollars already owed once the request was accepted,
// independent of whether the resulting audio later reaches the caller).
// This test drives a real mid-stream connection drop -- the server
// returns HTTP 200 with a Content-Length promising more bytes than it
// actually sends, then closes the connection, so the response body read
// fails with a genuine transport error partway through -- and confirms
// this is NOT a cost-tracking bug: the channel closes without ever
// delivering an IsFinal=true chunk (a failed synthesis by this package's
// own documented contract; see readLoop's doc comment), while the cost
// recorded at accept time is still exactly one character-based charge,
// neither zeroed out nor doubled by the later failure.
func TestElevenLabsSynthesizer_MidStreamDropAfterAccept_CostStillBilledOnce(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/text-to-speech/", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		body := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		// Promise far more bytes than are ever actually sent via
		// Content-Length, then close the raw connection after writing
		// only `body` -- the client's body.Read() will surface a genuine
		// transport error (io.ErrUnexpectedEOF) partway through, exactly
		// like a real dropped connection mid-audio-stream.
		header := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", len(body)+100000)
		if _, err := bufrw.Writer.WriteString(header); err != nil {
			return
		}
		if _, err := bufrw.Writer.Write(body); err != nil {
			return
		}
		_ = bufrw.Writer.Flush()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	metrics := observability.NewLatencyRecorder()
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsMetrics(metrics))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	const text = "namaste, how can I help you today?"
	ch, err := e.SynthesizeStream(context.Background(), text, Persona{Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	chunks := drainElevenLabsChunks(t, ch, 5*time.Second)

	for i, chunk := range chunks {
		if chunk.IsFinal {
			t.Errorf("chunk[%d].IsFinal = true, want false: the connection dropped before the promised audio was fully delivered", i)
		}
	}

	want := float64(len(text)) * elevenlabsCostPerCharUSD
	got := metrics.CostTotal("elevenlabs")
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostTotal(elevenlabs) = %v, want %v (billed once at accept time; unaffected by the later mid-stream drop, per this package's documented per-character-on-accept billing design)", got, want)
	}
	if n := metrics.CostEventCount("elevenlabs"); n != 1 {
		t.Errorf("CostEventCount(elevenlabs) = %d, want exactly 1 (recorded once at accept time, neither dropped nor doubled by the later mid-stream failure)", n)
	}
}

// TestElevenLabsCircuitBreaker_OpenRejectionNeverRecordsCost is part of
// the 2026-07-21 PE cost-tracking-under-retry/reconnect audit: it pins
// invariant (4) -- a call rejected fail-fast by an open circuit breaker
// (zero HTTP requests ever made) must never record a nonzero cost.
func TestElevenLabsCircuitBreaker_OpenRejectionNeverRecordsCost(t *testing.T) {
	var failing int32 = 1
	srv := newToggleElevenLabsServer(t, &failing, []byte{1, 2, 3, 4})
	defer srv.Close()

	t.Setenv("ELEVENLABS_API_KEY", "test-key")
	metrics := observability.NewLatencyRecorder()
	const threshold = 1
	e, err := NewElevenLabsSynthesizer(WithElevenLabsBaseURL(srv.URL), WithElevenLabsDialTimeout(2*time.Second), WithElevenLabsCircuitBreaker(threshold, 10*time.Second), WithElevenLabsMetrics(metrics))
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	if _, err := e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish}); err == nil {
		t.Fatal("expected the first call to fail (server returns 500)")
	}

	_, err = e.SynthesizeStream(context.Background(), "hello", Persona{Language: LanguageEnglish})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second call to be rejected by the (now open) breaker, got: %v", err)
	}

	if got := metrics.CostEventCount("elevenlabs"); got != 0 {
		t.Errorf("CostEventCount(elevenlabs) = %d, want 0: a circuit-open rejection must never record cost (no HTTP request was ever made)", got)
	}
	if got := metrics.CostTotal("elevenlabs"); got != 0 {
		t.Errorf("CostTotal(elevenlabs) = %v, want 0 for a circuit-open rejection", got)
	}
}
