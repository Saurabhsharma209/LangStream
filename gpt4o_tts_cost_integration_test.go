// gpt4o_tts_cost_integration_test.go is QA's follow-up to
// cost_tracking_integration_test.go: Sprint 9's cost-tracking test only
// exercised the two ASR vendors (Deepgram, Sarvam); PE also wired
// observability.RecordCost into the three non-ASR real vendor clients
// that same day (translate.GPT4oTranslator via translate.WithMetrics,
// tts.CartesiaSynthesizer via tts.WithMetrics, and
// tts.ElevenLabsSynthesizer via tts.WithElevenLabsMetrics), and nothing
// covered those three specifically until now.
//
// FAKE-SERVER ROUND TRIP, NOT A LIVE MEASUREMENT - same caveat as
// cost_tracking_integration_test.go: every server below is a small
// in-process fake, and the private per-unit pricing constants in
// pkg/translate/gpt4o.go and pkg/tts/{cartesia,elevenlabs}.go are
// documented pricing ASSUMPTIONS, not verified billing-grade rates. This
// file never asserts an exact dollar figure against those constants --
// only correct attribution, no double-counting, and that cost scales
// with actual usage (tokens for GPT-4o, characters for Cartesia/
// ElevenLabs) rather than being a flat per-call constant.
package langstream_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// newFakeGPT4oCostServer starts a local HTTP server standing in for
// OpenAI's POST /chat/completions (streaming) endpoint. It derives its
// response deterministically from the request's own user-message length:
// it echoes back a translation exactly as long as the input text, and
// reports prompt_tokens/completion_tokens both equal to that length via
// OpenAI's documented "final usage chunk" SSE feature (see
// pkg/translate/gpt4o.go's streamOptions doc comment). This lets tests
// control exact token counts (and therefore exact expected cost ratios)
// purely by choosing the input text's length.
func newFakeGPT4oCostServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var reqBody struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		var userText string
		for _, m := range reqBody.Messages {
			if m.Role == "user" {
				userText = m.Content
			}
		}
		translated := strings.Repeat("y", len(userText))

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		contentChunk, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": translated}},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", contentChunk)

		usageChunk, _ := json.Marshal(map[string]interface{}{
			"choices": []interface{}{},
			"usage": map[string]int{
				"prompt_tokens":     len(userText),
				"completion_tokens": len(translated),
				"total_tokens":      len(userText) + len(translated),
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", usageChunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// newFakeCartesiaCostServer starts a local WebSocket server standing in
// for Cartesia's /tts/websocket endpoint. Since
// tts.CartesiaSynthesizer.SynthesizeStream records cost immediately after
// the connect+send handshake succeeds -- based on len(text), not on
// anything the server sends back -- this server's only job is to
// complete that handshake and then close out the one context promptly
// with a single done:true chunk, so callers can safely drain the
// returned channel synchronously without waiting on ctx cancellation.
func newFakeCartesiaCostServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ContextID string `json:"context_id"`
		}
		_ = json.Unmarshal(payload, &req)

		msg, _ := json.Marshal(map[string]interface{}{
			"type":       "chunk",
			"data":       base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0}),
			"done":       true,
			"context_id": req.ContextID,
		})
		_ = conn.WriteMessage(websocket.TextMessage, msg)
	})
	srv := httptest.NewServer(mux)
	return srv, wsURL(srv)
}

// newFakeElevenLabsCostServer starts a local HTTP server standing in for
// ElevenLabs' POST /v1/text-to-speech/{voice_id}/stream endpoint. Like
// Cartesia above, tts.ElevenLabsSynthesizer.SynthesizeStream records cost
// right after the request is accepted (HTTP 200), based on len(text), not
// on the response body -- so this server only needs to accept any
// /v1/text-to-speech/.../stream request and return a small fixed PCM
// payload that EOFs immediately, so the returned channel closes promptly
// and can be drained synchronously.
func newFakeElevenLabsCostServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/text-to-speech/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 320))
	})
	return httptest.NewServer(mux)
}

// drainTTSChannel reads and discards every AudioChunk from ch until it's
// closed. Both fake servers above close their one context/response
// promptly, so this returns quickly rather than blocking on ctx
// cancellation.
func drainTTSChannel(ch <-chan tts.AudioChunk) {
	for range ch {
	}
}

// TestCostTracking_GPT4oCartesiaElevenLabs_AttributedCorrectly is this
// file's main check, the same shape as
// cost_tracking_integration_test.go's
// TestCostTracking_DeepgramAndSarvam_AttributedCorrectly but for the
// translate/TTS leg of the pipeline: drive real translate.GPT4oTranslator,
// tts.CartesiaSynthesizer, and tts.ElevenLabsSynthesizer clients, sharing
// one *observability.LatencyRecorder, a different (and easy to tell
// apart) number of times each, and verify:
//
//   - each vendor's RecordCost event count exactly matches the number of
//     successful calls made against it (no double-counting);
//   - each vendor's total cost is strictly positive after real calls were
//     made (cost tracking is actually wired, not a silent no-op);
//   - "gpt-4o", "cartesia", and "elevenlabs" never leak into each other's
//     totals despite sharing the same recorder; and
//   - the same data is visible through
//     observability.BuildDashboardData/CostSnapshot, not just
//     LatencyRecorder's own CostTotal/CostEventCount accessors.
func TestCostTracking_GPT4oCartesiaElevenLabs_AttributedCorrectly(t *testing.T) {
	gpt4oSrv := newFakeGPT4oCostServer(t)
	defer gpt4oSrv.Close()
	cartesiaSrv, cartesiaWSURL := newFakeCartesiaCostServer(t)
	defer cartesiaSrv.Close()
	elevenlabsSrv := newFakeElevenLabsCostServer(t)
	defer elevenlabsSrv.Close()

	t.Setenv("CARTESIA_API_KEY", "fake-test-key")
	t.Setenv("ELEVENLABS_API_KEY", "fake-test-key")

	rec := observability.NewLatencyRecorder()

	tr, err := translate.NewGPT4oTranslator(
		translate.WithBaseURL(gpt4oSrv.URL),
		translate.WithAPIKey("fake-test-key"),
		translate.WithMetrics(rec),
	)
	if err != nil {
		t.Fatalf("NewGPT4oTranslator: %v", err)
	}
	cartesiaSynth, err := tts.NewCartesiaSynthesizer(
		tts.WithBaseURL(cartesiaWSURL),
		tts.WithDialTimeout(3*time.Second),
		tts.WithMetrics(rec),
	)
	if err != nil {
		t.Fatalf("NewCartesiaSynthesizer: %v", err)
	}
	elevenlabsSynth, err := tts.NewElevenLabsSynthesizer(
		tts.WithElevenLabsBaseURL(elevenlabsSrv.URL),
		tts.WithElevenLabsMetrics(rec),
	)
	if err != nil {
		t.Fatalf("NewElevenLabsSynthesizer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const gptCalls = 5
	const cartesiaCalls = 3
	const elevenlabsCalls = 6 // deliberately different from gptCalls/cartesiaCalls

	for i := 0; i < gptCalls; i++ {
		if _, err := tr.Translate(ctx, fmt.Sprintf("hello there %d", i), "en", "hi", true); err != nil {
			t.Fatalf("Translate[%d]: %v", i, err)
		}
	}
	for i := 0; i < cartesiaCalls; i++ {
		ch, err := cartesiaSynth.SynthesizeStream(ctx, fmt.Sprintf("namaste %d", i), tts.Persona{Language: tts.LanguageHindi})
		if err != nil {
			t.Fatalf("Cartesia SynthesizeStream[%d]: %v", i, err)
		}
		drainTTSChannel(ch)
	}
	for i := 0; i < elevenlabsCalls; i++ {
		ch, err := elevenlabsSynth.SynthesizeStream(ctx, fmt.Sprintf("hello again %d", i), tts.Persona{Language: tts.LanguageEnglish})
		if err != nil {
			t.Fatalf("ElevenLabs SynthesizeStream[%d]: %v", i, err)
		}
		drainTTSChannel(ch)
	}

	if got := rec.CostEventCount("gpt-4o"); got != gptCalls {
		t.Errorf("CostEventCount(\"gpt-4o\") = %d, want %d (one RecordCost per successful Translate call -- a mismatch indicates missing or double cost recording)", got, gptCalls)
	}
	if got := rec.CostEventCount("cartesia"); got != cartesiaCalls {
		t.Errorf("CostEventCount(\"cartesia\") = %d, want %d (one RecordCost per successful SynthesizeStream call -- a mismatch indicates missing or double cost recording)", got, cartesiaCalls)
	}
	if got := rec.CostEventCount("elevenlabs"); got != elevenlabsCalls {
		t.Errorf("CostEventCount(\"elevenlabs\") = %d, want %d (one RecordCost per successful SynthesizeStream call -- a mismatch indicates missing or double cost recording)", got, elevenlabsCalls)
	}

	gptTotal := rec.CostTotal("gpt-4o")
	cartesiaTotal := rec.CostTotal("cartesia")
	elevenlabsTotal := rec.CostTotal("elevenlabs")
	if gptTotal <= 0 {
		t.Errorf("CostTotal(\"gpt-4o\") = %v, want > 0 after %d real Translate calls", gptTotal, gptCalls)
	}
	if cartesiaTotal <= 0 {
		t.Errorf("CostTotal(\"cartesia\") = %v, want > 0 after %d real SynthesizeStream calls", cartesiaTotal, cartesiaCalls)
	}
	if elevenlabsTotal <= 0 {
		t.Errorf("CostTotal(\"elevenlabs\") = %v, want > 0 after %d real SynthesizeStream calls", elevenlabsTotal, elevenlabsCalls)
	}

	// Cross-attribution check: nothing recorded for any of the three
	// vendors should have leaked in under an unrelated/misspelled vendor
	// string, and no vendor from cost_tracking_integration_test.go's ASR
	// pair ("deepgram"/"sarvam") should appear here either, since this
	// test never touches pkg/asr.
	snapshot := rec.CostSnapshot()
	seenVendors := make(map[string]observability.CostStats, len(snapshot))
	for _, cs := range snapshot {
		seenVendors[cs.Vendor] = cs
	}
	if len(snapshot) != 3 {
		t.Fatalf("CostSnapshot() returned %d vendor entries (%+v), want exactly 3 (gpt-4o, cartesia, elevenlabs) -- an extra/misspelled vendor key would indicate a wrong-vendor-string bug", len(snapshot), snapshot)
	}
	gptStats, ok := seenVendors["gpt-4o"]
	if !ok {
		t.Fatalf("CostSnapshot() has no \"gpt-4o\" entry: %+v", snapshot)
	}
	cartesiaStats, ok := seenVendors["cartesia"]
	if !ok {
		t.Fatalf("CostSnapshot() has no \"cartesia\" entry: %+v", snapshot)
	}
	elevenlabsStats, ok := seenVendors["elevenlabs"]
	if !ok {
		t.Fatalf("CostSnapshot() has no \"elevenlabs\" entry: %+v", snapshot)
	}
	if gptStats.Events != gptCalls {
		t.Errorf("CostSnapshot()'s gpt-4o entry Events = %d, want %d", gptStats.Events, gptCalls)
	}
	if cartesiaStats.Events != cartesiaCalls {
		t.Errorf("CostSnapshot()'s cartesia entry Events = %d, want %d", cartesiaStats.Events, cartesiaCalls)
	}
	if elevenlabsStats.Events != elevenlabsCalls {
		t.Errorf("CostSnapshot()'s elevenlabs entry Events = %d, want %d", elevenlabsStats.Events, elevenlabsCalls)
	}
	if gptStats.TotalUSD != gptTotal {
		t.Errorf("CostSnapshot()'s gpt-4o TotalUSD = %v, want %v (matching CostTotal(\"gpt-4o\"))", gptStats.TotalUSD, gptTotal)
	}
	if cartesiaStats.TotalUSD != cartesiaTotal {
		t.Errorf("CostSnapshot()'s cartesia TotalUSD = %v, want %v (matching CostTotal(\"cartesia\"))", cartesiaStats.TotalUSD, cartesiaTotal)
	}
	if elevenlabsStats.TotalUSD != elevenlabsTotal {
		t.Errorf("CostSnapshot()'s elevenlabs TotalUSD = %v, want %v (matching CostTotal(\"elevenlabs\"))", elevenlabsStats.TotalUSD, elevenlabsTotal)
	}

	// The same numbers must be visible through the actual seam
	// observability.BuildDashboardData -- if it ever
	// filtered/renamed/dropped cost entries, this would catch it even
	// though rec.CostTotal above wouldn't.
	dash := observability.BuildDashboardData(rec)
	if len(dash.Costs) != 3 {
		t.Fatalf("BuildDashboardData(rec).Costs has %d entries, want 3: %+v", len(dash.Costs), dash.Costs)
	}
	var dashGPT, dashCartesia, dashElevenLabs *observability.CostStats
	for i := range dash.Costs {
		switch dash.Costs[i].Vendor {
		case "gpt-4o":
			dashGPT = &dash.Costs[i]
		case "cartesia":
			dashCartesia = &dash.Costs[i]
		case "elevenlabs":
			dashElevenLabs = &dash.Costs[i]
		}
	}
	if dashGPT == nil {
		t.Fatalf("BuildDashboardData(rec).Costs is missing a \"gpt-4o\" entry: %+v", dash.Costs)
	}
	if dashCartesia == nil {
		t.Fatalf("BuildDashboardData(rec).Costs is missing a \"cartesia\" entry: %+v", dash.Costs)
	}
	if dashElevenLabs == nil {
		t.Fatalf("BuildDashboardData(rec).Costs is missing an \"elevenlabs\" entry: %+v", dash.Costs)
	}
	if dashGPT.TotalUSD != gptTotal || dashGPT.Events != gptCalls {
		t.Errorf("BuildDashboardData(rec)'s gpt-4o entry = %+v, want TotalUSD=%v Events=%d", *dashGPT, gptTotal, gptCalls)
	}
	if dashCartesia.TotalUSD != cartesiaTotal || dashCartesia.Events != cartesiaCalls {
		t.Errorf("BuildDashboardData(rec)'s cartesia entry = %+v, want TotalUSD=%v Events=%d", *dashCartesia, cartesiaTotal, cartesiaCalls)
	}
	if dashElevenLabs.TotalUSD != elevenlabsTotal || dashElevenLabs.Events != elevenlabsCalls {
		t.Errorf("BuildDashboardData(rec)'s elevenlabs entry = %+v, want TotalUSD=%v Events=%d", *dashElevenLabs, elevenlabsTotal, elevenlabsCalls)
	}
}

// TestCostTracking_ScalesWithUsageNotFlatPerCall guards against a
// different, easy-to-get-wrong shape of bug than the attribution test
// above: a cost formula that is secretly a flat per-call constant, or one
// that uses the wrong unit entirely. Unlike
// cost_tracking_integration_test.go's audio-duration version of this same
// check (which tolerates a 5% band because PCM byte count only
// approximates duration), this version's inputs are constructed so the
// expected ratio is exact: doubling len(text) exactly doubles Cartesia's/
// ElevenLabs' per-character cost, and (via newFakeGPT4oCostServer's
// token-count-equals-text-length construction) exactly doubles both of
// GPT-4o's prompt and completion token counts, hence its cost too, no
// matter what the underlying per-token rate mix is.
func TestCostTracking_ScalesWithUsageNotFlatPerCall(t *testing.T) {
	const relTolerance = 0.01 // generous but still catches a flat-per-call (ratio ~1.0) or wrong-unit formula.

	t.Run("gpt-4o", func(t *testing.T) {
		srv := newFakeGPT4oCostServer(t)
		defer srv.Close()

		rec := observability.NewLatencyRecorder()
		tr, err := translate.NewGPT4oTranslator(
			translate.WithBaseURL(srv.URL),
			translate.WithAPIKey("fake-test-key"),
			translate.WithMetrics(rec),
		)
		if err != nil {
			t.Fatalf("NewGPT4oTranslator: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		shortText := strings.Repeat("a", 50)
		longText := strings.Repeat("a", 100) // exactly double shortText's length

		if _, err := tr.Translate(ctx, shortText, "en", "hi", true); err != nil {
			t.Fatalf("Translate(short): %v", err)
		}
		shortCost := rec.CostTotal("gpt-4o")
		if shortCost <= 0 {
			t.Fatalf("cost after one short Translate call = %v, want > 0", shortCost)
		}

		if _, err := tr.Translate(ctx, longText, "en", "hi", true); err != nil {
			t.Fatalf("Translate(long): %v", err)
		}
		longCost := rec.CostTotal("gpt-4o") - shortCost

		assertRatioNear2(t, "gpt-4o", shortCost, longCost, relTolerance)
	})

	t.Run("cartesia", func(t *testing.T) {
		srv, wsURLStr := newFakeCartesiaCostServer(t)
		defer srv.Close()
		t.Setenv("CARTESIA_API_KEY", "fake-test-key")

		rec := observability.NewLatencyRecorder()
		synth, err := tts.NewCartesiaSynthesizer(tts.WithBaseURL(wsURLStr), tts.WithMetrics(rec))
		if err != nil {
			t.Fatalf("NewCartesiaSynthesizer: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		shortText := strings.Repeat("b", 50)
		longText := strings.Repeat("b", 100)

		ch, err := synth.SynthesizeStream(ctx, shortText, tts.Persona{Language: tts.LanguageEnglish})
		if err != nil {
			t.Fatalf("SynthesizeStream(short): %v", err)
		}
		drainTTSChannel(ch)
		shortCost := rec.CostTotal("cartesia")
		if shortCost <= 0 {
			t.Fatalf("cost after one short SynthesizeStream call = %v, want > 0", shortCost)
		}

		ch, err = synth.SynthesizeStream(ctx, longText, tts.Persona{Language: tts.LanguageEnglish})
		if err != nil {
			t.Fatalf("SynthesizeStream(long): %v", err)
		}
		drainTTSChannel(ch)
		longCost := rec.CostTotal("cartesia") - shortCost

		assertRatioNear2(t, "cartesia", shortCost, longCost, relTolerance)
	})

	t.Run("elevenlabs", func(t *testing.T) {
		srv := newFakeElevenLabsCostServer(t)
		defer srv.Close()
		t.Setenv("ELEVENLABS_API_KEY", "fake-test-key")

		rec := observability.NewLatencyRecorder()
		synth, err := tts.NewElevenLabsSynthesizer(tts.WithElevenLabsBaseURL(srv.URL), tts.WithElevenLabsMetrics(rec))
		if err != nil {
			t.Fatalf("NewElevenLabsSynthesizer: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		shortText := strings.Repeat("c", 50)
		longText := strings.Repeat("c", 100)

		ch, err := synth.SynthesizeStream(ctx, shortText, tts.Persona{Language: tts.LanguageEnglish})
		if err != nil {
			t.Fatalf("SynthesizeStream(short): %v", err)
		}
		drainTTSChannel(ch)
		shortCost := rec.CostTotal("elevenlabs")
		if shortCost <= 0 {
			t.Fatalf("cost after one short SynthesizeStream call = %v, want > 0", shortCost)
		}

		ch, err = synth.SynthesizeStream(ctx, longText, tts.Persona{Language: tts.LanguageEnglish})
		if err != nil {
			t.Fatalf("SynthesizeStream(long): %v", err)
		}
		drainTTSChannel(ch)
		longCost := rec.CostTotal("elevenlabs") - shortCost

		assertRatioNear2(t, "elevenlabs", shortCost, longCost, relTolerance)
	})
}

// assertRatioNear2 fails t if longCost/shortCost isn't within relTolerance
// of 2.0 -- shared by all three subtests in
// TestCostTracking_ScalesWithUsageNotFlatPerCall above, each of which
// doubles its unit of billing (tokens for GPT-4o, characters for
// Cartesia/ElevenLabs) between the "short" and "long" call.
func assertRatioNear2(t *testing.T, vendor string, shortCost, longCost, relTolerance float64) {
	t.Helper()
	ratio := longCost / shortCost
	const wantRatio = 2.0
	if ratio < wantRatio*(1-relTolerance) || ratio > wantRatio*(1+relTolerance) {
		t.Errorf("%s: doubling the billed unit (tokens/characters) cost %v after the first call cost %v (ratio %.4f), want ratio ~%.1f -- cost does not appear to scale with usage (possible flat-per-call or wrong-unit cost formula)", vendor, longCost, shortCost, ratio, wantRatio)
	}
}
