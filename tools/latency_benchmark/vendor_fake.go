// This file backs latency_benchmark's -vendor-fake mode: instead of PE's
// Week 1 in-memory mocks (asr.MockRecognizer, translate.MockTranslator,
// tts.MockSynthesizer), it spins up small in-process fake HTTP/WebSocket
// servers standing in for Sarvam (ASR), GPT-4o (MT), and Cartesia (TTS),
// and points the *real* vendor client code at them (asr.NewSarvamRecognizer,
// translate.NewGPT4oTranslator, tts.NewCartesiaSynthesizer). This is the
// same fake-server pattern QA's integration_vendor_test.go uses at the
// repo root, duplicated here rather than imported because Go test helpers
// in _test.go files aren't available to a non-test binary like this one.
//
// It measures fake-server round-trip latency (real client marshaling,
// real WebSocket/HTTP framing, real goroutine plumbing - just no actual
// network hop to a vendor) as a proxy until live vendor API keys exist
// (see ROADMAP.md's Week 2 decision, 2026-07-07). It is not a substitute
// for real vendor latency numbers, only a better proxy than the pure
// in-memory mocks: it exercises every line of client code that will run
// against the real vendor, just against a local server that answers
// instantly instead of over the real internet.
//
// Sarvam (not Deepgram) is used for the ASR leg specifically because
// langstream.SessionConfig.ASR is one Recognizer shared by both legs
// (caller "hi", agent "en"), and Sarvam is the only ASR backend that
// advertises both languages (Deepgram only supports "en").
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

// fakeVendorServers holds the three local servers -vendor-fake mode wires
// the real vendor clients up against, plus their URLs in the shape each
// client's WithBaseURL-style option expects.
type fakeVendorServers struct {
	sarvam   *httptest.Server
	gpt4o    *httptest.Server
	cartesia *httptest.Server

	SarvamWSURL   string
	GPT4oHTTPURL  string
	CartesiaWSURL string
}

// startFakeVendorServers starts all three fake servers. Every Sarvam
// connection gets the same fixed transcript, every GPT-4o request gets the
// same fixed translation, and every Cartesia connection gets the same
// fixed two-chunk PCM response: -vendor-fake mode is measuring pipeline/
// client-code latency, not transcription accuracy, so canned, deterministic
// responses (rather than anything audio-content-aware) are the right
// choice here.
func startFakeVendorServers() *fakeVendorServers {
	const (
		hindiUtterance     = "मुझे मदद चाहिए"
		englishTranslation = "I need help"
	)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	sarvamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			result, _ := json.Marshal(map[string]interface{}{
				"type": "data",
				"data": map[string]interface{}{
					"request_id": "latency-benchmark",
					"transcript": hindiUtterance,
					"metrics":    map[string]float64{"audio_duration": 0.5, "processing_latency": 0.01},
				},
			})
			if err := conn.WriteMessage(websocket.TextMessage, result); err != nil {
				return
			}
		}
	}))

	gpt4oSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": englishTranslation}},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))

	cartesiaPCM := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8, 9, 10}}
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
		for i, pcm := range cartesiaPCM {
			msg, _ := json.Marshal(map[string]interface{}{
				"type":       "chunk",
				"data":       base64.StdEncoding.EncodeToString(pcm),
				"done":       i == len(cartesiaPCM)-1,
				"context_id": req.ContextID,
			})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	})
	cartesiaSrv := httptest.NewServer(mux)

	return &fakeVendorServers{
		sarvam:        sarvamSrv,
		gpt4o:         gpt4oSrv,
		cartesia:      cartesiaSrv,
		SarvamWSURL:   "ws" + strings.TrimPrefix(sarvamSrv.URL, "http") + "/speech-to-text/ws",
		GPT4oHTTPURL:  gpt4oSrv.URL,
		CartesiaWSURL: "ws" + strings.TrimPrefix(cartesiaSrv.URL, "http"),
	}
}

// Close shuts all three fake servers down.
func (f *fakeVendorServers) Close() {
	f.sarvam.Close()
	f.gpt4o.Close()
	f.cartesia.Close()
}

// setFakeVendorAPIKeys sets the environment variables the real vendor
// constructors require (SARVAM_API_KEY, CARTESIA_API_KEY have no
// WithAPIKey-style option, unlike GPT-4o's WithAPIKey) to a fixed
// benchmark-only placeholder value. This mutates the current process's
// environment, which is acceptable for a standalone CLI tool invoked once
// per run, but would not be for a long-lived service.
func setFakeVendorAPIKeys() {
	os.Setenv("SARVAM_API_KEY", "fake-benchmark-key")
	os.Setenv("CARTESIA_API_KEY", "fake-benchmark-key")
}
