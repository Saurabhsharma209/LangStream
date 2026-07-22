package translate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exotel/langstream/pkg/observability"
)

// defaultGeminiBaseURL is Google's Generative Language API root. Tests
// override this via WithGeminiBaseURL to point at an httptest server
// instead.
const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1"

// defaultGeminiModel is the model used for translation, selected via the
// streamGenerateContent SSE endpoint (see translateOnce).
const defaultGeminiModel = "gemini-2.0-flash"

// geminiMaxAttempts / geminiRetryBaseDelay / geminiRetryMaxDelay mirror
// gpt4o.go's retry policy exactly (see gpt4oMaxAttempts's doc comment for
// the reasoning: this call sits on the live-call translation critical
// path, so a tight latency budget matters more than squeezing out one
// more retry). Kept as separate constants (rather than reusing gpt4o's)
// so each vendor's retry budget can be tuned independently later without
// coupling the two backends.
const geminiMaxAttempts = 3

const (
	geminiRetryBaseDelay = 150 * time.Millisecond
	geminiRetryMaxDelay  = 1200 * time.Millisecond
)

// geminiInputCostPerTokenUSD / geminiOutputCostPerTokenUSD approximate
// Gemini 2.0 Flash's published per-token pricing: $0.10 per 1M input
// tokens, $0.40 per 1M output tokens (Google's originally published rate
// for this model, corroborated by third-party pricing aggregators
// reviewed 2026-07-22 -- e.g.
// https://pricepertoken.com/pricing-page/model/google-gemini-2.0-flash-001
// -- since Google's own pricing page for this specific, now-superseded
// model is no longer the most current page). As with gpt4o.go's
// equivalent constants, this is for pilot cost-visibility only, not
// billing-grade accuracy: prices change over time and this value is not
// read live from any API.
const (
	geminiInputCostPerTokenUSD  = 0.10 / 1_000_000
	geminiOutputCostPerTokenUSD = 0.40 / 1_000_000

	// geminiApproxCharsPerToken is the same commonly cited "~4 characters
	// per token" rule of thumb gpt4o.go's recordCost fallback path uses
	// (see gpt4oApproxCharsPerToken), applied here as Gemini's fallback
	// when a response doesn't carry usageMetadata (see recordCost). Not a
	// real tokenizer count, and least accurate for non-Latin scripts such
	// as Hindi/Devanagari -- treat fallback-derived cost as directional
	// only.
	geminiApproxCharsPerToken = 4.0
)

// GeminiTranslator implements Translator using Google's Gemini API in
// streaming mode (Server-Sent Events via streamGenerateContent?alt=sse).
// It performs Hindi<->English call-center speech translation, following
// the same one-call-in/one-Chunk-out contract as GPT4oTranslator (see its
// doc comment) -- streaming is used only to start the network round trip
// early and bound per-request memory, not to change the Translator
// interface's synchronous per-chunk shape.
type GeminiTranslator struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
	pairs      [][2]Language
	metrics    *observability.LatencyRecorder
	breaker    *circuitBreaker
}

// GeminiOption configures a GeminiTranslator at construction time.
type GeminiOption func(*GeminiTranslator)

// WithGeminiBaseURL overrides the API root (default
// "https://generativelanguage.googleapis.com/v1"). Tests use this to point
// the client at an httptest.Server.
func WithGeminiBaseURL(url string) GeminiOption {
	return func(g *GeminiTranslator) {
		g.baseURL = strings.TrimRight(url, "/")
	}
}

// WithGeminiAPIKey overrides the API key read from the GEMINI_API_KEY
// environment variable. Mainly useful for tests that don't want to
// mutate the process environment.
func WithGeminiAPIKey(key string) GeminiOption {
	return func(g *GeminiTranslator) { g.apiKey = key }
}

// WithGeminiModel overrides the model (default "gemini-2.0-flash").
func WithGeminiModel(model string) GeminiOption {
	return func(g *GeminiTranslator) { g.model = model }
}

// WithGeminiHTTPClient overrides the *http.Client used for requests
// (default http.DefaultClient). Useful for tests that need custom
// transports.
func WithGeminiHTTPClient(c *http.Client) GeminiOption {
	return func(g *GeminiTranslator) {
		if c != nil {
			g.httpClient = c
		}
	}
}

// WithGeminiMetrics wires a shared *observability.LatencyRecorder into
// this translator so every successful Translate call attributes its cost
// (see recordCost) to the "gemini" vendor. Optional -- a nil/unset
// recorder (the default) makes cost recording a no-op, matching this
// package's existing functional-options convention (WithMetrics on
// GPT4oTranslator, WithGeminiBaseURL, ...).
func WithGeminiMetrics(m *observability.LatencyRecorder) GeminiOption {
	return func(g *GeminiTranslator) { g.metrics = m }
}

// WithGeminiSupportedPairs overrides the (source, target) pairs this
// translator advertises via SupportedPairs. Defaults to hi<->en, the
// pilot's one supported language pair (see ROADMAP.md), matching
// GPT4oTranslator and MockTranslator.
func WithGeminiSupportedPairs(pairs ...[2]Language) GeminiOption {
	return func(g *GeminiTranslator) {
		cp := make([][2]Language, len(pairs))
		copy(cp, pairs)
		g.pairs = cp
	}
}

// WithGeminiCircuitBreaker overrides breaker threshold/cooldown;
// non-positive values fall back to defaults (see circuitbreaker.go).
func WithGeminiCircuitBreaker(threshold int, cooldown time.Duration) GeminiOption {
	return func(g *GeminiTranslator) { g.breaker = newCircuitBreaker(threshold, cooldown) }
}

// NewGeminiTranslator constructs a GeminiTranslator. The API key is read
// from the GEMINI_API_KEY environment variable unless overridden with
// WithGeminiAPIKey. It returns an error if no API key is available, since
// every Translate call would otherwise fail at request time anyway.
func NewGeminiTranslator(opts ...GeminiOption) (*GeminiTranslator, error) {
	g := &GeminiTranslator{
		apiKey:     os.Getenv("GEMINI_API_KEY"),
		baseURL:    defaultGeminiBaseURL,
		model:      defaultGeminiModel,
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
		return nil, fmt.Errorf("translate/gemini: no API key: set GEMINI_API_KEY or use WithGeminiAPIKey")
	}
	return g, nil
}

// Name implements Translator.
func (g *GeminiTranslator) Name() string { return "gemini" }

// SupportedPairs implements Translator.
func (g *GeminiTranslator) SupportedPairs() [][2]Language {
	out := make([][2]Language, len(g.pairs))
	copy(out, g.pairs)
	return out
}

func (g *GeminiTranslator) supports(source, target Language) bool {
	for _, p := range g.pairs {
		srcOK := p[0] == source || p[0] == ""
		if srcOK && p[1] == target {
			return true
		}
	}
	return false
}

// geminiRequest mirrors Gemini's generateContent/streamGenerateContent
// request body shape: a system instruction plus one or more "contents"
// turns, each holding one or more text "parts".
type geminiRequest struct {
	Contents          []geminiContent          `json:"contents"`
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

// geminiUsageMetadata mirrors Gemini's "usageMetadata" object, present on
// (at least) the final SSE chunk of a streamGenerateContent response, in
// the same role OpenAI's chatCompletionUsage (see gpt4o.go) plays for
// GPT-4o: exact token counts for one call, used by recordCost in
// preference to the character-count approximation.
type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// geminiResponse is one SSE "data: {...}" payload in a streamed
// generateContent response.
type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	// UsageMetadata is populated on (at least) the final chunk of a
	// streamGenerateContent response; see geminiUsageMetadata's doc
	// comment.
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

// geminiErrorBody is the JSON body Gemini returns on non-2xx responses:
// {"error": {"message": "...", "code": ..., "status": "..."}}.
type geminiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
		Status  string `json:"status"`
	} `json:"error"`
}

// geminiUnsafeFinishReasons are Candidate.finishReason values Gemini uses
// to report that a candidate was withheld by content-safety filtering
// rather than actually generated -- SAFETY (the response tripped a safety
// category), RECITATION (withheld as verbatim recited copyrighted
// content), BLOCKLIST/PROHIBITED_CONTENT (blocked terms/content-policy
// violation). Encountering any of these is treated as a permanent,
// non-retryable failure (see readStream): retrying the exact same input
// against the exact same safety filter cannot produce a different
// outcome.
var geminiUnsafeFinishReasons = map[string]bool{
	"SAFETY":             true,
	"RECITATION":         true,
	"BLOCKLIST":          true,
	"PROHIBITED_CONTENT": true,
}

// Translate implements Translator using Gemini's streaming generateContent
// API. Transient failures (network errors, HTTP 429, HTTP 5xx, or the
// response stream dropping mid-read) are retried up to geminiMaxAttempts
// times with capped exponential backoff (see retryBackoff, shared with
// GPT4oTranslator); permanent failures (unsupported pair, bad auth/bad
// request, malformed responses, safety blocks) fail fast on the first
// attempt. ctx cancellation/deadline is always respected immediately
// rather than being masked by a retry.
//
// A circuit breaker (see circuitbreaker.go, shared with GPT4oTranslator)
// also tracks consecutive full-retry-exhaustion failures; once tripped,
// calls fail immediately until a cooldown elapses, then exactly one probe
// call is allowed. This method's structure is deliberately kept
// line-for-line parallel to GPT4oTranslator.Translate so the two are easy
// to diff against each other; every exit path settles the breaker exactly
// once (recordSuccess on success, recordFailure only when a retryable
// error's retry budget is exhausted, and an unconditional deferred abort()
// otherwise) -- ctx cancellation and permanent (non-retryable) errors
// never touch consecutiveFails, matching GPT4oTranslator's already-audited
// behavior (see DEVLOG.md's vendor cost-recording / circuit-breaker audit
// history).
func (g *GeminiTranslator) Translate(ctx context.Context, text string, source, target Language, isFinal bool) (Chunk, error) {
	select {
	case <-ctx.Done():
		return Chunk{}, ctx.Err()
	default:
	}

	if !g.supports(source, target) {
		return Chunk{}, fmt.Errorf("translate/gemini: unsupported pair %q->%q", source, target)
	}

	if !g.breaker.allow() {
		if g.metrics != nil {
			g.metrics.RecordErrorReason("translate", g.Name(), "circuit_open")
		}
		return Chunk{}, fmt.Errorf("translate/gemini: %w", ErrCircuitOpen)
	}
	breakerSettled := false
	defer func() {
		if !breakerSettled {
			g.breaker.abort()
		}
	}()

	var lastErr error
	for attempt := 0; attempt < geminiMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBackoff(attempt-1, geminiRetryBaseDelay, geminiRetryMaxDelay)
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
		if !isRetryableTranslateErr(err) || attempt == geminiMaxAttempts-1 {
			if isRetryableTranslateErr(err) {
				g.breaker.recordFailure()
				breakerSettled = true
			}
			return Chunk{}, err
		}
	}
	return Chunk{}, lastErr
}

// translateOnce performs exactly one HTTP round trip against Gemini's
// streamGenerateContent endpoint. See Translate's doc comment for the
// retry policy built on top of this.
func (g *GeminiTranslator) translateOnce(ctx context.Context, text string, source, target Language) (string, *geminiUsageMetadata, error) {
	prompt := fmt.Sprintf(gpt4oSystemPromptTemplate, languageName(source), languageName(target))

	reqBody := geminiRequest{
		SystemInstruction: &geminiSystemInstruction{
			Parts: []geminiPart{{Text: prompt}},
		},
		Contents: []geminiContent{
			{
				Parts: []geminiPart{{Text: text}},
			},
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("translate/gemini: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", g.baseURL, g.model, g.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("translate/gemini: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		// A failure to even complete the HTTP round trip (dial refused,
		// TLS failure, connection reset, timeout, ...) is exactly the
		// "transient network blip" case retries exist for.
		return "", nil, &transientTranslateError{fmt.Errorf("translate/gemini: request failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr geminiErrorBody
		var errOut error
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Error.Message != "" {
			errOut = fmt.Errorf("translate/gemini: API error (status %d, status %q): %s",
				resp.StatusCode, apiErr.Error.Status, apiErr.Error.Message)
		} else {
			errOut = fmt.Errorf("translate/gemini: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

// recordCost attributes the cost of one successful Translate call to the
// "gemini" vendor, in USD (see geminiInputCostPerTokenUSD /
// geminiOutputCostPerTokenUSD's doc comment for the pricing source). It
// prefers exact token counts from the response's usageMetadata and falls
// back to approximating token counts from input/output character counts
// (see geminiApproxCharsPerToken) only if usage is nil (e.g. an older
// proxy stripping unknown fields, or a Gemini API version that doesn't
// populate it on every chunk). No-op if no metrics recorder was
// configured via WithGeminiMetrics. Mirrors GPT4oTranslator.recordCost's
// structure exactly, including its single call site (reached only once
// per Translate call, after the retry loop resolves to success) so a
// retry-then-succeed call records cost exactly once, never zero times or
// more than once.
func (g *GeminiTranslator) recordCost(inputText, outputText string, usage *geminiUsageMetadata) {
	if g.metrics == nil {
		return
	}
	var promptTokens, candidatesTokens float64
	if usage != nil {
		promptTokens = float64(usage.PromptTokenCount)
		candidatesTokens = float64(usage.CandidatesTokenCount)
	} else {
		promptTokens = float64(len(inputText)) / geminiApproxCharsPerToken
		candidatesTokens = float64(len(outputText)) / geminiApproxCharsPerToken
	}
	cost := promptTokens*geminiInputCostPerTokenUSD + candidatesTokens*geminiOutputCostPerTokenUSD
	g.metrics.RecordCost("gemini", cost)
}

// readStream parses an SSE stream of streamGenerateContent responses,
// concatenating candidate text parts until the body closes, and captures
// the latest usageMetadata seen (see geminiUsageMetadata's doc comment --
// Gemini's usageMetadata is cumulative across chunks, so the last chunk
// that carries one already reflects the whole call). It respects ctx
// cancellation between lines. Unlike GPT-4o's stream, Gemini's SSE stream
// has no "[DONE]" sentinel: the stream simply ends when the response body
// closes.
func (g *GeminiTranslator) readStream(ctx context.Context, body io.Reader) (string, *geminiUsageMetadata, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var out strings.Builder
	var usage *geminiUsageMetadata
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

		var resp geminiResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			// Malformed data from an otherwise-successful response: a
			// protocol/parsing problem, not a transient network
			// condition, so this is deliberately NOT wrapped as
			// transientTranslateError (retrying a malformed response
			// verbatim cannot help).
			return "", nil, fmt.Errorf("translate/gemini: malformed SSE chunk %q: %w", data, err)
		}

		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}

		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			return "", nil, fmt.Errorf("translate/gemini: blocked by content safety: prompt blocked (%s)", resp.PromptFeedback.BlockReason)
		}

		for _, c := range resp.Candidates {
			if geminiUnsafeFinishReasons[c.FinishReason] {
				return "", nil, fmt.Errorf("translate/gemini: blocked by content safety: finishReason %s", c.FinishReason)
			}
			for _, p := range c.Content.Parts {
				out.WriteString(p.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// The stream broke off mid-read (connection reset, timeout,
		// ...); this is the same class of transient failure as a
		// request that never got a response at all.
		return "", nil, &transientTranslateError{fmt.Errorf("translate/gemini: reading stream: %w", err)}
	}
	// Body closed cleanly (no "[DONE]" sentinel exists in Gemini's
	// protocol -- see this method's doc comment): whatever we
	// accumulated is the final result.
	return out.String(), usage, nil
}

var _ Translator = (*GeminiTranslator)(nil)
