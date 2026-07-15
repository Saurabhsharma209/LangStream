// ElevenLabs is a real-time speech-synthesis vendor. This file implements
// Synthesizer against ElevenLabs' streaming text-to-speech HTTP API.
//
// Unlike Cartesia's and Sarvam's protocols elsewhere in this package
// (which were built from published docs and best-effort inference), the
// following was verified live against the real ElevenLabs API with a real
// API key on 2026-07-14, not guessed from docs:
//
//   - Endpoint: POST https://api.elevenlabs.io/v1/text-to-speech/{voice_id}/stream?output_format=pcm_8000
//   - Headers: "xi-api-key: <key>", "Content-Type: application/json".
//   - Request body (JSON): {"text": "...", "model_id": "eleven_multilingual_v2",
//     "language_code": "hi"} -- language_code is optional (ISO 639-1); it
//     is omitted entirely for the zero-value Language rather than sent as
//     "en", though sending "en"/"hi" explicitly also works.
//   - Response: on success, HTTP 200 with a raw, headerless,
//     chunked-transfer-encoded stream of 16-bit little-endian PCM mono
//     samples at 8000 Hz -- no WAV header, no JSON wrapper, no
//     base64, no per-message framing (confirmed by fetching pcm_8000 for
//     a test sentence and inspecting the raw bytes). The response body is
//     simply read incrementally in bounded chunks (elevenlabsReadBufBytes,
//     200ms @ 8kHz/16-bit mono) and each read becomes one AudioChunk, with
//     the last one (at EOF) marked IsFinal. 8000 Hz was chosen deliberately
//     to match this repo's telephony/RTP convention (see
//     cartesiaDefaultSampleRate's doc comment), so no resampling is needed
//     downstream.
//   - On a non-200 response, the response body (ElevenLabs returns a JSON
//     error object) is read and included in the error SynthesizeStream
//     returns; no channel is ever handed back for a failed connect, the
//     same synchronous-connect-then-async-stream contract as Cartesia's
//     SynthesizeStream.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exotel/langstream/pkg/observability"
)

// elevenlabsDefaultBaseURL is ElevenLabs' production HTTP API host. Tests
// override this via WithElevenLabsBaseURL to point at a local
// httptest.Server.
const elevenlabsDefaultBaseURL = "https://api.elevenlabs.io"

// elevenlabsDefaultModel is the model_id used for generation requests.
// "eleven_multilingual_v2" is confirmed to speak both English and Hindi
// through either voice used by this backend (see elevenlabs_voices.go).
const elevenlabsDefaultModel = "eleven_multilingual_v2"

// elevenlabsDefaultSampleRate matches the 8kHz telephony convention this
// repo's other TTS backends use (see cartesiaDefaultSampleRate), so
// downstream RTP packetization doesn't need to resample. This value also
// selects ElevenLabs' output_format=pcm_8000 query parameter.
const elevenlabsDefaultSampleRate = 8000

// elevenlabsDefaultDialTimeout bounds how long establishing the HTTP
// connection and receiving response headers from ElevenLabs may take
// before SynthesizeStream gives up, independent of any deadline already
// on the caller's ctx.
const elevenlabsDefaultDialTimeout = 10 * time.Second

// elevenlabsReadBufBytes is the size of each read from the streaming
// response body: 200ms of 16-bit mono PCM at elevenlabsDefaultSampleRate
// (8000 samples/sec * 0.2s * 2 bytes/sample = 3200 bytes), i.e. one
// AudioChunk roughly every 200ms as bytes arrive.
const elevenlabsReadBufBytes = 3200

// elevenlabsMaxAttempts caps how many times SynthesizeStream's
// request/status-check phase will retry a transient failure (HTTP
// 429/5xx, or a generic connection error): one initial try plus up to two
// retries. Kept small since TTS sits on the live-call critical path and a
// tight latency budget matters more than squeezing out one more retry.
const elevenlabsMaxAttempts = 3

// elevenlabsRetryBaseDelay / elevenlabsRetryMaxDelay bound the
// capped-exponential backoff (see retryBackoff) applied between retry
// attempts.
const (
	elevenlabsRetryBaseDelay = 150 * time.Millisecond
	elevenlabsRetryMaxDelay  = 1200 * time.Millisecond
)

// elevenlabsCostPerCharUSD approximates ElevenLabs' published pay-as-you-
// go/subscription character pricing (roughly $0.00018/character --
// derived from tiers such as the Creator plan's ~100,000 credits for
// $22/mo, where one credit is approximately one character for the
// eleven_multilingual_v2 model used here -- per elevenlabs.io/pricing as
// reviewed while writing this). ElevenLabs bills per character of input
// text, not per second of audio produced, so this is charged against
// len(text) once ElevenLabs has accepted the request (HTTP 200; see
// SynthesizeStream). This is for pilot cost-visibility only, not
// billing-grade accuracy -- ElevenLabs' actual rates vary by plan/
// model/commitment and change over time, and this value is not read live
// from any API.
const elevenlabsCostPerCharUSD = 0.00018

// ElevenLabsSynthesizer implements Synthesizer against ElevenLabs'
// streaming text-to-speech HTTP API. Each call to SynthesizeStream issues
// its own HTTP request and reads the streamed response body on its own
// goroutine; there is no shared mutable connection state to guard against
// concurrent SynthesizeStream calls racing each other.
type ElevenLabsSynthesizer struct {
	apiKey      string
	baseURL     string
	modelID     string
	sampleRate  int
	dialTimeout time.Duration
	httpClient  *http.Client
	metrics     *observability.LatencyRecorder
}

// ElevenLabsOption configures an ElevenLabsSynthesizer at construction
// time.
type ElevenLabsOption func(*ElevenLabsSynthesizer)

// WithElevenLabsBaseURL overrides ElevenLabs' production API host
// (https://api.elevenlabs.io). Tests use this to point the client at a
// local httptest.Server, e.g. WithElevenLabsBaseURL("http://127.0.0.1:12345").
func WithElevenLabsBaseURL(url string) ElevenLabsOption {
	return func(e *ElevenLabsSynthesizer) { e.baseURL = strings.TrimRight(url, "/") }
}

// WithElevenLabsModel overrides the model_id used for generation
// requests. Defaults to elevenlabsDefaultModel.
func WithElevenLabsModel(modelID string) ElevenLabsOption {
	return func(e *ElevenLabsSynthesizer) { e.modelID = modelID }
}

// WithElevenLabsSampleRate overrides the PCM sample rate requested from
// ElevenLabs (via the output_format=pcm_<rate> query parameter) and
// reported on every AudioChunk. Defaults to elevenlabsDefaultSampleRate
// (8kHz, matching telephony/RTP convention elsewhere in this repo).
func WithElevenLabsSampleRate(hz int) ElevenLabsOption {
	return func(e *ElevenLabsSynthesizer) { e.sampleRate = hz }
}

// WithElevenLabsDialTimeout overrides how long connecting to ElevenLabs
// and receiving response headers may take before SynthesizeStream fails
// with a timeout error. Defaults to elevenlabsDefaultDialTimeout. A value
// <= 0 disables the extra timeout (the call still respects ctx's own
// deadline/cancellation).
func WithElevenLabsDialTimeout(d time.Duration) ElevenLabsOption {
	return func(e *ElevenLabsSynthesizer) { e.dialTimeout = d }
}

// WithElevenLabsMetrics wires a shared *observability.LatencyRecorder
// into this synthesizer so every successfully-accepted SynthesizeStream
// call attributes its cost (see RecordCost) to the "elevenlabs" vendor,
// per elevenlabsCostPerCharUSD. Optional -- a nil/unset recorder (the
// default) makes cost recording a no-op, matching this package's
// existing functional-options convention (WithElevenLabsBaseURL,
// WithElevenLabsModel, ...).
func WithElevenLabsMetrics(m *observability.LatencyRecorder) ElevenLabsOption {
	return func(e *ElevenLabsSynthesizer) { e.metrics = m }
}

// NewElevenLabsSynthesizer constructs an ElevenLabsSynthesizer, reading
// the API key from the ELEVENLABS_API_KEY environment variable. It
// returns an error if that variable is unset or empty, since every
// ElevenLabs call requires it.
func NewElevenLabsSynthesizer(opts ...ElevenLabsOption) (*ElevenLabsSynthesizer, error) {
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("tts/elevenlabs: ELEVENLABS_API_KEY environment variable is not set")
	}

	e := &ElevenLabsSynthesizer{
		apiKey:      apiKey,
		baseURL:     elevenlabsDefaultBaseURL,
		modelID:     elevenlabsDefaultModel,
		sampleRate:  elevenlabsDefaultSampleRate,
		dialTimeout: elevenlabsDefaultDialTimeout,
	}
	for _, opt := range opts {
		opt(e)
	}
	// Built after opts are applied so a WithElevenLabsDialTimeout override
	// takes effect. The timeout only bounds connecting (TCP dial + TLS
	// handshake) and waiting for response headers -- deliberately *not*
	// the whole request context, since that would also abort an
	// in-progress body read the moment it fired (see SynthesizeStream,
	// which uses the caller's ctx directly for the request so a long-
	// running stream isn't cut short by this same timeout).
	e.httpClient = e.newHTTPClient()
	return e, nil
}

// newHTTPClient builds an *http.Client whose Transport bounds only the
// connect/TLS-handshake/response-headers phase to e.dialTimeout (when
// positive); it never bounds reading the response body, which is governed
// solely by the ctx passed to SynthesizeStream.
func (e *ElevenLabsSynthesizer) newHTTPClient() *http.Client {
	transport := &http.Transport{}
	if e.dialTimeout > 0 {
		dialer := &net.Dialer{Timeout: e.dialTimeout}
		transport.DialContext = dialer.DialContext
		transport.TLSHandshakeTimeout = e.dialTimeout
		transport.ResponseHeaderTimeout = e.dialTimeout
	}
	return &http.Client{Transport: transport}
}

// Name implements Synthesizer.
func (e *ElevenLabsSynthesizer) Name() string { return "elevenlabs" }

// SupportedLanguages implements Synthesizer.
func (e *ElevenLabsSynthesizer) SupportedLanguages() []Language {
	return []Language{LanguageEnglish, LanguageHindi}
}

func (e *ElevenLabsSynthesizer) supports(lang Language) bool {
	for _, l := range e.SupportedLanguages() {
		if l == lang {
			return true
		}
	}
	return false
}

// elevenlabsRequest is the JSON body sent to the
// /v1/text-to-speech/{voice_id}/stream endpoint.
type elevenlabsRequest struct {
	Text         string `json:"text"`
	ModelID      string `json:"model_id"`
	LanguageCode string `json:"language_code,omitempty"`
}

// elevenlabsErrorBody is a best-effort decode of ElevenLabs' JSON error
// object on non-200 responses, used only to produce a readable error
// message; the raw body is always included verbatim as well in case the
// shape doesn't match (e.g. a gateway/proxy error page).
type elevenlabsErrorBody struct {
	Detail interface{} `json:"detail"`
}

// requestOnce performs one attempt of SynthesizeStream's request/status-
// check phase: build and send the HTTP request, and validate the status
// code. On success it returns the live response (caller owns closing its
// body); on failure it always closes any response body it opened.
func (e *ElevenLabsSynthesizer) requestOnce(ctx context.Context, url string, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("tts/elevenlabs: building request: %w", err)
	}
	httpReq.Header.Set("xi-api-key", e.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tts/elevenlabs: connecting: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, &elevenlabsStatusError{
			StatusCode: resp.StatusCode,
			err:        fmt.Errorf("tts/elevenlabs: unexpected status %d: %s", resp.StatusCode, describeElevenLabsError(body)),
		}
	}
	return resp, nil
}

// elevenlabsStatusError records the HTTP status code a request failed
// with, so SynthesizeStream's retry loop can classify it (429/5xx are
// transient and worth retrying; other 4xx -- bad auth, bad request, ...
// -- are permanent client errors a retry cannot fix) without parsing the
// formatted error string.
type elevenlabsStatusError struct {
	StatusCode int
	err        error
}

func (e *elevenlabsStatusError) Error() string { return e.err.Error() }
func (e *elevenlabsStatusError) Unwrap() error { return e.err }

// isRetryableElevenLabsErr classifies an error from requestOnce: a
// response with an explicit status code is retryable only for 429/5xx
// (see isRetryableStatusCode); anything else (a connection-level failure
// with no status code at all) is treated as a transient connection
// problem (see isRetryableConnErr).
func isRetryableElevenLabsErr(err error) bool {
	var statusErr *elevenlabsStatusError
	if errors.As(err, &statusErr) {
		return isRetryableStatusCode(statusErr.StatusCode)
	}
	return isRetryableConnErr(err)
}

// SynthesizeStream implements Synthesizer. It issues one streaming HTTP
// request to ElevenLabs for the full text and streams the raw PCM bytes
// of the response body back on the returned channel, chunked into
// elevenlabsReadBufBytes-sized pieces as they arrive. The channel is
// closed once the response body reaches EOF (the last chunk before that
// has IsFinal=true), on any request/transport error, or when ctx is
// cancelled -- whichever comes first.
//
// The request/status-check phase (not the subsequent body streaming) is
// retried up to elevenlabsMaxAttempts times with capped exponential
// backoff (see retryBackoff) on transient failures -- HTTP 429/5xx, or a
// generic connection error indistinguishable from a network blip at this
// layer. Any other non-200 status (bad auth, bad request, ...) fails
// fast on the first attempt.
func (e *ElevenLabsSynthesizer) SynthesizeStream(ctx context.Context, text string, persona Persona) (<-chan AudioChunk, error) {
	if text == "" {
		return nil, fmt.Errorf("tts/elevenlabs: empty text")
	}

	lang := persona.Language
	if lang == "" {
		lang = LanguageEnglish
	}
	if !e.supports(lang) {
		return nil, fmt.Errorf("tts/elevenlabs: unsupported language %q", lang)
	}

	reqBody := elevenlabsRequest{
		Text:    text,
		ModelID: e.modelID,
	}
	if persona.Language != "" {
		reqBody.LanguageCode = string(lang)
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("tts/elevenlabs: encoding request: %w", err)
	}

	// ctx (not a derived/shorter-lived context) governs the whole
	// request, including streaming the response body back on the
	// returned channel -- see newHTTPClient's doc comment for why the
	// dial timeout is applied at the Transport level instead.
	url := fmt.Sprintf("%s/v1/text-to-speech/%s/stream?output_format=pcm_%d", e.baseURL, e.voiceFor(persona), e.sampleRate)

	var (
		resp    *http.Response
		lastErr error
	)
	for attempt := 0; attempt < elevenlabsMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBackoff(attempt-1, elevenlabsRetryBaseDelay, elevenlabsRetryMaxDelay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}

		resp, lastErr = e.requestOnce(ctx, url, payload)
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isRetryableElevenLabsErr(lastErr) || attempt == elevenlabsMaxAttempts-1 {
			return nil, lastErr
		}
	}

	// ElevenLabs bills per character of input text once it has accepted
	// the request (HTTP 200), regardless of whether the subsequent audio
	// stream later fails mid-way -- see elevenlabsCostPerCharUSD's doc
	// comment.
	if e.metrics != nil {
		e.metrics.RecordCost("elevenlabs", float64(len(text))*elevenlabsCostPerCharUSD)
	}

	out := make(chan AudioChunk, 4)
	go e.readLoop(ctx, resp.Body, out)
	return out, nil
}

// describeElevenLabsError formats a non-200 response body for inclusion
// in a returned error: it tries to decode ElevenLabs' {"detail": ...}
// JSON error shape for readability, but always falls back to the raw body
// verbatim so no information is silently dropped if the shape doesn't
// match.
func describeElevenLabsError(body []byte) string {
	var e elevenlabsErrorBody
	if err := json.Unmarshal(body, &e); err == nil && e.Detail != nil {
		if detail, mErr := json.Marshal(e.Detail); mErr == nil {
			return string(detail)
		}
	}
	return string(body)
}

// readLoop reads body incrementally in elevenlabsReadBufBytes-sized
// pieces until EOF, a read error occurs, or ctx is cancelled, sending each
// piece of data as soon as it's read (no buffering/lookahead), so the
// first audio makes it onto the channel with minimal added latency. It
// always closes out and body before returning.
//
// Determining which chunk is truly last is subtler than "the read that
// returns io.EOF": io.Reader implementations, *http.Response.Body
// included, are free to signal EOF either by attaching it to the same
// call that returned the final data (n > 0, err == io.EOF) or by a
// separate, later call that returns no data at all (n == 0, err ==
// io.EOF) -- which one happens is a race against how much of the
// underlying TCP stream (including the chunked-encoding terminator) has
// arrived by the time the read is issued, and this implementation has
// observed both in practice against a local test server. Both cases are
// handled: when EOF arrives attached to real data, that chunk is sent
// with IsFinal=true; when EOF arrives on its own after the last real
// chunk was already sent with IsFinal=false, a final zero-length sentinel
// chunk (IsFinal=true, empty PCM) is sent so callers always see exactly
// one IsFinal=true chunk marking successful completion, per this
// backend's documented contract, without ever delaying delivery of real
// audio to find out.
//
// Note on error surfacing: Synthesizer.SynthesizeStream's channel only
// carries AudioChunk values, so a failure discovered *after* the initial
// request succeeds (a dropped connection, a read error mid-stream) has no
// typed error to report through -- the same limitation Cartesia's
// readLoop has. This implementation's contract for that case is: stop,
// close the channel without ever sending IsFinal=true, and close the
// response body. Callers should treat "channel closed but the last chunk
// observed had IsFinal == false (or no chunk arrived at all)" as a failed
// synthesis.
func (e *ElevenLabsSynthesizer) readLoop(ctx context.Context, body io.ReadCloser, out chan<- AudioChunk) {
	defer close(out)
	defer body.Close()

	// If ctx is cancelled while we're blocked in a read, force it to
	// unblock by closing the response body out from under it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			body.Close()
		case <-stop:
		}
	}()

	send := func(pcm []byte, isFinal bool) bool {
		select {
		case out <- AudioChunk{PCM: pcm, SampleRate: e.sampleRate, IsFinal: isFinal}:
			return true
		case <-ctx.Done():
			return false
		}
	}

	buf := make([]byte, elevenlabsReadBufBytes)
	for {
		if ctx.Err() != nil {
			return
		}

		n, err := body.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if !send(data, err == io.EOF) {
				return
			}
		}

		if err != nil {
			if err == io.EOF && n == 0 {
				// EOF arrived with no data of its own, and (if any real
				// audio existed) it was already sent above with
				// IsFinal=false since it wasn't this call. Send the
				// sentinel so exactly one IsFinal=true chunk always
				// marks successful completion.
				send(nil, true)
			}
			// Any other error -- including one caused by ctx
			// cancellation closing body -- means stop without ever
			// sending a final chunk, per this method's documented
			// contract.
			return
		}
	}
}

var _ Synthesizer = (*ElevenLabsSynthesizer)(nil)
