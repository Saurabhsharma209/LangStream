// Deepgram streaming ASR backend.
//
// Protocol details below were verified against Deepgram's public docs as of
// 2026-07-08 (https://developers.deepgram.com/reference/speech-to-text/listen-streaming,
// https://developers.deepgram.com/docs/lower-level-websockets,
// https://developers.deepgram.com/docs/audio-keep-alive,
// https://developers.deepgram.com/docs/close-stream,
// https://developers.deepgram.com/docs/errors):
//
//   - Endpoint: wss://api.deepgram.com/v1/listen (GET, 101 Switching Protocols).
//   - Auth: "Authorization: Token <API_KEY>" request header.
//   - Connection query params used here: encoding=linear16, sample_rate,
//     channels=1, language, model, interim_results=true, punctuate=true,
//     vad_events=true. (encoding=linear16 + sample_rate is required because
//     we send raw 16-bit PCM, not a self-describing container like WAV.)
//   - Audio is sent as binary WebSocket frames containing raw PCM.
//   - Control messages are sent as JSON text frames: {"type":"KeepAlive"}
//     every 3-5s during silence (Deepgram closes with NET-0001 after 10s of
//     no audio/KeepAlive), and {"type":"CloseStream"} to flush + end cleanly.
//   - Results arrive as JSON text frames of shape:
//     {"type":"Results","is_final":bool,"speech_final":bool,"start":sec,
//     "duration":sec,"channel":{"alternatives":[{"transcript":"...",
//     "confidence":0.9}]}}. Other message types we may see and ignore for
//     Recognizer purposes: "Metadata", "UtteranceEnd", "SpeechStarted".
//   - Deepgram's own error payload shape for the streaming path is not
//     published beyond the general REST error shape
//     ({"err_code":"...","err_msg":"..."}) documented at
//     https://developers.deepgram.com/docs/errors; we handle that shape
//     defensively but the exact streaming error frame format is unverified,
//     so we also fall back to surfacing raw WebSocket close/read errors.
package asr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/exotel/langstream/pkg/observability"
	"github.com/gorilla/websocket"
)

const (
	defaultDeepgramBaseURL = "wss://api.deepgram.com/v1/listen"
	defaultDeepgramModel   = "nova-2"

	// dgKeepAliveInterval must stay comfortably under Deepgram's documented
	// 10-second NET-0001 idle timeout.
	dgKeepAliveInterval = 5 * time.Second

	dgReconnectBase = 250 * time.Millisecond
	dgReconnectMax  = 5 * time.Second

	// deepgramCostPerMinuteUSD approximates Deepgram's published
	// Pay-As-You-Go pricing for Nova-2 streaming transcription
	// (~$0.0059 per audio-minute processed, per deepgram.com/pricing as
	// reviewed while writing this). This is for pilot cost-visibility
	// only, not billing-grade accuracy -- Deepgram's actual rates vary by
	// plan/commitment and change over time, and this value is not read
	// live from any API.
	deepgramCostPerMinuteUSD = 0.0059
)

// DeepgramOption configures a DeepgramRecognizer.
type DeepgramOption func(*DeepgramRecognizer)

// WithBaseURL overrides the Deepgram WebSocket endpoint. Intended for tests,
// which point this at a local httptest/WebSocket server instead of the real
// wss://api.deepgram.com/v1/listen.
func WithBaseURL(u string) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.baseURL = u }
}

// WithDeepgramModel overrides the Deepgram model query parameter (default
// "nova-2").
func WithDeepgramModel(model string) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.model = model }
}

// WithMaxReconnectAttempts caps how many consecutive times a session will
// try to re-establish its WebSocket connection after a disconnect before
// giving up and failing PushAudio/closing the session. Default 3.
func WithMaxReconnectAttempts(n int) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.maxReconnectAttempts = n }
}

// WithDialer overrides the gorilla/websocket dialer used to connect. Mainly
// useful for tests that need a custom TLS or timeout configuration.
func WithDialer(d *websocket.Dialer) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.dialer = d }
}

// WithMetrics wires a shared *observability.LatencyRecorder into this
// recognizer so every audio frame successfully pushed to Deepgram
// attributes its cost (see RecordCost) to the "deepgram" vendor, per
// deepgramCostPerMinuteUSD. Optional -- a nil/unset recorder (the
// default) makes cost recording a no-op, matching this package's
// existing functional-options convention (WithBaseURL, WithDialer, ...).
func WithMetrics(m *observability.LatencyRecorder) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.metrics = m }
}

// DeepgramRecognizer is a real, English-only streaming ASR backend backed by
// Deepgram's live transcription WebSocket API.
type DeepgramRecognizer struct {
	apiKey               string
	baseURL              string
	model                string
	maxReconnectAttempts int
	dialer               *websocket.Dialer
	metrics              *observability.LatencyRecorder
}

// NewDeepgramRecognizer builds a Deepgram-backed Recognizer. It reads the
// API key from the DEEPGRAM_API_KEY environment variable; construction fails
// if that variable is unset or empty, since every Deepgram request requires
// authentication.
func NewDeepgramRecognizer(opts ...DeepgramOption) (*DeepgramRecognizer, error) {
	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("asr/deepgram: DEEPGRAM_API_KEY environment variable is not set")
	}
	r := &DeepgramRecognizer{
		apiKey:               apiKey,
		baseURL:              defaultDeepgramBaseURL,
		model:                defaultDeepgramModel,
		maxReconnectAttempts: 3,
		dialer:               websocket.DefaultDialer,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Name implements Recognizer.
func (r *DeepgramRecognizer) Name() string { return "deepgram" }

// SupportedLanguages implements Recognizer. This backend is scoped to
// English only for the pilot.
func (r *DeepgramRecognizer) SupportedLanguages() []Language {
	return []Language{"en"}
}

// StartStream implements Recognizer. The underlying WebSocket connection is
// opened lazily on the first PushAudio call, because Deepgram requires the
// audio sample rate as a connect-time query parameter and StartStream does
// not receive audio frames up front.
func (r *DeepgramRecognizer) StartStream(ctx context.Context, languageHint Language) (StreamSession, error) {
	lang := languageHint
	if lang == "" {
		lang = "en"
	}
	if lang != "en" {
		return nil, fmt.Errorf("asr/deepgram: unsupported language %q (this backend is English-only)", lang)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	s := &deepgramSession{
		r:      r,
		lang:   lang,
		ctx:    sessCtx,
		cancel: cancel,
		out:    make(chan Transcript, 64),
	}
	return s, nil
}

var _ Recognizer = (*DeepgramRecognizer)(nil)

// deepgramSession implements StreamSession against a live Deepgram
// WebSocket connection.
//
// Concurrency model: mu guards session/connection state (closed, conn, gen,
// reconnectAttempts). writeMu serializes writes to the current conn (audio
// frames from PushAudio and control/keepalive frames), since gorilla's
// websocket.Conn forbids concurrent writers. gen is bumped on every
// (re)connect so that goroutines belonging to a superseded connection can
// tell they are stale and exit quietly instead of corrupting state for the
// current connection.
type deepgramSession struct {
	r    *DeepgramRecognizer
	lang Language

	ctx    context.Context
	cancel context.CancelFunc

	out chan Transcript

	mu                sync.Mutex
	conn              *websocket.Conn
	sampleRate        int
	gen               int
	closed            bool
	reconnectAttempts int
	fatalErr          error

	writeMu sync.Mutex

	sendWG   sync.WaitGroup // in-flight sends to `out`; mirrors mock.go's pattern.
	workerWG sync.WaitGroup // reader + keepalive goroutines of the *current* gen.
}

// PushAudio implements StreamSession.
func (s *deepgramSession) PushAudio(ctx context.Context, frame AudioFrame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	if s.closed {
		err := s.fatalErr
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("asr/deepgram: session closed: %w", err)
		}
		return fmt.Errorf("asr/deepgram: session closed")
	}
	needConnect := s.conn == nil
	s.mu.Unlock()

	if needConnect {
		if err := s.ensureConnected(frame.SampleRate); err != nil {
			s.failAndClose(err)
			return err
		}
	}

	if err := s.writeAudio(frame.PCM); err != nil {
		// Basic, documented reconnect: on a write failure we assume the
		// connection dropped mid-stream, reconnect once (fresh Deepgram
		// session -- Deepgram has no resume/session-token concept for
		// live transcription, so any audio already buffered server-side
		// for the old connection is lost), and retry the write exactly
		// once. If that also fails we surface the error and the caller
		// (orchestrator) decides whether to give up on this leg.
		if connErr := s.ensureConnected(frame.SampleRate); connErr != nil {
			s.failAndClose(connErr)
			return connErr
		}
		if err := s.writeAudio(frame.PCM); err != nil {
			s.failAndClose(err)
			return fmt.Errorf("asr/deepgram: write audio after reconnect: %w", err)
		}
	}
	s.recordAudioCost(frame)
	return nil
}

// recordAudioCost attributes the cost of processing one successfully
// pushed audio frame to the "deepgram" vendor, in USD, based on the
// frame's duration and deepgramCostPerMinuteUSD. It is a no-op if no
// metrics recorder was configured via WithMetrics.
func (s *deepgramSession) recordAudioCost(frame AudioFrame) {
	if s.r.metrics == nil || frame.SampleRate <= 0 {
		return
	}
	samples := len(frame.PCM) / 2 // 16-bit mono PCM
	minutes := float64(samples) / float64(frame.SampleRate) / 60.0
	s.r.metrics.RecordCost("deepgram", minutes*deepgramCostPerMinuteUSD)
}

// ensureConnected dials Deepgram if there is no live connection, applying
// the shared reconnect backoff across attempts. It is safe to call whether
// or not a connection already exists (no-op if one does).
func (s *deepgramSession) ensureConnected(sampleRate int) error {
	s.mu.Lock()
	if s.conn != nil {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("asr/deepgram: session closed")
	}
	attempt := s.reconnectAttempts
	maxAttempts := s.r.maxReconnectAttempts
	s.mu.Unlock()

	if attempt >= maxAttempts {
		return fmt.Errorf("asr/deepgram: exceeded max reconnect attempts (%d)", maxAttempts)
	}
	if attempt > 0 {
		delay := reconnectBackoff(attempt-1, dgReconnectBase, dgReconnectMax)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		}
	}

	wsURL, err := s.buildURL(sampleRate)
	if err != nil {
		return fmt.Errorf("asr/deepgram: build connect URL: %w", err)
	}
	header := http.Header{}
	header.Set("Authorization", "Token "+s.r.apiKey)

	conn, _, err := s.r.dialer.DialContext(s.ctx, wsURL, header)
	if err != nil {
		s.mu.Lock()
		s.reconnectAttempts++
		s.mu.Unlock()
		return fmt.Errorf("asr/deepgram: connect: %w", err)
	}

	s.mu.Lock()
	s.conn = conn
	s.sampleRate = sampleRate
	s.gen++
	gen := s.gen
	s.mu.Unlock()

	s.workerWG.Add(2)
	go s.readLoop(conn, gen)
	go s.keepAliveLoop(conn, gen)
	return nil
}

func (s *deepgramSession) buildURL(sampleRate int) (string, error) {
	u, err := url.Parse(s.r.baseURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(sampleRate))
	q.Set("channels", "1")
	q.Set("language", string(s.lang))
	q.Set("model", s.r.model)
	q.Set("interim_results", "true")
	q.Set("punctuate", "true")
	q.Set("vad_events", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *deepgramSession) writeAudio(pcm []byte) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("asr/deepgram: not connected")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, pcm)
}

// dgResultsMessage models Deepgram's "Results" streaming message. See the
// package doc comment for the source of this shape.
type dgResultsMessage struct {
	Type      string  `json:"type"`
	IsFinal   bool    `json:"is_final"`
	Start     float64 `json:"start"`
	Duration  float64 `json:"duration"`
	FromFinal bool    `json:"from_finalize"`
	Channel   struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}

// dgErrorMessage models the general Deepgram REST error shape
// ({"err_code","err_msg"}, see https://developers.deepgram.com/docs/errors).
// The exact shape of an error delivered over an already-open streaming
// connection is not published; we treat any message containing a non-empty
// err_code/err_msg pair as fatal.
type dgErrorMessage struct {
	ErrCode string `json:"err_code"`
	ErrMsg  string `json:"err_msg"`
}

func (s *deepgramSession) readLoop(conn *websocket.Conn, gen int) {
	defer s.workerWG.Done()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			s.onConnLost(gen, err)
			return
		}
		s.handleMessage(gen, data)
	}
}

func (s *deepgramSession) handleMessage(gen int, data []byte) {
	var errMsg dgErrorMessage
	if err := json.Unmarshal(data, &errMsg); err == nil && errMsg.ErrCode != "" {
		s.failAndClose(fmt.Errorf("asr/deepgram: %s: %s", errMsg.ErrCode, errMsg.ErrMsg))
		return
	}

	var msg dgResultsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		// Unparseable frame: ignore rather than crash the session. This
		// covers "Metadata"/"UtteranceEnd"/"SpeechStarted" payloads whose
		// fields don't match dgResultsMessage's shape either -- they will
		// simply produce a message with an empty transcript, which the
		// check below filters out anyway.
		return
	}
	if msg.Type != "Results" {
		return
	}
	if len(msg.Channel.Alternatives) == 0 {
		return
	}
	alt := msg.Channel.Alternatives[0]
	if alt.Transcript == "" {
		// Deepgram periodically emits empty interim results; skip them.
		return
	}

	s.mu.Lock()
	if s.closed || gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.sendWG.Add(1)
	s.mu.Unlock()

	t := Transcript{
		Text:       alt.Transcript,
		Language:   s.lang,
		IsFinal:    msg.IsFinal,
		Confidence: alt.Confidence,
		StartMS:    int64(msg.Start * 1000),
		EndMS:      int64((msg.Start + msg.Duration) * 1000),
	}
	s.send(t)
	s.sendWG.Done()
}

func (s *deepgramSession) send(t Transcript) {
	select {
	case s.out <- t:
	case <-s.ctx.Done():
	}
}

// onConnLost handles a ReadMessage error from a connection generation. If
// the session was closed intentionally (Close() already ran), this is
// expected and silently ignored. Otherwise it drops the stale connection so
// the next PushAudio call reconnects; this is the "basic, documented"
// mid-stream reconnect behavior described on DeepgramRecognizer.
func (s *deepgramSession) onConnLost(gen int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || gen != s.gen {
		return // stale goroutine from a superseded/closed connection.
	}
	_ = err // the specific reason isn't surfaced to callers; PushAudio's
	// next reconnect attempt (or its own failure) is what callers observe.
	s.conn = nil
}

// keepAliveLoop sends a {"type":"KeepAlive"} text frame every
// dgKeepAliveInterval so Deepgram does not close the connection with
// NET-0001 during pauses in audio (e.g. caller silence). It exits once the
// session is closed or a newer connection generation supersedes it.
func (s *deepgramSession) keepAliveLoop(conn *websocket.Conn, gen int) {
	defer s.workerWG.Done()
	ticker := time.NewTicker(dgKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			stale := s.closed || gen != s.gen
			s.mu.Unlock()
			if stale {
				return
			}
			s.writeMu.Lock()
			err := conn.WriteJSON(map[string]string{"type": "KeepAlive"})
			s.writeMu.Unlock()
			if err != nil {
				return // readLoop will observe the same failure and reconnect.
			}
		}
	}
}

// failAndClose marks the session permanently closed due to a fatal,
// non-recoverable error (e.g. malformed vendor error frame, exceeded
// reconnect attempts) and tears it down the same way Close() would.
func (s *deepgramSession) failAndClose(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.fatalErr = err
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	s.cancel()

	// failAndClose can be invoked from inside a worker goroutine itself
	// (readLoop, on a fatal vendor error frame). Waiting on workerWG
	// synchronously here would deadlock in that case: the calling
	// goroutine is a member of workerWG and hasn't called Done() yet
	// because it hasn't returned from this call. Finish the teardown
	// (draining in-flight sends and closing `out`) in a separate
	// goroutine instead, once every worker -- including this call's
	// eventual caller -- actually exits.
	go func() {
		s.workerWG.Wait()
		s.sendWG.Wait()
		close(s.out)
	}()
}

// Close implements StreamSession. It is idempotent and safe to call
// concurrently with PushAudio.
func (s *deepgramSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()

	if conn != nil {
		// Best-effort: ask Deepgram to flush remaining audio and return
		// final results before we tear down the socket. Errors here are
		// not fatal to Close() since we're shutting down regardless.
		s.writeMu.Lock()
		_ = conn.WriteJSON(map[string]string{"type": "CloseStream"})
		s.writeMu.Unlock()
		_ = conn.Close()
	}

	s.cancel()
	s.workerWG.Wait()
	s.sendWG.Wait()
	close(s.out)
	return nil
}

// Transcripts implements StreamSession.
func (s *deepgramSession) Transcripts() <-chan Transcript {
	return s.out
}

var _ StreamSession = (*deepgramSession)(nil)
