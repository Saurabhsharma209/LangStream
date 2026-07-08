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
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
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

// Translate implements Translator using GPT-4o's streaming chat completions
// API.
func (g *GPT4oTranslator) Translate(ctx context.Context, text string, source, target Language, isFinal bool) (Chunk, error) {
	select {
	case <-ctx.Done():
		return Chunk{}, ctx.Err()
	default:
	}

	if !g.supports(source, target) {
		return Chunk{}, fmt.Errorf("translate/gpt4o: unsupported pair %q->%q", source, target)
	}

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
		Stream:      true,
		Temperature: 0.2,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return Chunk{}, fmt.Errorf("translate/gpt4o: encode request: %w", err)
	}

	url := g.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Chunk{}, fmt.Errorf("translate/gpt4o: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Chunk{}, ctx.Err()
		}
		return Chunk{}, fmt.Errorf("translate/gpt4o: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr openAIErrorBody
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Error.Message != "" {
			return Chunk{}, fmt.Errorf("translate/gpt4o: API error (status %d, type %q, code %q): %s",
				resp.StatusCode, apiErr.Error.Type, apiErr.Error.Code, apiErr.Error.Message)
		}
		return Chunk{}, fmt.Errorf("translate/gpt4o: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	translated, err := g.readStream(ctx, resp.Body)
	if err != nil {
		return Chunk{}, err
	}

	return Chunk{
		Text:       translated,
		SourceLang: source,
		TargetLang: target,
		IsFinal:    isFinal,
	}, nil
}

// readStream parses an SSE stream of chat-completion chunks, concatenating
// delta.content fields until it hits the "[DONE]" sentinel or the body
// closes. It respects ctx cancellation between lines.
func (g *GPT4oTranslator) readStream(ctx context.Context, body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var out strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
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
			return out.String(), nil
		}

		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", fmt.Errorf("translate/gpt4o: malformed SSE chunk %q: %w", data, err)
		}
		for _, choice := range chunk.Choices {
			out.WriteString(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("translate/gpt4o: reading stream: %w", err)
	}
	// Body closed without an explicit [DONE] sentinel: treat whatever we
	// accumulated as the final result rather than erroring, since some
	// proxies/servers close the connection right after the last chunk.
	return out.String(), nil
}

var _ Translator = (*GPT4oTranslator)(nil)
