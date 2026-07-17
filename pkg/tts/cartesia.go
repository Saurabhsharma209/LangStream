// Cartesia is a real-time speech-synthesis vendor. This file implements
// Synthesizer against Cartesia's streaming TTS WebSocket API
// (wss://api.cartesia.ai/tts/websocket), confirmed against
// https://docs.cartesia.ai/api-reference/tts/tts as of this writing:
//
//   - Auth: "X-API-Key: <key>" header plus a "Cartesia-Version: YYYY-MM-DD"
//     header (there is also a query-param access-token flow, but that's
//     for browser clients that can't set WebSocket headers; this is a
//     server-side Go client, so it uses the header form).
//   - Request (client -> server, one JSON text frame per generation):
//     {"model_id", "transcript", "voice": {"mode": "id", "id": "..."},
//     "language", "context_id", "output_format": {"container": "raw",
//     "encoding": "pcm_s16le", "sample_rate": 8000}, "continue": false}.
//   - Response (server -> client, one JSON text frame per message):
//     {"type": "chunk", "data": "<base64 PCM>", "done": bool,
//     "context_id": ...} for audio, or {"type": "done", ...} once
//     generation is complete, or {"type": "error", "title", "message",
//     "status_code", ...} on failure.
package tts

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exotel/langstream/pkg/observability"
)

// cartesiaDefaultBaseURL is Cartesia's production WebSocket endpoint host.
// Tests override this via WithBaseURL to point at a local fake server.
const cartesiaDefaultBaseURL = "wss://api.cartesia.ai"

// cartesiaDefaultAPIVersion is sent as the Cartesia-Version header/query
// parameter on every request, per Cartesia's versioned-API convention.
const cartesiaDefaultAPIVersion = "2025-04-16"

// cartesiaDefaultModel is the model_id used for generation requests.
// "sonic-2" is the model used in Cartesia's own quick-start examples.
const cartesiaDefaultModel = "sonic-2"

// cartesiaDefaultSampleRate matches the 8kHz telephony convention this
// repo's mock TTS/ASR backends already use (see pkg/tts/mock.go), so
// downstream RTP packetization doesn't need to resample.
const cartesiaDefaultSampleRate = 8000

// cartesiaDefaultDialTimeout bounds how long connecting to Cartesia (TCP +
// TLS + WebSocket handshake) may take before SynthesizeStream gives up,
// independent of any deadline already on the caller's ctx.
const cartesiaDefaultDialTimeout = 10 * time.Second

// cartesiaMaxAttempts caps how many times SynthesizeStream's connect+send
// phase will retry a transient failure (429/5xx handshake rejection, or a
// generic dial/TLS/write error): one initial try plus up to two retries.
// Kept small since TTS sits on the live-call critical path and a tight
// latency budget matters more than squeezing out one more retry.
const cartesiaMaxAttempts = 3

// cartesiaRetryBaseDelay / cartesiaRetryMaxDelay bound the capped-
// exponential backoff (see retryBackoff) applied between retry attempts.
const (
	cartesiaRetryBaseDelay = 150 * time.Millisecond
	cartesiaRetryMaxDelay  = 1200 * time.Millisecond
)

// cartesiaCostPerCharUSD approximates Cartesia's published Sonic TTS
// pay-as-you-go pricing (~$0.00004/character, i.e. roughly $40 per 1M
// characters, per cartesia.ai/pricing as reviewed while writing this).
// Cartesia bills per character of input text, not per second of audio
// produced, so this is charged against len(text) once Cartesia has
// accepted the generation request (see SynthesizeStream). This is for
// pilot cost-visibility only, not billing-grade accuracy -- Cartesia's
// actual rates vary by plan/commitment and change over time, and this
// value is not read live from any API.
const cartesiaCostPerCharUSD = 0.00004

// CartesiaSynthesizer implements Synthesizer against Cartesia's streaming
// TTS WebSocket API. Each call to SynthesizeStream opens its own
// short-lived WebSocket connection and context (Cartesia's term for one
// logical generation stream); this trades a little per-call connection
// overhead for a much simpler, stateless client with no shared mutable
// connection to guard against concurrent SynthesizeStream calls racing
// each other.
type CartesiaSynthesizer struct {
	apiKey      string
	baseURL     string
	apiVersion  string
	modelID     string
	sampleRate  int
	dialTimeout time.Duration
	metrics     *observability.LatencyRecorder
	breaker     *circuitBreaker
}

// Option configures a CartesiaSynthesizer at construction time.
type Option func(*CartesiaSynthesizer)

// WithBaseURL overrides Cartesia's production WebSocket endpoint
// (wss://api.cartesia.ai). Tests use this to point the client at a local
// fake WebSocket server, e.g. WithBaseURL("ws://127.0.0.1:12345").
func WithBaseURL(url string) Option {
	return func(c *CartesiaSynthesizer) { c.baseURL = url }
}

// WithAPIVersion overrides the Cartesia-Version header sent on every
// request. Defaults to cartesiaDefaultAPIVersion.
func WithAPIVersion(version string) Option {
	return func(c *CartesiaSynthesizer) { c.apiVersion = version }
}

// WithModel overrides the model_id used for generation requests. Defaults
// to cartesiaDefaultModel.
func WithModel(modelID string) Option {
	return func(c *CartesiaSynthesizer) { c.modelID = modelID }
}

// WithSampleRate overrides the PCM sample rate requested from Cartesia (and
// reported on every AudioChunk). Defaults to cartesiaDefaultSampleRate
// (8kHz, matching telephony/RTP convention elsewhere in this repo).
func WithSampleRate(hz int) Option {
	return func(c *CartesiaSynthesizer) { c.sampleRate = hz }
}

// WithDialTimeout overrides how long connecting to Cartesia may take
// before SynthesizeStream fails with a timeout error. Defaults to
// cartesiaDefaultDialTimeout. A value <= 0 disables the extra timeout
// (the call still respects ctx's own deadline/cancellation).
func WithDialTimeout(d time.Duration) Option {
	return func(c *CartesiaSynthesizer) { c.dialTimeout = d }
}

// WithMetrics wires a shared *observability.LatencyRecorder into this
// synthesizer so every successfully-submitted SynthesizeStream call
// attributes its cost (see RecordCost) to the "cartesia" vendor, per
// cartesiaCostPerCharUSD. Optional -- a nil/unset recorder (the default)
// makes cost recording a no-op, matching this package's existing
// functional-options convention (WithBaseURL, WithModel, ...).
func WithMetrics(m *observability.LatencyRecorder) Option {
	return func(c *CartesiaSynthesizer) { c.metrics = m }
}

// WithCircuitBreaker overrides breaker threshold/cooldown; non-positive values fall back to defaults.
func WithCircuitBreaker(threshold int, cooldown time.Duration) Option {
	return func(c *CartesiaSynthesizer) { c.breaker = newCircuitBreaker(threshold, cooldown) }
}

// NewCartesiaSynthesizer constructs a CartesiaSynthesizer, reading the API
// key from the CARTESIA_API_KEY environment variable. It returns an error
// if that variable is unset or empty, since every Cartesia call requires
// it.
func NewCartesiaSynthesizer(opts ...Option) (*CartesiaSynthesizer, error) {
	apiKey := os.Getenv("CARTESIA_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("tts/cartesia: CARTESIA_API_KEY environment variable is not set")
	}

	c := &CartesiaSynthesizer{
		apiKey:      apiKey,
		baseURL:     cartesiaDefaultBaseURL,
		apiVersion:  cartesiaDefaultAPIVersion,
		modelID:     cartesiaDefaultModel,
		sampleRate:  cartesiaDefaultSampleRate,
		dialTimeout: cartesiaDefaultDialTimeout,
		breaker:     newCircuitBreaker(0, 0),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Name implements Synthesizer.
func (c *CartesiaSynthesizer) Name() string { return "cartesia" }

// SupportedLanguages implements Synthesizer.
func (c *CartesiaSynthesizer) SupportedLanguages() []Language {
	return []Language{LanguageEnglish, LanguageHindi}
}

func (c *CartesiaSynthesizer) supports(lang Language) bool {
	for _, l := range c.SupportedLanguages() {
		if l == lang {
			return true
		}
	}
	return false
}

// cartesiaVoiceRef is the "voice" object in a generation request.
type cartesiaVoiceRef struct {
	Mode string `json:"mode"`
	ID   string `json:"id"`
}

// cartesiaOutputFormat is the "output_format" object in a generation
// request.
type cartesiaOutputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

// cartesiaGenerationRequest is the client -> server JSON message that
// requests speech synthesis for a chunk of text within one context.
type cartesiaGenerationRequest struct {
	ModelID      string               `json:"model_id"`
	Transcript   string               `json:"transcript"`
	Voice        cartesiaVoiceRef     `json:"voice"`
	Language     string               `json:"language,omitempty"`
	ContextID    string               `json:"context_id"`
	OutputFormat cartesiaOutputFormat `json:"output_format"`
	Continue     bool                 `json:"continue"`
}

// cartesiaMessage is a superset of the server -> client message shapes on
// /tts/websocket (chunk, done, error, flush_done, timestamps,
// phoneme_timestamps). Only the fields this client acts on are decoded;
// encoding/json silently ignores the rest, and fields absent for a given
// message `type` simply decode to their zero value.
type cartesiaMessage struct {
	Type       string `json:"type"`
	Data       string `json:"data"` // base64-encoded PCM, only set when Type == "chunk"
	Done       bool   `json:"done"`
	StatusCode int    `json:"status_code"`
	ContextID  string `json:"context_id"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	ErrorCode  string `json:"error_code"`
}

// isRetryableDialErr classifies an error from connectAndSend: a
// handshake explicitly rejected with a status code is retryable only for
// 429/5xx (see isRetryableStatusCode); anything else (a generic dial/TLS/
// write failure with no status code at all) is treated as a transient
// connection-level problem (see isRetryableConnErr).
func isRetryableDialErr(err error) bool {
	var statusErr *wsHandshakeStatusError
	if errors.As(err, &statusErr) {
		return isRetryableStatusCode(statusErr.StatusCode)
	}
	return isRetryableConnErr(err)
}

// connectAndSend performs one attempt of SynthesizeStream's connect
// phase: dial Cartesia's WebSocket endpoint and send one generation
// request. On success it returns the live connection/reader; on failure
// it always closes any connection it opened before returning.
func (c *CartesiaSynthesizer) connectAndSend(ctx context.Context, wsURL string, header http.Header, req cartesiaGenerationRequest) (net.Conn, *bufio.Reader, error) {
	dialCtx := ctx
	if c.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, c.dialTimeout)
		defer cancel()
	}

	conn, br, err := dialWS(dialCtx, wsURL, header)
	if err != nil {
		return nil, nil, fmt.Errorf("tts/cartesia: connecting: %w", err)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: encoding generation request: %w", err)
	}
	if err := writeWSFrame(conn, wsOpText, payload); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: sending generation request: %w", err)
	}
	return conn, br, nil
}

// SynthesizeStream implements Synthesizer. It opens a fresh WebSocket
// connection to Cartesia, sends one generation request for the full text
// (Continue: false, since callers of this interface pass complete
// utterances rather than incremental transcript chunks), and streams
// decoded PCM chunks back on the returned channel as they arrive. The
// channel is closed once Cartesia reports the context done, on any
// connection/protocol error, or when ctx is cancelled -- whichever comes
// first.
//
// The connect+send phase (not the subsequent audio stream) is retried up
// to cartesiaMaxAttempts times with capped exponential backoff (see
// retryBackoff) on transient failures -- HTTP 429/5xx handshake
// rejections, or a generic dial/TLS/write error indistinguishable from a
// network blip at this layer -- since Cartesia has no concept of
// resuming a generation that never got an ack; a retry is simply "do the
// whole handshake again" with a fresh context_id, the same shape as
// asr's Deepgram/Sarvam reconnect policy (see pkg/asr/backoff.go) but
// scoped to one SynthesizeStream call. Any other handshake rejection
// (bad auth, bad request, ...) fails fast on the first attempt.
func (c *CartesiaSynthesizer) SynthesizeStream(ctx context.Context, text string, persona Persona) (<-chan AudioChunk, error) {
	if text == "" {
		return nil, fmt.Errorf("tts/cartesia: empty text")
	}

	lang := persona.Language
	if lang == "" {
		lang = LanguageEnglish
	}
	if !c.supports(lang) {
		return nil, fmt.Errorf("tts/cartesia: unsupported language %q", lang)
	}

	if !c.breaker.allow() {
		if c.metrics != nil {
			c.metrics.RecordErrorReason("tts", c.Name(), "circuit_open")
		}
		return nil, fmt.Errorf("tts/cartesia: %w", ErrCircuitOpen)
	}
	breakerSettled := false
	defer func() {
		if !breakerSettled {
			c.breaker.abort()
		}
	}()

	header := http.Header{}
	header.Set("X-API-Key", c.apiKey)
	header.Set("Cartesia-Version", c.apiVersion)

	wsURL := fmt.Sprintf("%s/tts/websocket?cartesia_version=%s", strings.TrimRight(c.baseURL, "/"), c.apiVersion)

	var (
		conn      net.Conn
		br        *bufio.Reader
		contextID string
		lastErr   error
	)

	for attempt := 0; attempt < cartesiaMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBackoff(attempt-1, cartesiaRetryBaseDelay, cartesiaRetryMaxDelay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}

		cid, err := newCartesiaContextID()
		if err != nil {
			return nil, fmt.Errorf("tts/cartesia: generating context id: %w", err)
		}

		req := cartesiaGenerationRequest{
			ModelID:    c.modelID,
			Transcript: text,
			Voice:      cartesiaVoiceRef{Mode: "id", ID: c.voiceFor(persona)},
			Language:   string(lang),
			ContextID:  cid,
			OutputFormat: cartesiaOutputFormat{
				Container:  "raw",
				Encoding:   "pcm_s16le",
				SampleRate: c.sampleRate,
			},
			Continue: false,
		}

		conn, br, lastErr = c.connectAndSend(ctx, wsURL, header, req)
		if lastErr == nil {
			contextID = cid
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isRetryableDialErr(lastErr) || attempt == cartesiaMaxAttempts-1 {
			if isRetryableDialErr(lastErr) {
				c.breaker.recordFailure()
				breakerSettled = true
			}
			return nil, lastErr
		}
	}

	c.breaker.recordSuccess()
	breakerSettled = true

	// Cartesia bills per character of input text once it has accepted
	// the generation request, regardless of whether the subsequent audio
	// stream later fails mid-way -- see cartesiaCostPerCharUSD's doc
	// comment.
	if c.metrics != nil {
		c.metrics.RecordCost("cartesia", float64(len(text))*cartesiaCostPerCharUSD)
	}

	out := make(chan AudioChunk, 4)
	go c.readLoop(ctx, conn, br, contextID, out)
	return out, nil
}

// readLoop consumes messages for one context from conn until Cartesia
// reports completion, an error message/protocol violation occurs, the
// connection drops, or ctx is cancelled. It always closes out and conn
// before returning.
//
// Note on error surfacing: Synthesizer.SynthesizeStream's channel only
// carries AudioChunk values, so a failure discovered *after* the initial
// connection succeeds (a malformed frame, a mid-stream {"type":"error"}
// message, an unexpected disconnect) has no typed error to report through
// -- the same limitation MockSynthesizer has for context cancellation.
// This implementation's contract for that case is: stop, close the
// channel without ever sending IsFinal=true, and close the connection.
// Callers should treat "channel closed but the last chunk observed had
// IsFinal == false (or no chunk arrived at all)" as a failed synthesis.
func (c *CartesiaSynthesizer) readLoop(ctx context.Context, conn net.Conn, br *bufio.Reader, contextID string, out chan<- AudioChunk) {
	defer close(out)
	defer conn.Close()

	// If ctx is cancelled while we're blocked in a read, force it to
	// unblock by closing the connection out from under it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		opcode, data, err := readWSMessage(conn, br)
		if err != nil {
			// Either ctx cancellation forced the connection closed, the
			// peer closed normally, or a real transport error occurred;
			// in all cases there is nothing more to do but stop.
			return
		}
		if opcode != wsOpText {
			// Cartesia's protocol only sends JSON text frames; ignore
			// anything else rather than treating it as fatal.
			continue
		}

		var msg cartesiaMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			// Malformed response: stop rather than risk misinterpreting
			// garbage as audio.
			return
		}
		if msg.ContextID != "" && msg.ContextID != contextID {
			// Belongs to a different multiplexed context; not possible
			// today since each SynthesizeStream call uses its own
			// connection, but harmless to guard against.
			continue
		}

		switch msg.Type {
		case "chunk":
			pcm, decErr := base64.StdEncoding.DecodeString(msg.Data)
			if decErr != nil {
				return
			}
			chunk := AudioChunk{
				PCM:        pcm,
				SampleRate: c.sampleRate,
				IsFinal:    msg.Done,
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
			if msg.Done {
				return
			}
		case "done":
			return
		case "error":
			// msg.Title/msg.Message/msg.ErrorCode describe the failure,
			// but (see doc comment above) there is no channel of type
			// error to report them through; stop cleanly.
			return
		case "flush_done", "timestamps", "phoneme_timestamps":
			continue
		default:
			continue
		}
	}
}

// newCartesiaContextID returns a random UUIDv4-formatted string suitable
// for Cartesia's context_id field (Cartesia accepts any unique string, a
// UUID is just a convenient way to guarantee uniqueness).
func newCartesiaContextID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

var _ Synthesizer = (*CartesiaSynthesizer)(nil)
