package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/observability"
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

// --- retry-with-backoff tests --------------------------------------

// newCountingServer starts an httptest server whose handler is invoked
// via respond(attempt) for every request (1-indexed), so tests can script
// "fail N times, then succeed" sequences to exercise Translate's retry
// loop deterministically.
func newCountingServer(t *testing.T, respond func(attempt int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		respond(int(n), w)
	}))
	return srv, &attempts
}

func TestGPT4oTranslator_Translate_RetriesOn429ThenSucceeds(t *testing.T) {
	sse := sseChunk("hi") + "data: [DONE]\n\n"
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if chunk.Text != "hi" {
		t.Errorf("chunk.Text = %q, want %q", chunk.Text, "hi")
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}
}

func TestGPT4oTranslator_Translate_RetriesOn5xxThenSucceeds(t *testing.T) {
	sse := sseChunk("hi") + "data: [DONE]\n\n"
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream unavailable"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success), gpt4oMaxAttempts=%d", got, gpt4oMaxAttempts)
	}
}

func TestGPT4oTranslator_Translate_DoesNotRetryOn400(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for HTTP 400 response, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (400 must fail fast, not retry)", got)
	}
}

func TestGPT4oTranslator_Translate_DoesNotRetryOn401(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("bad-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for HTTP 401 response, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (401 bad auth must fail fast, not retry)", got)
	}
}

func TestGPT4oTranslator_Translate_ExhaustsRetriesOnPersistentFailure(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(attempts); int(got) != gpt4oMaxAttempts {
		t.Errorf("attempts = %d, want gpt4oMaxAttempts = %d", got, gpt4oMaxAttempts)
	}
}

func TestGPT4oTranslator_Translate_RetriesOnConnectionReset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	// Reserve a port, then close it immediately so nothing listens there:
	// every dial attempt gets a connection-refused error, the same class
	// of transient network failure as a mid-call reset.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL("http://"+addr), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	start := time.Now()
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	// With gpt4oMaxAttempts=3 and backoff starting at gpt4oRetryBaseDelay,
	// two backoff sleeps must have elapsed before giving up -- a rough
	// sanity check that retries actually happened rather than failing
	// fast on the first attempt.
	if elapsed < gpt4oRetryBaseDelay {
		t.Errorf("elapsed = %v, expected at least one retry backoff delay (%v), suggesting no retry happened", elapsed, gpt4oRetryBaseDelay)
	}
}

func TestGPT4oTranslator_Translate_ContextCancellationDuringRetryBackoff(t *testing.T) {
	srv, _ := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	})
	defer srv.Close()

	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	// Long enough to complete the first (failing) attempt, short enough
	// to expire while waiting out the backoff delay before attempt 2.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err = tr.Translate(ctx, "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected an error from context deadline during retry backoff, got nil")
	}
}

// --- cost recording tests --------------------------------------------

func TestGPT4oTranslator_Translate_RecordsCostFromUsage(t *testing.T) {
	usageChunk := chatCompletionChunk{
		Usage: &chatCompletionUsage{PromptTokens: 100, CompletionTokens: 40, TotalTokens: 140},
	}
	usageB, err := json.Marshal(usageChunk)
	if err != nil {
		t.Fatalf("marshal usage chunk: %v", err)
	}
	sse := sseChunk("hello there") + fmt.Sprintf("data: %s\n\n", usageB) + "data: [DONE]\n\n"

	srv := newTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	want := 100*gpt4oInputCostPerTokenUSD + 40*gpt4oOutputCostPerTokenUSD
	got := metrics.CostTotal("gpt-4o")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(gpt-4o) = %v, want %v (from usage tokens)", got, want)
	}
	if n := metrics.CostEventCount("gpt-4o"); n != 1 {
		t.Errorf("CostEventCount(gpt-4o) = %d, want 1", n)
	}
}

func TestGPT4oTranslator_Translate_RecordsCostFallbackWithoutUsage(t *testing.T) {
	const input = "namaste, kaise ho?"
	sse := sseChunk("hello, how are you?") + "data: [DONE]\n\n"

	srv := newTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), input, "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	wantPromptTokens := float64(len(input)) / gpt4oApproxCharsPerToken
	wantCompletionTokens := float64(len(chunk.Text)) / gpt4oApproxCharsPerToken
	want := wantPromptTokens*gpt4oInputCostPerTokenUSD + wantCompletionTokens*gpt4oOutputCostPerTokenUSD
	got := metrics.CostTotal("gpt-4o")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(gpt-4o) = %v, want %v (character-count fallback)", got, want)
	}
}

func TestGPT4oTranslator_Translate_NoMetricsConfiguredNoOp(t *testing.T) {
	sse := sseChunk("hi") + "data: [DONE]\n\n"
	srv := newTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	// No WithMetrics option: recordCost must be a safe no-op rather than
	// panicking on a nil *observability.LatencyRecorder.
	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err != nil {
		t.Fatalf("Translate: %v", err)
	}
}

func TestGPT4oTranslator_Translate_FailedRequestDoesNotRecordCost(t *testing.T) {
	srv, _ := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	})
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGPT4oTranslator(WithBaseURL(srv.URL), WithAPIKey("test-api-key"), WithMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an error, got nil")
	}
	if n := metrics.CostEventCount("gpt-4o"); n != 0 {
		t.Errorf("CostEventCount(gpt-4o) = %d, want 0 for a failed request", n)
	}
}
