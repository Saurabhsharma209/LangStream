// Sarvam AI streaming ASR backend for Hindi (and Hindi-English code-mixed
// speech).
//
// Protocol details verified against Sarvam's public docs as of 2026-07-08
// (https://docs.sarvam.ai/api-reference-docs/speech-to-text/transcribe/ws,
// https://docs.sarvam.ai/api-reference-docs/api-guides-tutorials/speech-to-text/streaming-api):
//
//   - Endpoint: wss://api.sarvam.ai/speech-to-text/ws (GET, 101 Switching
//     Protocols).
//   - Auth: "Api-Subscription-Key: <key>" request header.
//   - Connection query params used here: language-code, model=saaras:v3,
//     mode=codemix (Sarvam's documented mode for natural Hindi-English
//     code-switched transcription), sample_rate, input_audio_codec, and
//     vad_signals=true.
//   - Unlike Deepgram, audio is NOT sent as raw binary WS frames: Sarvam's
//     WS protocol sends audio as a JSON text frame:
//     {"audio":{"data":"<base64 PCM>","sample_rate":"16000","encoding":"..."}}.
//   - Responses are JSON text frames of shape:
//     {"type":"data","data":{"request_id":"...","transcript":"...",
//     "metrics":{"audio_duration":1.1,"processing_latency":1.1}}}.
//
// ASSUMPTIONS (Sarvam's public reference is thinner than Deepgram's; these
// are explicitly called out per the pilot's instructions rather than
// silently invented):
//
//  1. Per-message "encoding" field for the Audio Transcription Message:
//     RESOLVED 2026-07-14 against a live Sarvam endpoint (real API key,
//     real WebSocket traffic -- see DEVLOG.md's 2026-07-14 entry). The
//     original guess in this comment (encoding "pcm_s16le" to match the
//     connection-level input_audio_codec param, with raw headerless PCM as
//     the "data" payload) is WRONG: Sarvam's server rejects it outright
//     with a Pydantic validation error ("Input should be 'audio/wav'").
//     The real, verified contract is: "encoding" must always be the literal
//     string "audio/wav", and "data" must be a base64-encoded, *self-
//     contained WAV file* (RIFF/WAVE header + PCM data), not headerless
//     PCM -- confirmed both for a single-shot whole-utterance send and for
//     a real streaming session that sends many small (~400ms) WAV-wrapped
//     chunks in sequence (Sarvam correctly buffers across messages and
//     still returns one coherent final transcript for the whole
//     utterance, VAD start/end events included). This is what
//     pcm16MonoToWAV + PushAudio's msg.Audio.Encoding now implement. The
//     connection-level "input_audio_codec" query parameter is left as
//     "pcm_s16le" unchanged -- that one was never rejected, and Sarvam's
//     error was specific to the per-message field.
//  2. Flush signal shape: the reference page lists a second Send message
//     type, "Speech Flush Signal object", gated by the documented
//     flush_signal=true connection parameter, but its JSON field name is
//     collapsed behind "Show 1 properties" in the public docs and not
//     expanded anywhere else we could find. We model it as {"type":"flush"}
//     by analogy with Deepgram's {"type":"Finalize"} control message and
//     common streaming-ASR vendor convention; this is a best-effort guess,
//     not a verified wire format. We use it only as a best-effort, ignorable
//     step on Close() (its failure does not fail Close()).
//  3. Finality of "data" transcript messages: the streaming guide's Response
//     Types section documents "transcript: Final transcription result" for
//     the STT endpoint when vad_signals=true, distinct from separate
//     speech_start/speech_end events. We therefore treat every {"type":"data"}
//     message as a final Transcript (IsFinal=true) rather than trying to
//     invent an interim/partial distinction Sarvam's public docs do not
//     describe for this endpoint.
//  4. Per-segment timing: Sarvam's response includes audio_duration/
//     processing_latency metrics but no absolute start/end offsets into the
//     stream (unlike Deepgram's start/duration). We approximate StartMS/
//     EndMS by accumulating the duration of audio actually pushed via
//     PushAudio, the same technique pkg/asr/mock.go uses.
package asr

import (
	"context"
	"encoding/base64"
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
	defaultSarvamBaseURL = "wss://api.sarvam.ai/speech-to-text/ws"
	defaultSarvamModel   = "saaras:v3"

	// sarvamMode is fixed to "codemix" for this backend: the whole point of
	// this integration is code-switching-aware Hindi-English recognition,
	// per Sarvam's documented mode table (mode=codemix: "Transcribe
	// code-mixed speech (e.g., Hindi-English) naturally").
	sarvamMode = "codemix"

	sarvamReconnectBase = 250 * time.Millisecond
	sarvamReconnectMax  = 5 * time.Second

	// sarvamCostPerMinuteUSD is an ASSUMPTION, in the same spirit as the
	// numbered assumptions in this file's package doc comment: Sarvam's
	// public pricing docs are thin (no per-minute streaming STT rate
	// found alongside the API reference used to build this backend), so
	// this approximates their listed STT pricing (~INR 0.5 per
	// audio-minute, converted at a nominal ~83 INR/USD) as roughly
	// $0.006/minute. This is for pilot cost-visibility only, not
	// billing-grade accuracy, and should be replaced with a verified
	// figure once Sarvam's actual invoiced rate for this account is
	// known.
	sarvamCostPerMinuteUSD = 0.006
)

// sarvamLanguageCodes maps our internal Language tags to Sarvam's BCP-47-ish
// language-code query parameter values.
var sarvamLanguageCodes = map[Language]string{
	"hi": "hi-IN",
	"en": "en-IN",
}

// SarvamOption configures a SarvamRecognizer.
type SarvamOption func(*SarvamRecognizer)

// WithSarvamBaseURL overrides the Sarvam WebSocket endpoint. Intended for
// tests, which point this at a local fake WebSocket server instead of the
// real wss://api.sarvam.ai/speech-to-text/ws.
func WithSarvamBaseURL(u string) SarvamOption {
	return func(r *SarvamRecognizer) { r.baseURL = u }
}

// WithSarvamModel overrides the Sarvam model query parameter (default
// "saaras:v3").
func WithSarvamModel(model string) SarvamOption {
	return func(r *SarvamRecognizer) { r.model = model }
}

// WithSarvamMaxReconnectAttempts caps how many consecutive times a session
// will try to re-establish its WebSocket connection after a disconnect
// before giving up. Default 3.
func WithSarvamMaxReconnectAttempts(n int) SarvamOption {
	return func(r *SarvamRecognizer) { r.maxReconnectAttempts = n }
}

// WithSarvamDialer overrides the gorilla/websocket dialer used to connect.
func WithSarvamDialer(d *websocket.Dialer) SarvamOption {
	return func(r *SarvamRecognizer) { r.dialer = d }
}

// WithSarvamMetrics wires a shared *observability.LatencyRecorder into
// this recognizer so every audio frame successfully pushed to Sarvam
// attributes its cost (see RecordCost) to the "sarvam" vendor, per
// sarvamCostPerMinuteUSD. Optional -- a nil/unset recorder (the default)
// makes cost recording a no-op, matching this package's existing
// functional-options convention (WithSarvamBaseURL, WithSarvamDialer,
// ...).
func WithSarvamMetrics(m *observability.LatencyRecorder) SarvamOption {
	return func(r *SarvamRecognizer) { r.metrics = m }
}

// SarvamRecognizer is a real, code-switching-aware streaming ASR backend for
// Hindi (and Hindi-English code-mixed speech) backed by Sarvam AI's
// streaming speech-to-text WebSocket API.
type SarvamRecognizer struct {
	apiKey               string
	baseURL              string
	model                string
	maxReconnectAttempts int
	dialer               *websocket.Dialer
	metrics              *observability.LatencyRecorder
}

// NewSarvamRecognizer builds a Sarvam-backed Recognizer. It reads the API
// subscription key from the SARVAM_API_KEY environment variable;
// construction fails if that variable is unset or empty.
func NewSarvamRecognizer(opts ...SarvamOption) (*SarvamRecognizer, error) {
	apiKey := os.Getenv("SARVAM_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("asr/sarvam: SARVAM_API_KEY environment variable is not set")
	}
	r := &SarvamRecognizer{
		apiKey:               apiKey,
		baseURL:              defaultSarvamBaseURL,
		model:                defaultSarvamModel,
		maxReconnectAttempts: 3,
		dialer:               websocket.DefaultDialer,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Name implements Recognizer.
func (r *SarvamRecognizer) Name() string { return "sarvam" }

// SupportedLanguages implements Recognizer. Hindi is primary; English is
// supported too since code-switching is the point of this backend.
func (r *SarvamRecognizer) SupportedLanguages() []Language {
	return []Language{"hi", "en"}
}

// StartStream implements Recognizer. An empty languageHint defaults to "hi"
// (the primary language for this backend) while still requesting Sarvam's
// codemix mode, so English words embedded in Hindi speech (and vice versa)
// are transcribed naturally rather than rejected.
func (r *SarvamRecognizer) StartStream(ctx context.Context, languageHint Language) (StreamSession, error) {
	lang := languageHint
	if lang == "" {
		lang = "hi"
	}
	code, ok := sarvamLanguageCodes[lang]
	if !ok {
		return nil, fmt.Errorf("asr/sarvam: unsupported language %q", lang)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	s := &sarvamSession{
		r:            r,
		lang:         lang,
		languageCode: code,
		ctx:          sessCtx,
		cancel:       cancel,
		out:          make(chan Transcript, 64),
	}
	return s, nil
}

var _ Recognizer = (*SarvamRecognizer)(nil)

// sarvamSession implements StreamSession against a live Sarvam WebSocket
// connection. Its concurrency model mirrors deepgramSession: mu guards
// session/connection state, writeMu serializes writes to the current conn,
// and gen distinguishes goroutines belonging to superseded connections.
type sarvamSession struct {
	r            *SarvamRecognizer
	lang         Language
	languageCode string

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
	totalMS           int64 // cumulative pushed-audio duration; see assumption (4) above.

	writeMu sync.Mutex

	sendWG   sync.WaitGroup
	workerWG sync.WaitGroup
}

// PushAudio implements StreamSession.
func (s *sarvamSession) PushAudio(ctx context.Context, frame AudioFrame) error {
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
			return fmt.Errorf("asr/sarvam: session closed: %w", err)
		}
		return fmt.Errorf("asr/sarvam: session closed")
	}
	needConnect := s.conn == nil
	s.mu.Unlock()

	if needConnect {
		if err := s.ensureConnected(frame.SampleRate); err != nil {
			s.failAndClose(err)
			return err
		}
	}

	frameMS := int64(0)
	if frame.SampleRate > 0 {
		samples := len(frame.PCM) / 2 // 16-bit mono PCM.
		frameMS = int64(samples) * 1000 / int64(frame.SampleRate)
	}

	msg := sarvamAudioMessage{}
	// See assumption (1) in the package doc comment: the real Sarvam
	// endpoint requires "audio/wav" here, with "data" itself being a
	// self-contained WAV file, not headerless PCM -- verified live
	// 2026-07-14.
	msg.Audio.Data = base64.StdEncoding.EncodeToString(pcm16MonoToWAV(frame.PCM, frame.SampleRate))
	msg.Audio.SampleRate = strconv.Itoa(frame.SampleRate)
	msg.Audio.Encoding = "audio/wav"

	if err := s.writeJSON(msg); err != nil {
		if connErr := s.ensureConnected(frame.SampleRate); connErr != nil {
			s.failAndClose(connErr)
			return connErr
		}
		if err := s.writeJSON(msg); err != nil {
			s.failAndClose(err)
			return fmt.Errorf("asr/sarvam: write audio after reconnect: %w", err)
		}
	}

	s.mu.Lock()
	s.totalMS += frameMS
	s.mu.Unlock()
	s.recordAudioCost(frameMS)
	return nil
}

// recordAudioCost attributes the cost of processing one successfully
// pushed audio frame (frameMS milliseconds of audio) to the "sarvam"
// vendor, in USD. See sarvamCostPerMinuteUSD's doc comment for the
// pricing assumption. No-op if no metrics recorder was configured via
// WithSarvamMetrics.
func (s *sarvamSession) recordAudioCost(frameMS int64) {
	if s.r.metrics == nil || frameMS <= 0 {
		return
	}
	minutes := float64(frameMS) / 1000.0 / 60.0
	s.r.metrics.RecordCost("sarvam", minutes*sarvamCostPerMinuteUSD)
}

// ensureConnected dials Sarvam if there is no live connection, applying the
// shared reconnect backoff across attempts.
func (s *sarvamSession) ensureConnected(sampleRate int) error {
	s.mu.Lock()
	if s.conn != nil {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("asr/sarvam: session closed")
	}
	attempt := s.reconnectAttempts
	maxAttempts := s.r.maxReconnectAttempts
	s.mu.Unlock()

	if attempt >= maxAttempts {
		return fmt.Errorf("asr/sarvam: exceeded max reconnect attempts (%d)", maxAttempts)
	}
	if attempt > 0 {
		delay := reconnectBackoff(attempt-1, sarvamReconnectBase, sarvamReconnectMax)
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
		return fmt.Errorf("asr/sarvam: build connect URL: %w", err)
	}
	header := http.Header{}
	header.Set("Api-Subscription-Key", s.r.apiKey)

	conn, _, err := s.r.dialer.DialContext(s.ctx, wsURL, header)
	if err != nil {
		s.mu.Lock()
		s.reconnectAttempts++
		s.mu.Unlock()
		return fmt.Errorf("asr/sarvam: connect: %w", err)
	}

	s.mu.Lock()
	s.conn = conn
	s.sampleRate = sampleRate
	s.gen++
	gen := s.gen
	s.mu.Unlock()

	s.workerWG.Add(1)
	go s.readLoop(conn, gen)
	return nil
}

func (s *sarvamSession) buildURL(sampleRate int) (string, error) {
	u, err := url.Parse(s.r.baseURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("language-code", s.languageCode)
	q.Set("model", s.r.model)
	q.Set("mode", sarvamMode)
	q.Set("sample_rate", strconv.Itoa(sampleRate))
	q.Set("input_audio_codec", "pcm_s16le")
	q.Set("vad_signals", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *sarvamSession) writeJSON(v interface{}) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("asr/sarvam: not connected")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteJSON(v)
}

// pcm16MonoToWAV wraps raw 16-bit-LE mono PCM samples (the format
// pkg/asr/interface.go's AudioFrame carries) in a minimal, self-contained
// WAV (RIFF/WAVE, PCM format tag 1) container: a 44-byte header followed
// by the PCM data unchanged. Sarvam's live streaming endpoint requires
// each Audio Transcription Message's "data" field to be a real WAV file
// when "encoding" is "audio/wav" (see assumption (1) above) -- confirmed
// against a live Sarvam session both for a single whole-utterance message
// and for a sequence of many small per-chunk WAV messages in a real
// streaming PushAudio loop, so wrapping every frame this way (rather than
// only once per utterance) is the correct, verified behavior, not an
// approximation.
func pcm16MonoToWAV(pcm []byte, sampleRate int) []byte {
	const (
		numChannels   = 1
		bitsPerSample = 16
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataLen := len(pcm)

	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, "RIFF"...)
	buf = append(buf, le32(uint32(36+dataLen))...)
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = append(buf, le32(16)...) // fmt chunk size
	buf = append(buf, le16(1)...)  // audio format: 1 = PCM
	buf = append(buf, le16(numChannels)...)
	buf = append(buf, le32(uint32(sampleRate))...)
	buf = append(buf, le32(uint32(byteRate))...)
	buf = append(buf, le16(blockAlign)...)
	buf = append(buf, le16(bitsPerSample)...)
	buf = append(buf, "data"...)
	buf = append(buf, le32(uint32(dataLen))...)
	buf = append(buf, pcm...)
	return buf
}

func le16(v int) []byte {
	return []byte{byte(v), byte(v >> 8)}
}

func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// sarvamAudioMessage is the "Audio Transcription Message" sent to Sarvam,
// per https://docs.sarvam.ai/api-reference-docs/speech-to-text/transcribe/ws.
type sarvamAudioMessage struct {
	Audio struct {
		Data       string `json:"data"`
		SampleRate string `json:"sample_rate"`
		Encoding   string `json:"encoding"`
	} `json:"audio"`
}

// sarvamFlushMessage is our best-effort model of Sarvam's undocumented
// "Speech Flush Signal" message; see assumption (2) in the package doc
// comment.
type sarvamFlushMessage struct {
	Type string `json:"type"`
}

// sarvamResponseMessage models Sarvam's streaming response shape:
// {"type":"data","data":{"request_id":...,"transcript":...,"metrics":{...}}}.
// It also tolerates the "events" (VAD signal) shape described in the
// streaming-api guide, which nests a "signal_type" field instead of a
// transcript; we parse it defensively but do not emit a Transcript for it.
type sarvamResponseMessage struct {
	Type string `json:"type"`
	Data struct {
		RequestID  string `json:"request_id"`
		Transcript string `json:"transcript"`
		SignalType string `json:"signal_type"`
		Metrics    struct {
			AudioDurationSec float64 `json:"audio_duration"`
		} `json:"metrics"`
	} `json:"data"`
	// Some vendor error responses use a flat {"error": "..."} or
	// {"message": "..."} shape rather than nesting under "data"; we check
	// both defensively since Sarvam's error format for an already-open
	// streaming connection is not published.
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (s *sarvamSession) readLoop(conn *websocket.Conn, gen int) {
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

func (s *sarvamSession) handleMessage(gen int, data []byte) {
	var msg sarvamResponseMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return // ignore unparseable frames rather than crash the session.
	}

	if msg.Type == "error" || msg.Error != "" || msg.Message != "" {
		reason := msg.Error
		if reason == "" {
			reason = msg.Message
		}
		if reason == "" {
			reason = "unspecified error"
		}
		s.failAndClose(fmt.Errorf("asr/sarvam: %s", reason))
		return
	}

	if msg.Type == "events" {
		// VAD signal (speech_start/speech_end per the streaming guide, or
		// signal_type per the reference schema). Not a transcript; ignored
		// for Recognizer purposes, which only surfaces Transcripts.
		return
	}
	if msg.Type != "data" || msg.Data.Transcript == "" {
		return
	}

	s.mu.Lock()
	if s.closed || gen != s.gen {
		s.mu.Unlock()
		return
	}
	start := s.totalMS
	// Prefer the audio_duration metric Sarvam reports for this segment
	// when available; otherwise fall back to cumulative pushed-audio time.
	end := start
	if msg.Data.Metrics.AudioDurationSec > 0 {
		end = start + int64(msg.Data.Metrics.AudioDurationSec*1000)
	} else {
		end = s.totalMS
	}
	s.sendWG.Add(1)
	s.mu.Unlock()

	t := Transcript{
		Text:     msg.Data.Transcript,
		Language: s.lang,
		// See assumption (3) in the package doc comment.
		IsFinal:    true,
		Confidence: 1.0,
		StartMS:    start,
		EndMS:      end,
	}
	s.send(t)
	s.sendWG.Done()
}

func (s *sarvamSession) send(t Transcript) {
	select {
	case s.out <- t:
	case <-s.ctx.Done():
	}
}

// onConnLost drops a superseded/dead connection so the next PushAudio call
// reconnects; this is the "basic, documented" mid-stream reconnect behavior
// described on SarvamRecognizer (mirrors deepgramSession.onConnLost).
func (s *sarvamSession) onConnLost(gen int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || gen != s.gen {
		return
	}
	_ = err
	s.conn = nil
}

// failAndClose marks the session permanently closed due to a fatal,
// non-recoverable error and tears it down the same way Close() would.
func (s *sarvamSession) failAndClose(err error) {
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
func (s *sarvamSession) Close() error {
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
		// Best-effort flush; see assumption (2) in the package doc comment.
		// Its failure must not fail Close() -- we're shutting down either way.
		s.writeMu.Lock()
		_ = conn.WriteJSON(sarvamFlushMessage{Type: "flush"})
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
func (s *sarvamSession) Transcripts() <-chan Transcript {
	return s.out
}

var _ StreamSession = (*sarvamSession)(nil)
