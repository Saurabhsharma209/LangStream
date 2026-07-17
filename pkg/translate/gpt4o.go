package translate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exotel/langstream/pkg/observability"
)

// defaultGPT4oBaseURL is OpenAI's production API root. Tests override this
// via WithBaseURL to point at an httptest server instead.
const defaultGPT4oBaseURL = "https://api.openai.com/v1"

// defaultGPT4oModel is the chat-completions model used for translation.
const defaultGPT4oModel = "gpt-4o"

// maxSSELineSize bumps bufio.Scanner's default 64KiB buffer ceiling so a
// single SSE "data: ..." line (one JSON chunk) is never truncated even for
// unusually long completions.
const maxSSELineSize = 1 << 20 // 1MiB

// gpt4oMaxAttempts caps how many times Translate will try a request that
// keeps failing with a transient error (see transientTranslateError):
// one initial try plus up to two retries. Kept deliberately small since
// this call sits on the live-call translation critical path and a tight
// latency budget matters more here than squeezing out one more retry.
const gpt4oMaxAttempts = 3

// gpt4oRetryBaseDelay / gpt4oRetryMaxDelay bound the capped-exponential
// backoff (see retryBackoff) applied between retry attempts.
const (
	gpt4oRetryBaseDelay = 150 * time.Millisecond
	gpt4oRetryMaxDelay  = 1200 * time.Millisecond
)

// gpt4oInputCostPerTokenUSD / gpt4oOutputCostPerTokenUSD approximate
// OpenAI's published gpt-4o pricing (https://openai.com/api/pricing/, as
// reviewed while writing this): $2.50 per 1M input tokens, $10.00 per 1M
// output tokens. This is for pilot cost-visibility only, not
// billing-grade accuracy -- OpenAI's actual prices change over time and
// this value is not read live from any API.
const (
	gpt4oInputCostPerTokenUSD  = 2.50 / 1_000_000
	gpt4oOutputCostPerTokenUSD = 10.00 / 1_000_000

	// gpt4oApproxCharsPerToken is the commonly cited rule-of-thumb ratio
	// ("~4 characters per token" for English text) used to approximate
	// token counts on the fallback path, when the API response doesn't
	// carry a usage field (see recordCost). This is a documented
	// approximation, not a real tokenizer count -- it is least accurate
	// for non-Latin scripts such as Hindi/Devanagari, where GPT
	// tokenizers typically produce a different characters-per-token
	// ratio -- so treat fallback-derived cost as directional only.
	gpt4oApproxCharsPerToken = 4.0
)

// GPT4oTranslator implements Translator using OpenAI's GPT-4o chat
// completions endpoint in streaming mode (Server-Sent Events). It performs
// Hindi<->English call-center speech translation.
//
// Translate makes one streaming HTTP request per call, reads the SSE
// "data: {...}" chunks as they arrive, concatenates the incrementally
// streamed delta.content fields, and returns a single assembled Chunk once
// the stream ends (a "data: [DONE]" sentinel or the response body closes).
// This matches the Translator interface's contract exactly: one call in,
// one Chunk out, per (possibly partial) piece of ASR text -- streaming is
// used purely to start the network round trip as early as possible and to
// bound per-request memory, not to change the interface's synchronous
// per-chunk shape.
type GPT4oTranslator struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
	pairs      [][2]Language
	metrics    *observability.LatencyRecorder
	breaker    *circuitBreaker
}

// Option configures a GPT4oTranslator at construction time.
type Option func(*GPT4oTranslator)

// WithBaseURL overrides the API root (default "https://api.openai.com/v1").
// Tests use this to point the client at an httptest.Server.
func WithBaseURL(url string) Option {
	return func(g *GPT4oTranslator) {
		g.baseURL = strings.TrimRight(url, "/")
	}
}

// WithAPIKey overrides the API key read from the OPENAI_API_KEY environment
// variable. Mainly useful for tests that don't want to mutate the process
// environment.
func WithAPIKey(key string) Option {
	return func(g *GPT4oTranslator) { g.apiKey = key }
}

// WithModel overrides the chat-completions model (default "gpt-4o").
func WithModel(model string) Option {
	return func(g *GPT4oTranslator) { g.model = model }
}

// WithHTTPClient overrides the *http.Client used for requests (default
// http.DefaultClient). Useful for tests that need custom transports.
func WithHTTPClient(c *http.Client) Option {
	return func(g *GPT4oTranslator) {
		if c != nil {
			g.httpClient = c
		}
	}
}

// WithMetrics wires a shared *observability.LatencyRecorder into this
// translator so every successful Translate call attributes its cost (see
// RecordCost) to the "gpt-4o" vendor. Optional -- a nil/unset recorder
// (the default) makes cost recording a no-op, matching this package's
// existing functional-options convention (WithBaseURL, WithModel, ...).
func WithMetrics(m *observability.LatencyRecorder) Option {
	return func(g *GPT4oTranslator) { g.metrics = m }
}

// WithSupportedPairs overrides the (source, target) pairs this translator
// advertises via SupportedPairs. Defaults to hi<->en, the pilot's one
// supported language pair (see ROADMAP.md), matching MockTranslator.
func WithSupportedPairs(pairs ...[2]Language) Option {
	return func(g *GPT4oTranslator) {
		cp := make([][2]Language, len(pairs))
		copy(cp, pairs)
		g.pairs = cp
	}
}

// WithCircuitBreaker overrides breaker threshold/cooldown; non-positive values fall back to defaults.
func WithCircuitBreaker(threshold int, cooldown time.Duration) Option {
	return func(g *GPT4oTranslator) { g.breaker = newCircuitBreaker(threshold, cooldown) }
}

// NewGPT4oTranslator constructs a GPT4oTranslator. The API key is read from
// the OPENAI_API_KEY environment variable unless overridden with
// WithAPIKey. It returns an error if no API key is available, since every
// Translate call would otherwise fail at request time anyway.
func NewGPT4oTranslator(opts ...Option) (*GPT4oTranslator, error) {
	g := &GPT4oTranslator{
		apiKey:     os.Getenv("OPENAI_API_KEY"),
		baseURL:    defaultGPT4oBaseURL,
		model:      defaultGPT4oModel,
		httpClient: http.DefaultClient,
		pairs: [][2]Language{
			{"hi", "en"},
			{"en", "hi"},
		},
		breaker: newCircuitBreaker(0, 0),
	}
	for _, opt := range opts {
		opt(g)
	}
	if g.apiKey == "" {
		return nil, fmt.Errorf("translate/gpt4o: no API key: set OPENAI_API_KEY or use WithAPIKey")
	}
	return g, nil
}

// Name implements Translator.
func (g *GPT4oTranslator) Name() string { return "gpt-4o" }

// SupportedPairs implements Translator.
func (g *GPT4oTranslator) SupportedPairs() [][2]Language {
	out := make([][2]Language, len(g.pairs))
	copy(out, g.pairs)
	return out
}

func (g *GPT4oTranslator) supports(source, target Language) bool {
	for _, p := range g.pairs {
		srcOK := p[0] == source || p[0] == ""
		if srcOK && p[1] == target {
			return true
		}
	}
	return false
}

// languageName maps the short codes used throughout LangStream to the
// English names GPT-4o expects in the prompt. Unknown codes fall back to
// the raw code so an unexpected-but-supported pair still produces a
// sensible instruction rather than an empty one.
func languageName(l Language) string {
	switch l {
	case "hi":
		return "Hindi"
	case "en":
		return "English"
	default:
		return string(l)
	}
}

const gpt4oSystemPromptTemplate = `You are a real-time speech translation engine embedded in a call-center voice platform. Translate every message you receive from %s to %s.

Rules:
- Output ONLY the translation. No explanations, no notes, no quotation marks, no language labels, no restating the source text.
- Preserve the speaker's meaning, tone, and register (formal/informal, polite/urgent) exactly as spoken.
- Callers frequently code-switch between Hindi and English ("Hinglish") mid-sentence. Understand the intended meaning across both languages and produce one natural, fluent translation in the target language.
- Do not translate proper nouns, names, order/ticket numbers, OTPs, phone numbers, or other numeric identifiers; keep them verbatim.
- The input may be a partial, still-being-spoken utterance rather than a complete sentence, in which case translate only the words given as naturally as possible without inventing words that have not been said yet. Later calls will refine this translation as more of the utterance arrives.`

// chatMessage mirrors OpenAI's chat message shape: {"role": "...", "content": "..."}.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionRequest is the request body for POST /chat/completions with
// stream:true.
type chatCompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	Temperature   float64        `json:"temperature"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions requests OpenAI's documented "final usage chunk" for a
// streamed chat completion (a real, published API feature, not a guess):
// with include_usage:true, the server sends one extra SSE chunk right
// before "[DONE]" whose "usage" field carries the same prompt/completion/
// total token counts a non-streamed response would report. recordCost
// uses this for exact per-call cost, falling back to a character-count
// approximation only if it's ever absent (e.g. an older proxy stripping
// unknown fields).
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatCompletionUsage mirrors OpenAI's "usage" object: token counts for
// one chat completion.
type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatCompletionChunk is one SSE "data: {...}" payload in a streamed chat
// completion response.
type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is only populated on the final chunk of a stream started with
	// stream_options.include_usage:true (see streamOptions); nil on every
	// other chunk.
	Usage *chatCompletionUsage `json:"usage,omitempty"`
}

// openAIErrorBody is the JSON body OpenAI returns on non-2xx responses:
// {"error": {"message": "...", "type": "...", "code": "..."}}.
type openAIErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// transientTranslateError marks an error surfaced by translateOnce as a
// transient, worth-retrying failure: the request never made it to
// OpenAI (a network-level problem), the response came back 429/5xx, or
// the response stream broke off mid-read. Errors NOT wrapped this way --
// malformed SSE JSON, an unsupported language pair, request-encoding
// failures, other 4xx statuses (bad auth, bad request, ...) -- are
// treated as permanent, so Translate fails fast on them instead of
// spending this call's tight latency budget on retries that cannot help.
type transientTranslateError struct{ err error }

func (e *transientTranslateError) Error() string { return e.err.Error() }
func (e *transientTranslateError) Unwrap() error { return e.err }

// isRetryableTranslateErr reports whether err (as returned by
// translateOnce) is worth retrying; see transientTranslateError's doc
// comment for the classification rule.
func isRetryableTranslateErr(err error) bool {
	var transient *transientTranslateError
	return errors.As(err, &transient)
}

// Translate implements Translator using GPT-4o's streaming chat completions
// API. Transient failures (network errors, HTTP 429, HTTP 5xx, or the
// response stream dropping mid-read) are retried up to gpt4oMaxAttempts
// times with capped exponential backoff (see retryBackoff); permanent
// failures (unsupported pair, bad auth/bad request, malformed responses)
// fail fast on the first attempt. ctx cancellation/deadline is always
// respected immediately rather than being masked by a retry.
// A circuit breaker (see circuitbreaker.go) also tracks consecutive
// full-retry-exhaustion failures; once tripped, calls fail immediately
// until a cooldown elapses, then exactly one probe call is allowed.
func (g *GPT4oTranslator) Translate(ctx context.Context, text string, source, target Language, isFinal bool) (Chunk, error) {
	select {
	case <-ctx.Done():
		return Chunk{}, ctx.Err()
	default:
	}

	if !g.supports(source, target) {
		return Chunk{}, fmt.Errorf("translate/gpt4o: unsupported pair %q->%q", source, target)
	}

	if !g.breaker.allow() {
		if g.metrics != nil {
			g.metrics.RecordErrorReason("translate", g.Name(), "circuit_open")
		}
		return Chunk{}, fmt.Errorf("translate/gpt4o: %w", ErrCircuitOpen)
	}
	breakerSettled := false
	defer func() {
		if !breakerSettled {
			g.breaker.abort()
		}
	}()

	var lastErr error
	for attempt := 0; attempt < gpt4oMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBackoff(attempt-1, gpt4oRetryBaseDelay, gpt4oRetryMaxDelay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return Chunk{}, ctx.Err()
			}
		}

		translated, usage, err := g.translateOnce(ctx, text, source, target)
		if err == nil {
			g.breaker.recordSuccess()
			breakerSettled = true
			g.recordCost(text, translated, usage)
			return Chunk{
				Text:       translated,
				SourceLang: source,
				TargetLang: target,
				IsFinal:    isFinal,
			}, nil
		}

		if ctx.Err() != nil {
			// The caller's own cancellation/deadline, not a vendor
			// failure -- propagate it directly rather than retrying or
			// returning a possibly-stale vendor error instead.
			return Chunk{}, ctx.Err()
		}

		lastErr = err
		if !isRetryableTranslateErr(err) || attempt == gpt4oMaxAttempts-1 {
			if isRetryableTranslateErr(err) {
				g.breaker.recordFailure()
				breakerSettled = true
			}
			return Chunk{}, err
		}
	}
	return Chunk{}, lastErr
}

// translateOnce performs exactly one HTTP round trip against GPT-4o's
// streaming chat completions endpoint. See Translate's doc comment for
// the retry policy built on top of this.
func (g *GPT4oTranslator) translateOnce(ctx context.Context, text string, source, target Language) (string, *chatCompletionUsage, error) {
	reqBody := chatCompletionRequest{
		Model: g.model,
		Messages: []chatMessage{
			{
				Role:    "system",
				Content: fmt.Sprintf(gpt4oSystemPromptTemplate, languageName(source), languageName(target)),
			},
			{
				Role:    "user",
				Content: text,
			},
		},
		Stream:        true,
		Temperature:   0.2,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("translate/gpt4o: encode request: %w", err)
	}

	url := g.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("translate/gpt4o: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		// A failure to even complete the HTTP round trip (dial refused,
		// TLS failure, connection reset, timeout, ...) is exactly the
		// "transient network blip" case retries exist for.
		return "", nil, &transientTranslateError{fmt.Errorf("translate/gpt4o: request failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr openAIErrorBody
		var errOut error
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Error.Message != "" {
			errOut = fmt.Errorf("translate/gpt4o: API error (status %d, type %q, code %q): %s",
				resp.StatusCode, apiErr.Error.Type, apiErr.Error.Code, apiErr.Error.Message)
		} else {
			errOut = fmt.Errorf("translate/gpt4o: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return "", nil, &transientTranslateError{errOut}
		}
		// Any other 4xx (bad auth, bad request, not found, ...) is a
		// permanent client error a retry cannot fix.
		return "", nil, errOut
	}

	return g.readStream(ctx, resp.Body)
}

// gpt4oInputCostPerTokenUSD's package doc comment above documents the
// pricing assumptions; recordCost attributes the cost of one successful
// Translate call to the "gpt-4o" vendor, in USD. It prefers exact token
// counts from the API's usage field (populated via
// stream_options.include_usage; see translateOnce) and falls back to
// approximating token counts from input/output character counts (see
// gpt4oApproxCharsPerToken) only if usage is nil. No-op if no metrics
// recorder was configured via WithMetrics.
func (g *GPT4oTranslator) recordCost(inputText, outputText string, usage *chatCompletionUsage) {
	if g.metrics == nil {
		return
	}
	var promptTokens, completionTokens float64
	if usage != nil {
		promptTokens = float64(usage.PromptTokens)
		completionTokens = float64(usage.CompletionTokens)
	} else {
		promptTokens = float64(len(inputText)) / gpt4oApproxCharsPerToken
		completionTokens = float64(len(outputText)) / gpt4oApproxCharsPerToken
	}
	cost := promptTokens*gpt4oInputCostPerTokenUSD + completionTokens*gpt4oOutputCostPerTokenUSD
	g.metrics.RecordCost("gpt-4o", cost)
}

// readStream parses an SSE stream of chat-completion chunks, concatenating
// delta.content fields until it hits the "[DONE]" sentinel or the body
// closes, and captures the final usage chunk (see streamOptions) if the
// server sent one. It respects ctx cancellation between lines.
func (g *GPT4oTranslator) readStream(ctx context.Context, body io.Reader) (string, *chatCompletionUsage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var out strings.Builder
	var usage *chatCompletionUsage
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue // SSE frames are separated by blank lines
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			// Not a data field (e.g. a comment or event: line); ignore.
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return out.String(), usage, nil
		}

		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Malformed data from an otherwise-successful response: a
			// protocol/parsing problem, not a transient network
			// condition, so this is deliberately NOT wrapped as
			// transientTranslateError (retrying a malformed response
			// verbatim cannot help).
			return "", nil, fmt.Errorf("translate/gpt4o: malformed SSE chunk %q: %w", data, err)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			out.WriteString(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		// The stream broke off mid-read (connection reset, timeout,
		// ...); this is the same class of transient failure as a
		// request that never got a response at all.
		return "", nil, &transientTranslateError{fmt.Errorf("translate/gpt4o: reading stream: %w", err)}
	}
	// Body closed without an explicit [DONE] sentinel: treat whatever we
	// accumulated as the final result rather than erroring, since some
	// proxies/servers close the connection right after the last chunk.
	return out.String(), usage, nil
}

var _ Translator = (*GPT4oTranslator)(nil)
