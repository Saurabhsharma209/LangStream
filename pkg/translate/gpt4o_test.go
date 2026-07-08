package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, sseBody string, statusCode int, lastReq *chatCompletionRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastReq != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("test server: reading request body: %v", err)
			}
			if err := json.Unmarshal(body, lastReq); err != nil {
				t.Errorf("test server: decoding request body: %v", err)
			}
		}

		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.URL.Path; got != "/chat/completions" {
			t.Errorf("expected path /chat/completions, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
			t.Errorf("expected Authorization header 'Bearer test-api-key', got %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", got)
		}

		if statusCode != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(sseBody))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}

func sseChunk(content string) string {
	payload := chatCompletionChunk{}
	payload.Choices = []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}{
		{
			Delta: struct {
				Content string `json:"content"`
			}{Content: content},
		},
	}
	b, _ := json.Marshal(payload)
	return fmt.Sprintf("data: %s\n\n", b)
}

func TestGPT4oTranslator_Translate_IncrementalSSE(t *testing.T) {
	sse := sseChunk("Hello") + sseChunk(", how") + sseChunk(" are you?") + "data: [DONE]\n\n"

	var lastReq chatCompletionRequest
	srv := newTestServer(t, sse, http.StatusOK, &lastReq)
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), "namaste, kaise ho?", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if want := "Hello, how are you?"; chunk.Text != want {
		t.Errorf("expected assembled text %q, got %q", want, chunk.Text)
	}
	if chunk.SourceLang != "hi" || chunk.TargetLang != "en" {
		t.Errorf("expected source=hi target=en, got source=%v target=%v", chunk.SourceLang, chunk.TargetLang)
	}
	if !chunk.IsFinal {
		t.Error("expected IsFinal=true to be propagated")
	}

	if lastReq.Model != defaultGPT4oModel {
		t.Errorf("expected model %q, got %q", defaultGPT4oModel, lastReq.Model)
	}
	if !lastReq.Stream {
		t.Error("expected stream:true in request body")
	}
	if len(lastReq.Messages) != 2 {
		t.Fatalf("expected 2 messages (system, user), got %d", len(lastReq.Messages))
	}
	sys := lastReq.Messages[0]
	if sys.Role != "system" {
		t.Errorf("expected first message role 'system', got %q", sys.Role)
	}
	if !strings.Contains(sys.Content, "Hindi") || !strings.Contains(sys.Content, "English") {
		t.Errorf("expected system prompt to mention Hindi and English, got %q", sys.Content)
	}
	if !strings.Contains(sys.Content, "from Hindi to English") {
		t.Errorf("expected system prompt to state direction 'from Hindi to English', got %q", sys.Content)
	}
	user := lastReq.Messages[1]
	if user.Role != "user" {
		t.Errorf("expected second message role 'user', got %q", user.Role)
	}
	if user.Content != "namaste, kaise ho?" {
		t.Errorf("expected user message to be the raw source text, got %q", user.Content)
	}
}

func TestGPT4oTranslator_Translate_EnToHi_PromptDirection(t *testing.T) {
	sse := sseChunk("test") + "data: [DONE]\n\n"

	var lastReq chatCompletionRequest
	srv := newTestServer(t, sse, http.StatusOK, &lastReq)
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), "hello", "en", "hi", false)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if chunk.IsFinal {
		t.Error("expected IsFinal=false to be propagated for partial input")
	}
	if !strings.Contains(lastReq.Messages[0].Content, "from English to Hindi") {
		t.Errorf("expected system prompt direction 'from English to Hindi', got %q", lastReq.Messages[0].Content)
	}
}

func TestGPT4oTranslator_Translate_UnsupportedPair(t *testing.T) {
	tr, err := NewGPT4oTranslator(WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	if _, err := tr.Translate(context.Background(), "bonjour", "fr", "en", true); err == nil {
		t.Fatal("expected error for unsupported language pair, got nil")
	}
}

func TestGPT4oTranslator_Translate_HTTPErrorStatus(t *testing.T) {
	errBody := `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error","code":"rate_limit_exceeded"}}`
	srv := newTestServer(t, errBody, http.StatusTooManyRequests, nil)
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for HTTP 429 response, got nil")
	}
	if !strings.Contains(err.Error(), "Rate limit reached") {
		t.Errorf("expected error to include API error message, got %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to include status code 429, got %v", err)
	}
}

func TestGPT4oTranslator_Translate_MalformedSSE(t *testing.T) {
	sse := "data: {not valid json\n\n"
	srv := newTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for malformed SSE chunk, got nil")
	}
	if !strings.Contains(err.Error(), "malformed SSE chunk") {
		t.Errorf("expected malformed SSE error, got %v", err)
	}
}

func TestGPT4oTranslator_Translate_ContextCancellation(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
	}))
	defer srv.Close()
	defer close(blockCh)

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = tr.Translate(ctx, "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error from context cancellation, got nil")
	}
}

func TestGPT4oTranslator_Translate_PreCancelledContext(t *testing.T) {
	tr, err := NewGPT4oTranslator(WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tr.Translate(ctx, "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
}

func TestNewGPT4oTranslator_MissingAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := NewGPT4oTranslator(); err == nil {
		t.Fatal("expected error when OPENAI_API_KEY is unset and no WithAPIKey given")
	}
}

func TestNewGPT4oTranslator_ReadsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	tr, err := NewGPT4oTranslator()
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	if tr.apiKey != "env-key" {
		t.Errorf("expected apiKey to be read from OPENAI_API_KEY, got %q", tr.apiKey)
	}
}

func TestGPT4oTranslator_Name(t *testing.T) {
	tr, err := NewGPT4oTranslator(WithAPIKey("k"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	if tr.Name() != "gpt-4o" {
		t.Errorf("expected Name() == \"gpt-4o\", got %q", tr.Name())
	}
}

func TestGPT4oTranslator_SupportedPairs(t *testing.T) {
	tr, err := NewGPT4oTranslator(WithAPIKey("k"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	pairs := tr.SupportedPairs()
	want := map[[2]Language]bool{
		{"hi", "en"}: false,
		{"en", "hi"}: false,
	}
	for _, p := range pairs {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("expected SupportedPairs() to include %v, got %v", p, pairs)
		}
	}
}

var _ Translator = (*GPT4oTranslator)(nil)
