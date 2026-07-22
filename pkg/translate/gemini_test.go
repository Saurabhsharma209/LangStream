package translate

import (
	"context"
	"encoding/json"
	"errors"
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

// newGeminiTestServer starts an httptest server shaped like Gemini's
// streamGenerateContent?alt=sse endpoint. If lastReq is non-nil, the
// decoded request body is written into it.
func newGeminiTestServer(t *testing.T, sseBody string, statusCode int, lastReq *geminiRequest) *httptest.Server {
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
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("expected path to contain :streamGenerateContent, got %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Errorf("expected alt=sse query param, got %q", got)
		}
		if got := r.URL.Query().Get("key"); got != "test-api-key" {
			t.Errorf("expected key=test-api-key query param, got %q", got)
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

// geminiSSEChunk builds one "data: {...}" SSE frame carrying a single text
// part, optionally with a finishReason and/or usageMetadata.
func geminiSSEChunk(text, finishReason string, usage *geminiUsageMetadata) string {
	resp := geminiResponse{
		Candidates: []geminiCandidate{
			{
				Content:      geminiContent{Parts: []geminiPart{{Text: text}}},
				FinishReason: finishReason,
			},
		},
		UsageMetadata: usage,
	}
	b, _ := json.Marshal(resp)
	return fmt.Sprintf("data: %s\n\n", b)
}

func TestGeminiTranslator_Translate_IncrementalSSE(t *testing.T) {
	sse := geminiSSEChunk("Hello", "", nil) + geminiSSEChunk(", how", "", nil) + geminiSSEChunk(" are you?", "STOP", nil)

	var lastReq geminiRequest
	srv := newGeminiTestServer(t, sse, http.StatusOK, &lastReq)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
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

	if len(lastReq.Contents) != 1 || len(lastReq.Contents[0].Parts) != 1 {
		t.Fatalf("expected exactly one content part, got %+v", lastReq.Contents)
	}
	if lastReq.Contents[0].Parts[0].Text != "namaste, kaise ho?" {
		t.Errorf("expected user content to be the raw source text, got %q", lastReq.Contents[0].Parts[0].Text)
	}
	if lastReq.SystemInstruction == nil || len(lastReq.SystemInstruction.Parts) != 1 {
		t.Fatalf("expected a system instruction with one part, got %+v", lastReq.SystemInstruction)
	}
	sys := lastReq.SystemInstruction.Parts[0].Text
	if !strings.Contains(sys, "Hindi") || !strings.Contains(sys, "English") {
		t.Errorf("expected system prompt to mention Hindi and English, got %q", sys)
	}
	if !strings.Contains(sys, "from Hindi to English") {
		t.Errorf("expected system prompt to state direction 'from Hindi to English', got %q", sys)
	}
}

func TestGeminiTranslator_Translate_EnToHi_PromptDirection(t *testing.T) {
	sse := geminiSSEChunk("test", "STOP", nil)

	var lastReq geminiRequest
	srv := newGeminiTestServer(t, sse, http.StatusOK, &lastReq)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), "hello", "en", "hi", false)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if chunk.IsFinal {
		t.Error("expected IsFinal=false to be propagated for partial input")
	}
	if !strings.Contains(lastReq.SystemInstruction.Parts[0].Text, "from English to Hindi") {
		t.Errorf("expected system prompt direction 'from English to Hindi', got %q", lastReq.SystemInstruction.Parts[0].Text)
	}
}

func TestGeminiTranslator_Translate_UnsupportedPair(t *testing.T) {
	tr, err := NewGeminiTranslator(WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	if _, err := tr.Translate(context.Background(), "bonjour", "fr", "en", true); err == nil {
		t.Fatal("expected error for unsupported language pair, got nil")
	}
}

func TestGeminiTranslator_Translate_HTTPErrorStatus(t *testing.T) {
	errBody := `{"error":{"message":"Resource has been exhausted","code":429,"status":"RESOURCE_EXHAUSTED"}}`
	srv := newGeminiTestServer(t, errBody, http.StatusTooManyRequests, nil)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for HTTP 429 response, got nil")
	}
	if !strings.Contains(err.Error(), "Resource has been exhausted") {
		t.Errorf("expected error to include API error message, got %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to include status code 429, got %v", err)
	}
}

func TestGeminiTranslator_Translate_MalformedSSE(t *testing.T) {
	sse := "data: {not valid json\n\n"
	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for malformed SSE chunk, got nil")
	}
	if !strings.Contains(err.Error(), "malformed SSE chunk") {
		t.Errorf("expected malformed SSE error, got %v", err)
	}
}

// --- safety-block handling --------------------------------------------

func TestGeminiTranslator_Translate_SafetyBlockedFinishReason(t *testing.T) {
	sse := geminiSSEChunk("partial", "SAFETY", nil)
	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for a SAFETY finishReason, got nil")
	}
	if !strings.Contains(err.Error(), "content safety") {
		t.Errorf("expected a content-safety error, got %v", err)
	}
}

func TestGeminiTranslator_Translate_PromptFeedbackBlocked(t *testing.T) {
	resp := geminiResponse{
		PromptFeedback: &struct {
			BlockReason string `json:"blockReason"`
		}{BlockReason: "OTHER"},
	}
	b, _ := json.Marshal(resp)
	sse := fmt.Sprintf("data: %s\n\n", b)
	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for a blocked promptFeedback, got nil")
	}
	if !strings.Contains(err.Error(), "content safety") {
		t.Errorf("expected a content-safety error, got %v", err)
	}
}

func TestGeminiTranslator_Translate_SafetyBlockDoesNotRetry(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, geminiSSEChunk("", "SAFETY", nil))
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for a SAFETY finishReason, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (a safety block must fail fast, not retry)", got)
	}
}

// --- context cancellation ---------------------------------------------

func TestGeminiTranslator_Translate_ContextCancellation(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
	}))
	defer srv.Close()
	defer close(blockCh)

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = tr.Translate(ctx, "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error from context cancellation, got nil")
	}
}

// TestGeminiTranslator_Translate_ContextCancellationMidStream verifies that
// cancelling ctx while the SSE body is still streaming (rather than before
// the first byte arrives) is observed and returned promptly, exercising
// readStream's per-line ctx.Done() check rather than just the top-level
// pre-request check.
func TestGeminiTranslator_Translate_ContextCancellationMidStream(t *testing.T) {
	firstChunkSent := make(chan struct{})
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, geminiSSEChunk("partial", "", nil))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(firstChunkSent)
		<-blockCh
	}))
	defer srv.Close()
	defer close(blockCh)

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-firstChunkSent
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err = tr.Translate(ctx, "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error from mid-stream context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
}

func TestGeminiTranslator_Translate_PreCancelledContext(t *testing.T) {
	tr, err := NewGeminiTranslator(WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tr.Translate(ctx, "hello", "en", "hi", true)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
}

// --- construction / naming ----------------------------------------------

func TestNewGeminiTranslator_MissingAPIKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	if _, err := NewGeminiTranslator(); err == nil {
		t.Fatal("expected error when GEMINI_API_KEY is unset and no WithGeminiAPIKey given")
	}
}

func TestNewGeminiTranslator_ReadsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "env-key")
	tr, err := NewGeminiTranslator()
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	if tr.apiKey != "env-key" {
		t.Errorf("expected apiKey to be read from GEMINI_API_KEY, got %q", tr.apiKey)
	}
}

func TestGeminiTranslator_Name(t *testing.T) {
	tr, err := NewGeminiTranslator(WithGeminiAPIKey("k"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	if tr.Name() != "gemini" {
		t.Errorf("expected Name() == \"gemini\", got %q", tr.Name())
	}
}

func TestGeminiTranslator_SupportedPairs(t *testing.T) {
	tr, err := NewGeminiTranslator(WithGeminiAPIKey("k"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
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

var _ Translator = (*GeminiTranslator)(nil)

// --- retry-with-backoff tests --------------------------------------

func TestGeminiTranslator_Translate_RetriesOn429ThenSucceeds(t *testing.T) {
	sse := geminiSSEChunk("hi", "STOP", nil)
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":429,"status":"RESOURCE_EXHAUSTED"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
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

func TestGeminiTranslator_Translate_RetriesOn5xxThenSucceeds(t *testing.T) {
	sse := geminiSSEChunk("hi", "STOP", nil)
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

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures + 1 success), geminiMaxAttempts=%d", got, geminiMaxAttempts)
	}
}

func TestGeminiTranslator_Translate_DoesNotRetryOn400(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","code":400,"status":"INVALID_ARGUMENT"}}`))
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for HTTP 400 response, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (400 must fail fast, not retry)", got)
	}
}

func TestGeminiTranslator_Translate_DoesNotRetryOn401(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","code":401,"status":"UNAUTHENTICATED"}}`))
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("bad-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error for HTTP 401 response, got nil")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 (401 bad auth must fail fast, not retry)", got)
	}
}

func TestGeminiTranslator_Translate_ExhaustsRetriesOnPersistentFailure(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(attempts); int(got) != geminiMaxAttempts {
		t.Errorf("attempts = %d, want geminiMaxAttempts = %d", got, geminiMaxAttempts)
	}
}

func TestGeminiTranslator_Translate_RetriesOnConnectionReset(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	// Reserve a port, then close it immediately so nothing listens there:
	// every dial attempt gets a connection-refused error, the same class
	// of transient network failure as a mid-call reset.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL("http://"+addr), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	start := time.Now()
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	if elapsed < geminiRetryBaseDelay {
		t.Errorf("elapsed = %v, expected at least one retry backoff delay (%v), suggesting no retry happened", elapsed, geminiRetryBaseDelay)
	}
}

func TestGeminiTranslator_Translate_ContextCancellationDuringRetryBackoff(t *testing.T) {
	srv, _ := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	})
	defer srv.Close()

	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
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

func TestGeminiTranslator_Translate_RecordsCostFromUsage(t *testing.T) {
	usage := &geminiUsageMetadata{PromptTokenCount: 100, CandidatesTokenCount: 40, TotalTokenCount: 140}
	sse := geminiSSEChunk("hello there", "STOP", usage)

	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"), WithGeminiMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err != nil {
		t.Fatalf("Translate: %v", err)
	}

	want := 100*geminiInputCostPerTokenUSD + 40*geminiOutputCostPerTokenUSD
	got := metrics.CostTotal("gemini")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(gemini) = %v, want %v (from usage tokens)", got, want)
	}
	if n := metrics.CostEventCount("gemini"); n != 1 {
		t.Errorf("CostEventCount(gemini) = %d, want 1", n)
	}
}

func TestGeminiTranslator_Translate_RecordsCostFallbackWithoutUsage(t *testing.T) {
	const input = "namaste, kaise ho?"
	sse := geminiSSEChunk("hello, how are you?", "STOP", nil)

	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"), WithGeminiMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	chunk, err := tr.Translate(context.Background(), input, "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	wantPromptTokens := float64(len(input)) / geminiApproxCharsPerToken
	wantCandidatesTokens := float64(len(chunk.Text)) / geminiApproxCharsPerToken
	want := wantPromptTokens*geminiInputCostPerTokenUSD + wantCandidatesTokens*geminiOutputCostPerTokenUSD
	got := metrics.CostTotal("gemini")
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostTotal(gemini) = %v, want %v (character-count fallback)", got, want)
	}
}

func TestGeminiTranslator_Translate_NoMetricsConfiguredNoOp(t *testing.T) {
	sse := geminiSSEChunk("hi", "STOP", nil)
	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	// No WithGeminiMetrics option: recordCost must be a safe no-op rather
	// than panicking on a nil *observability.LatencyRecorder.
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err != nil {
		t.Fatalf("Translate: %v", err)
	}
}

func TestGeminiTranslator_Translate_FailedRequestDoesNotRecordCost(t *testing.T) {
	srv, _ := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","code":400,"status":"INVALID_ARGUMENT"}}`))
	})
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"), WithGeminiMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an error, got nil")
	}
	if n := metrics.CostEventCount("gemini"); n != 0 {
		t.Errorf("CostEventCount(gemini) = %d, want 0 for a failed request", n)
	}
}

// TestGeminiTranslator_Translate_SafetyBlockDoesNotRecordCost pins that a
// content-safety block (a "successful" HTTP round trip that nonetheless
// never produced usable translated text) never records cost either --
// this path returns an error from translateOnce before Translate's success
// branch (and therefore recordCost) is ever reached.
func TestGeminiTranslator_Translate_SafetyBlockDoesNotRecordCost(t *testing.T) {
	sse := geminiSSEChunk("", "SAFETY", nil)
	srv := newGeminiTestServer(t, sse, http.StatusOK, nil)
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"), WithGeminiMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an error for a safety-blocked response")
	}
	if n := metrics.CostEventCount("gemini"); n != 0 {
		t.Errorf("CostEventCount(gemini) = %d, want 0 for a safety-blocked call", n)
	}
}

// TestGeminiTranslator_Translate_RetrySucceeds_RecordsCostExactlyOnce pins
// invariant (1) from the 2026-07-21 PE cost-tracking audit (see
// gpt4o_test.go's equivalent and DEVLOG.md): a retry-then-succeed call
// must record cost exactly once, not once per HTTP attempt.
func TestGeminiTranslator_Translate_RetrySucceeds_RecordsCostExactlyOnce(t *testing.T) {
	sse := geminiSSEChunk("hi", "STOP", nil)
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":429,"status":"RESOURCE_EXHAUSTED"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	})
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	tr, err := NewGeminiTranslator(WithGeminiBaseURL(srv.URL), WithGeminiAPIKey("test-api-key"), WithGeminiMetrics(metrics))
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (1 failure + 1 success)", got)
	}

	if n := metrics.CostEventCount("gemini"); n != 1 {
		t.Errorf("CostEventCount(gemini) = %d, want exactly 1 for a call that failed once (429) then succeeded on retry -- a mismatch would mean RecordCost fired once per HTTP attempt instead of once per Translate call", n)
	}
	if got := metrics.CostTotal("gemini"); got <= 0 {
		t.Errorf("CostTotal(gemini) = %v, want > 0 after the retry eventually succeeded", got)
	}
}

// TestGeminiTranslator_Translate_CircuitOpen_NeverRecordsCost pins
// invariant (4) from the 2026-07-21 PE cost-tracking audit: a call
// rejected fail-fast by an open circuit breaker (zero HTTP requests ever
// made) must never record a nonzero cost.
func TestGeminiTranslator_Translate_CircuitOpen_NeverRecordsCost(t *testing.T) {
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	})
	defer srv.Close()

	metrics := observability.NewLatencyRecorder()
	const threshold = 1
	tr, err := NewGeminiTranslator(
		WithGeminiBaseURL(srv.URL),
		WithGeminiAPIKey("test-api-key"),
		WithGeminiMetrics(metrics),
		WithGeminiCircuitBreaker(threshold, 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	// This call exhausts its full retry budget against the always-500
	// server, tripping the breaker (threshold=1).
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected an error after exhausting retries against the always-failing server")
	}
	attemptsAfterTrip := atomic.LoadInt32(attempts)
	costEventsAfterTrip := metrics.CostEventCount("gemini")

	// The breaker should now be open: the next call must be rejected
	// fail-fast, with zero additional HTTP requests and, critically, zero
	// additional RecordCost calls.
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second Translate call to be rejected by the (now open) breaker, got: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != attemptsAfterTrip {
		t.Errorf("HTTP attempts after breaker-rejected Translate = %d, want unchanged %d (zero additional requests)", got, attemptsAfterTrip)
	}
	if got := metrics.CostEventCount("gemini"); got != costEventsAfterTrip {
		t.Errorf("CostEventCount(gemini) after breaker-rejected Translate = %d, want unchanged %d (a circuit-open rejection must never record cost)", got, costEventsAfterTrip)
	}
	if got := metrics.CostTotal("gemini"); got != 0 {
		t.Errorf("CostTotal(gemini) = %v, want 0: no call in this test ever succeeded, so no cost should ever have been recorded", got)
	}
}

// --- circuit breaker: probe recovery + cooldown -------------------------

// TestGeminiTranslator_Translate_CircuitBreaker_ProbeAfterCooldownSucceeds
// covers the probe-recovery half of the breaker's state machine (trip ->
// fail-fast during cooldown -> exactly one probe let through -> success
// closes the breaker), mirroring
// TestDeepgramRecognizer_CircuitBreaker_ProbeAfterCooldownSucceeds's
// coverage for the ASR side of this same circuitBreaker type.
func TestGeminiTranslator_Translate_CircuitBreaker_ProbeAfterCooldownSucceeds(t *testing.T) {
	var down int32 = 1
	srv, attempts := newCountingServer(t, func(attempt int, w http.ResponseWriter) {
		if atomic.LoadInt32(&down) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("still down"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, geminiSSEChunk("recovered", "STOP", nil))
	})
	defer srv.Close()

	const cooldown = 40 * time.Millisecond
	tr, err := NewGeminiTranslator(
		WithGeminiBaseURL(srv.URL),
		WithGeminiAPIKey("test-api-key"),
		WithGeminiCircuitBreaker(1, cooldown),
	)
	if err != nil {
		t.Fatalf("NewGeminiTranslator: %v", err)
	}

	// Trip the breaker with one fully-exhausted-retries failure.
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); err == nil {
		t.Fatal("expected the first call to fail against the always-500 server")
	}
	attemptsBeforeProbe := atomic.LoadInt32(attempts)

	// Immediately after tripping, calls must fail fast (zero additional
	// HTTP requests).
	if _, err := tr.Translate(context.Background(), "namaste", "hi", "en", true); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected the second call to be rejected by the open breaker, got: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != attemptsBeforeProbe {
		t.Errorf("HTTP attempts while cooling down = %d, want unchanged %d", got, attemptsBeforeProbe)
	}

	// Let the cooldown elapse and flip the vendor back to healthy.
	time.Sleep(cooldown + 30*time.Millisecond)
	atomic.StoreInt32(&down, 0)

	chunk, err := tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("expected the post-cooldown probe call to succeed, got: %v", err)
	}
	if chunk.Text != "recovered" {
		t.Errorf("chunk.Text = %q, want %q", chunk.Text, "recovered")
	}

	// Breaker should now be fully closed: a subsequent call must not be
	// fail-fast even against a now-failing vendor -- it should attempt a
	// real request (and, per geminiMaxAttempts, exhaust its own retries).
	atomic.StoreInt32(&down, 1)
	attemptsBeforeNext := atomic.LoadInt32(attempts)
	_, err = tr.Translate(context.Background(), "namaste", "hi", "en", true)
	if err == nil {
		t.Fatal("expected the next call to fail against the now-down vendor")
	}
	if errors.Is(err, ErrCircuitOpen) {
		t.Error("expected a real vendor failure, not an immediate circuit-open rejection, right after the breaker closed")
	}
	if got := atomic.LoadInt32(attempts) - attemptsBeforeNext; got == 0 {
		t.Error("expected real dial attempts after the breaker closed, got none (still fail-fast)")
	}
}
