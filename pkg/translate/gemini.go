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

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1"
const defaultGeminiModel = "gemini-2.0-flash"

const geminiMaxAttempts = 3

const (
	geminiRetryBaseDelay = 150 * time.Millisecond
	geminiRetryMaxDelay  = 1200 * time.Millisecond
)

type GeminiTranslator struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
	pairs      [][2]Language
	metrics    *observability.LatencyRecorder
	breaker    *circuitBreaker
}

type GeminiOption func(*GeminiTranslator)

func WithGeminiBaseURL(url string) GeminiOption {
	return func(g *GeminiTranslator) {
		g.baseURL = strings.TrimRight(url, "/")
	}
}

func WithGeminiAPIKey(key string) GeminiOption {
	return func(g *GeminiTranslator) { g.apiKey = key }
}

func WithGeminiModel(model string) GeminiOption {
	return func(g *GeminiTranslator) { g.model = model }
}

func WithGeminiHTTPClient(c *http.Client) GeminiOption {
	return func(g *GeminiTranslator) {
		if c != nil {
			g.httpClient = c
		}
	}
}

func WithGeminiMetrics(m *observability.LatencyRecorder) GeminiOption {
	return func(g *GeminiTranslator) { g.metrics = m }
}

func WithGeminiSupportedPairs(pairs ...[2]Language) GeminiOption {
	return func(g *GeminiTranslator) {
		cp := make([][2]Language, len(pairs))
		copy(cp, pairs)
		g.pairs = cp
	}
}

func WithGeminiCircuitBreaker(threshold int, cooldown time.Duration) GeminiOption {
	return func(g *GeminiTranslator) { g.breaker = newCircuitBreaker(threshold, cooldown) }
}

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

func (g *GeminiTranslator) Name() string { return "gemini" }

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

func languageNameGemini(l Language) string {
	switch l {
	case "hi":
		return "Hindi"
	case "en":
		return "English"
	default:
		return string(l)
	}
}

const geminiSystemPrompt = `You are a real-time speech translation engine embedded in a call-center voice platform. Translate every message you receive from %s to %s.

Rules:
- Output ONLY the translation. No explanations, no notes, no quotation marks, no language labels, no restating the source text.
- Preserve the speaker's meaning, tone, and register (formal/informal, polite/urgent) exactly as spoken.
- Callers frequently code-switch between Hindi and English ("Hinglish") mid-sentence. Understand the intended meaning across both languages and produce one natural, fluent translation in the target language.
- Do not translate proper nouns, names, order/ticket numbers, OTPs, phone numbers, or other numeric identifiers; keep them verbatim.
- The input may be a partial, still-being-spoken utterance rather than a complete sentence, in which case translate only the words given as naturally as possible without inventing words that have not been said yet. Later calls will refine this translation as more of the utterance arrives.`

type geminiRequest struct {
	Contents         []geminiContent         `json:"contents"`
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

type geminiResponse struct {
	Candidates []geminiCandidate `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
		Status  string `json:"status"`
	} `json:"error"`
}

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

		translated, err := g.translateOnce(ctx, text, source, target)
		if err == nil {
			g.breaker.recordSuccess()
			breakerSettled = true
			return Chunk{
				Text:       translated,
				SourceLang: source,
				TargetLang: target,
				IsFinal:    isFinal,
			}, nil
		}

		if ctx.Err() != nil {
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

func (g *GeminiTranslator) translateOnce(ctx context.Context, text string, source, target Language) (string, error) {
	prompt := fmt.Sprintf(geminiSystemPrompt, languageNameGemini(source), languageNameGemini(target))

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
		return "", fmt.Errorf("translate/gemini: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", g.baseURL, g.model, g.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("translate/gemini: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &transientTranslateError{fmt.Errorf("translate/gemini: request failed: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr geminiErrorBody
		var errOut error
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Error.Message != "" {
			errOut = fmt.Errorf("translate/gemini: API error (status %d, code %q): %s",
				resp.StatusCode, apiErr.Error.Status, apiErr.Error.Message)
		} else {
			errOut = fmt.Errorf("translate/gemini: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return "", &transientTranslateError{errOut}
		}
		return "", errOut
	}

	return g.readStream(ctx, resp.Body)
}

func (g *GeminiTranslator) readStream(ctx context.Context, body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var out strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)

		var resp geminiResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			return "", fmt.Errorf("translate/gemini: malformed SSE chunk: %w", err)
		}

		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			return "", fmt.Errorf("translate/gemini: blocked: %s", resp.PromptFeedback.BlockReason)
		}

		for _, c := range resp.Candidates {
			if c.FinishReason == "SAFETY" || c.FinishReason == "BLOCKLIST" || c.FinishReason == "PROHIBITED_CONTENT" {
				return "", fmt.Errorf("translate/gemini: blocked by content safety: %s", c.FinishReason)
			}
			for _, p := range c.Content.Parts {
				out.WriteString(p.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", &transientTranslateError{fmt.Errorf("translate/gemini: reading stream: %w", err)}
	}
	return out.String(), nil
}

var _ Translator = (*GeminiTranslator)(nil)
