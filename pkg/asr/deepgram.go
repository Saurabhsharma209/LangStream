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
	"errors"
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

	// defaultDeepgramHindiModel is the model used for Hindi
	// (languageHint == "hi") sessions -- deliberately NOT nova-2. See
	// DeepgramRecognizer's and SupportedLanguages' doc comments for the
	// full reasoning: this repo's Hindi calls are "Hinglish" (callers
	// code-switch between Hindi and English mid-sentence -- see
	// pkg/asr/sarvam.go's doc comment and DEVLOG.md's 2026-07-14 entry
	// for a live-verified example), and Deepgram's real-time
	// code-switching support is model- and language-pair-specific:
	// Nova-2's only documented code-switching pair is Spanish+English
	// (its "multi" wire-language mode), not Hindi+English, so Nova-2
	// cannot handle Hinglish speech even with language=hi. Nova-3 added
	// real-time code-switching across 10 languages including Hindi,
	// engaged via the wire-level language=multi parameter (verified
	// 2026-07-22 against
	// https://developers.deepgram.com/docs/multilingual-code-switching
	// and https://developers.deepgram.com/docs/models-languages-overview:
	// "Nova-3 supports the ability to transcribe codeswitching
	// conversations in real-time between 10 languages -- English,
	// Spanish, French, German, Hindi, Russian, Portuguese, Japanese,
	// Italian, and Dutch" using the `multi` language code, versus
	// Nova-2's `multi` being Spanish+English only). So Hindi sessions use
	// nova-3 with wireLanguage "multi", never nova-2 with language "hi".
	defaultDeepgramHindiModel = "nova-3"

	// dgKeepAliveInterval must stay comfortably under Deepgram's documented
	// 10-second NET-0001 idle timeout.
	dgKeepAliveInterval = 5 * time.Second

	dgReconnectBase = 250 * time.Millisecond
	dgReconnectMax  = 5 * time.Second

	// dgCodeSwitchEndpointingMS is the endpointing window (in
	// milliseconds) used for code-switching ("multi" wire-language)
	// sessions. Deepgram's docs recommend a tighter endpointing value
	// than the connection default for code-switching sessions so a
	// language-switch boundary is flushed promptly rather than absorbed
	// into a longer default pause window (see
	// https://developers.deepgram.com/docs/multilingual-code-switching,
	// reviewed 2026-07-22).
	dgCodeSwitchEndpointingMS = "100"

	// deepgramCostPerMinuteUSD approximates Deepgram's published
	// Pay-As-You-Go pricing for Nova-2 streaming transcription
	// (~$0.0059 per audio-minute processed, per deepgram.com/pricing as
	// reviewed while writing this). This is for pilot cost-visibility
	// only, not billing-grade accuracy -- Deepgram's actual rates vary by
	// plan/commitment and change over time, and this value is not read
	// live from any API.
	deepgramCostPerMinuteUSD = 0.0059

	// deepgramHindiCostPerMinuteUSD approximates Deepgram's published
	// Nova-3 multilingual (`multi`) streaming rate (~$0.0058 per
	// audio-minute as of 2026-07-22, see https://deepgram.com/pricing and
	// https://convertaudiototext.com/blog/deepgram-nova-3-explained's
	// summary of it -- Nova-3 monolingual streaming is pricier, around
	// $0.0077/min, but Hindi sessions always use the cheaper
	// `multi`-mode rate since that's the wire-language they connect
	// with). Same pilot-cost-visibility-only caveats as
	// deepgramCostPerMinuteUSD above.
	deepgramHindiCostPerMinuteUSD = 0.0058
)

// DeepgramOption configures a DeepgramRecognizer.
type DeepgramOption func(*DeepgramRecognizer)

// WithBaseURL overrides the Deepgram WebSocket endpoint. Intended for tests,
// which point this at a local httptest/WebSocket server instead of the real
// wss://api.deepgram.com/v1/listen.
func WithBaseURL(u string) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.baseURL = u }
}

// WithDeepgramModel overrides the Deepgram model query parameter used for
// English ("en") sessions (default "nova-2"). It has no effect on Hindi
// ("hi") sessions -- see WithDeepgramHindiModel for that.
func WithDeepgramModel(model string) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.model = model }
}

// WithDeepgramHindiModel overrides the Deepgram model used for Hindi
// ("hi") sessions (default "nova-3"; see defaultDeepgramHindiModel's doc
// comment for why Hindi does not default to nova-2). Exists mainly for
// tests; production code should rarely need this, since nova-3 is
// currently the only Deepgram model verified to support real-time
// Hindi-English code-switching.
func WithDeepgramHindiModel(model string) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.hindiModel = model }
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
// This same recorder is reused (not duplicated) to tag circuit-breaker
// rejections; see WithCircuitBreaker.
func WithMetrics(m *observability.LatencyRecorder) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.metrics = m }
}

// WithCircuitBreaker overrides this recognizer's circuit-breaker
// threshold/cooldown; non-positive values fall back to this package's
// defaults (see circuitbreaker.go). The breaker trips after `threshold`
// consecutive sessions whose very first connect attempt never
// succeeded, then fails fast (StartStream returns an error wrapping
// ErrCircuitOpen, with zero dial attempts) for `cooldown` before letting
// exactly one probe session through. A circuit breaker is always active,
// even without this option (see NewDeepgramRecognizer's default).
func WithCircuitBreaker(threshold int, cooldown time.Duration) DeepgramOption {
	return func(r *DeepgramRecognizer) { r.breaker = newCircuitBreaker(threshold, cooldown) }
}

// DeepgramRecognizer is a real streaming ASR backend backed by Deepgram's
// live transcription WebSocket API. It supports two language hints:
//
//   - "en": Nova-2 (see defaultDeepgramModel), Deepgram's general
//     English streaming model, connected with wire-language "en".
//   - "hi": Nova-3 (see defaultDeepgramHindiModel), connected with the
//     wire-level "language=multi" parameter rather than "language=hi".
//     This is deliberate, not a shortcut or an oversight: this repo's
//     Hindi calls are "Hinglish" -- callers code-switch between Hindi
//     and English mid-sentence (see pkg/asr/sarvam.go's doc comment and
//     DEVLOG.md's 2026-07-14 entry for a live-verified example utterance
//     ("...अपना order वापस चाहिए...") where "order" is a mid-sentence
//     English word). Deepgram's real-time code-switching support is
//     model- and language-pair-specific: Nova-2's only documented
//     code-switching pair is Spanish+English, so model=nova-2 with
//     language=hi would transcribe Hindi speech in isolation but handle
//     mid-utterance English words badly -- exactly the case this
//     product needs Hindi calls to handle well. Nova-3's "multi" wire
//     mode supports real-time code-switching across 10 languages
//     including Hindi (verified 2026-07-22 against
//     https://developers.deepgram.com/docs/multilingual-code-switching
//     and https://developers.deepgram.com/docs/models-languages-overview),
//     so that pair (model=nova-3, language=multi) is what a "hi"
//     languageHint connects with. See defaultDeepgramHindiModel's doc
//     comment for the sourcing detail.
type DeepgramRecognizer struct {
	apiKey               string
	baseURL              string
	model                string // English ("en") sessions; see WithDeepgramModel.
	hindiModel           string // Hindi ("hi") sessions; see WithDeepgramHindiModel.
	maxReconnectAttempts int
	dialer               *websocket.Dialer
	metrics              *observability.LatencyRecorder
	breaker              *circuitBreaker
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
		hindiModel:           defaultDeepgramHindiModel,
		maxReconnectAttempts: 3,
		dialer:               websocket.DefaultDialer,
		breaker:              newCircuitBreaker(0, 0),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Name implements Recognizer.
func (r *DeepgramRecognizer) Name() string { return "deepgram" }

// SupportedLanguages implements Recognizer. English ("en", Nova-2) and
// Hindi ("hi", Nova-3 connected with wire-level language=multi for
// Hindi-English code-switching) are both supported -- see
// DeepgramRecognizer's doc comment for why "hi" does not simply mean
// "nova-2 with language=hi".
func (r *DeepgramRecognizer) SupportedLanguages() []Language {
	return []Language{"en", "hi"}
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

	// model/wireLanguage pick the actual Deepgram model and wire-level
	// "language" query param for this session. See DeepgramRecognizer's
	// doc comment for why "hi" maps to (nova-3, "multi") rather than
	// (nova-3, "hi") or (nova-2, "hi").
	var model, wireLanguage string
	switch lang {
	case "en":
		model = r.model
		wireLanguage = "en"
	case "hi":
		model = r.hindiModel
		wireLanguage = "multi"
	default:
		return nil, fmt.Errorf("asr/deepgram: unsupported language %q (this backend supports \"en\" and \"hi\" only)", lang)
	}

	// Circuit breaker gate: if too many consecutive sessions have failed
	// to ever establish their initial connection, fail fast here with
	// zero dial attempts rather than constructing a session that would
	// only pay the full ensureConnected dial-and-backoff cost before
	// failing anyway. See circuitbreaker.go.
	if !r.breaker.allow() {
		if r.metrics != nil {
			r.metrics.RecordErrorReason("asr_connect", r.Name(), "circuit_open")
		}
		return nil, fmt.Errorf("asr/deepgram: %w", ErrCircuitOpen)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	s := &deepgramSession{
		r:            r,
		lang:         lang,
		model:        model,
		wireLanguage: wireLanguage,
		ctx:          sessCtx,
		cancel:       cancel,
		out:          make(chan Transcript, 64),
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

	// model/wireLanguage are the Deepgram model and wire-level "language"
	// query param this session connects with, resolved once in
	// StartStream from lang (see DeepgramRecognizer's doc comment):
	// ("en" -> r.model, "en") or ("hi" -> r.hindiModel, "multi").
	model        string
	wireLanguage string

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

	// connectedOnce/breakerSettled track this session's relationship with
	// r.breaker (see circuitbreaker.go). connectedOnce becomes true the
	// first time this session establishes a live connection; once true,
	// this session never touches the breaker again (mid-stream drops and
	// reconnects via the reconnectBackoff path are not the breaker's
	// concern). breakerSettled becomes true exactly once, when this
	// session's initial-connect outcome (success, failure, or an
	// unresolved abort at teardown) has been reported to the breaker via
	// recordSuccess/recordFailure/abort -- it guards against
	// double-reporting from concurrent teardown paths (failAndClose vs.
	// Close).
	connectedOnce  bool
	breakerSettled bool

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
	// Hindi sessions connect with wire-language "multi" (Nova-3
	// code-switching -- see DeepgramRecognizer's doc comment) which
	// Deepgram bills at its multilingual streaming rate, distinct from
	// the plain Nova-2 English rate.
	rate := deepgramCostPerMinuteUSD
	if s.wireLanguage == "multi" {
		rate = deepgramHindiCostPerMinuteUSD
	}
	samples := len(frame.PCM) / 2 // 16-bit mono PCM
	minutes := float64(samples) / float64(frame.SampleRate) / 60.0
	s.r.metrics.RecordCost("deepgram", minutes*rate)
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
	// This is the session's very first successful connection iff it has
	// never connected before and the breaker outcome for this session
	// hasn't already been reported (guards against a narrow race with a
	// concurrent Close()/failAndClose() settling the breaker first; see
	// settleBreaker). Mid-stream reconnects (connectedOnce already true)
	// never touch the breaker.
	firstConnect := !s.connectedOnce && !s.breakerSettled
	if firstConnect {
		s.connectedOnce = true
		s.breakerSettled = true
	}
	s.mu.Unlock()

	if firstConnect {
		s.r.breaker.recordSuccess()
	}

	s.workerWG.Add(2)
	go s.readLoop(conn, gen)
	go s.keepAliveLoop(conn, gen)
	return nil
}

// settleBreaker reports this session's initial-connect outcome to
// r.breaker exactly once (idempotent via breakerSettled): recordFailure
// if this session never managed to connect and the given err is a
// genuine give-up (not a ctx cancellation), abort if the session never
// connected but ended without a definitive vendor-health verdict (ctx
// cancelled, or torn down gracefully before ever attempting to connect),
// or a no-op if the session already connected at least once (that
// outcome was already reported as a success by ensureConnected, and any
// later mid-stream failure/close is not the breaker's concern). Called
// from both failAndClose and Close so it fires exactly once regardless
// of which teardown path actually runs for a given session (the other is
// always a no-op once s.closed is set).
func (s *deepgramSession) settleBreaker(err error) {
	s.mu.Lock()
	if s.breakerSettled {
		s.mu.Unlock()
		return
	}
	s.breakerSettled = true
	connectedOnce := s.connectedOnce
	s.mu.Unlock()

	if connectedOnce {
		return
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.r.breaker.abort()
		return
	}
	s.r.breaker.recordFailure()
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
	q.Set("language", s.wireLanguage)
	q.Set("model", s.model)
	q.Set("interim_results", "true")
	q.Set("punctuate", "true")
	q.Set("vad_events", "true")
	if s.wireLanguage == "multi" {
		// See dgCodeSwitchEndpointingMS's doc comment: Deepgram
		// recommends a tighter endpointing window for code-switching
		// sessions.
		q.Set("endpointing", dgCodeSwitchEndpointingMS)
	}
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

	s.settleBreaker(err)

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

	s.settleBreaker(nil)

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
