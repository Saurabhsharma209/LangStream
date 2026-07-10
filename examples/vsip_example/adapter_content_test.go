// QA's addition to this example's test coverage. adapter_test.go (Tech's
// original tests) already proves VSIPCallAdapter composes with a real
// langstream.Session end to end on both legs at once (see
// TestVSIPCallAdapter_Playback_DeliversUntilSessionCloses): it pushes
// caller and agent audio through the adapter concurrently with the
// session, closes it, and checks each playback pump delivers "at least
// one chunk, with the last one IsFinal" - i.e. that *something* comes out
// the other side.
//
// What that leaves unchecked is *content*: whether what comes out is
// actually the right thing, not just non-empty. Because every backend
// here is deterministic (see pkg/asr/mock.go's mockPhrases doc comment:
// "part of the mock's observable contract... other workstreams (QA in
// particular) may assert on them directly", translate/mock.go's
// "[<TARGET>] <text>" tagging, and tts/mock.go's word-count-driven chunk
// pacing + fakePCM(i) deterministic payloads), the exact chunk count and
// exact PCM bytes the adapter should deliver on each leg are fully
// computable ahead of time. This test computes them and asserts on them
// exactly, so a bug that silently swapped which leg's translation went
// where, dropped/duplicated a chunk, or corrupted PCM in transit through
// VSIPCallAdapter's pump/playback plumbing would be caught - none of
// which "at least one chunk arrived" can detect.
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/tts"
)

// Mirrors pkg/asr/mock.go's mockPhrases - a canned transcript per
// language, called out there as an intentionally stable, assertable
// contract.
const (
	mockHindiPhrase   = "नमस्ते, यह एक परीक्षण कॉल है"
	mockEnglishPhrase = "hello, this is a test call"
)

// mockWordsPerChunk and mockFrameBytes mirror the unexported constants of
// the same name in pkg/tts/mock.go (this package can't import them
// directly - they're unexported in package tts), reproduced here so this
// test can independently compute the exact chunk count/content the mock
// synthesizer must produce for a given input text.
const (
	mockWordsPerChunkLocal = 3
	mockFrameBytesLocal    = 320
)

// expectedMockTTSChunkCount mirrors tts.MockSynthesizer.SynthesizeStream's
// numChunks calculation exactly.
func expectedMockTTSChunkCount(text string) int {
	n := len(strings.Fields(text))/mockWordsPerChunkLocal + 1
	if n < 1 {
		n = 1
	}
	return n
}

// expectedMockFakePCM mirrors tts/mock.go's fakePCM(i) exactly.
func expectedMockFakePCM(i int) []byte {
	b := make([]byte, mockFrameBytesLocal)
	for j := range b {
		b[j] = byte((i + j) % 256)
	}
	return b
}

// mockTranslatedText mirrors translate.MockTranslator.Translate's tagging
// exactly: "[<TARGET-UPPER>] <text>".
func mockTranslatedText(target, text string) string {
	return fmt.Sprintf("[%s] %s", strings.ToUpper(target), text)
}

func pcmEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestVSIPCallAdapter_ContentCorrectness_RealSessionEndToEnd pushes one
// caller utterance and one agent utterance through VSIPCallAdapter against
// a real langstream.Session (mock backends, hi caller / en agent - the
// same newDemoSession helper main.go's own demo and adapter_test.go use),
// closes the session, and asserts both playback legs deliver *exactly* the
// chunk count and PCM bytes the deterministic mock ASR -> MT -> TTS chain
// must produce - not just "at least one chunk", which is what
// adapter_test.go's existing dual-leg test already covers.
func TestVSIPCallAdapter_ContentCorrectness_RealSessionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx) // CallerLanguage: "hi", AgentLanguage: "en"
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}

	adapter := NewVSIPCallAdapter(sess, 8000)

	// The caller speaks (hi); the agent leg hears it translated into "en".
	wantAgentHearsText := mockTranslatedText("en", mockHindiPhrase)
	wantAgentChunks := expectedMockTTSChunkCount(wantAgentHearsText)

	// The agent speaks (en); the caller leg hears it translated into "hi".
	wantCallerHearsText := mockTranslatedText("hi", mockEnglishPhrase)
	wantCallerChunks := expectedMockTTSChunkCount(wantCallerHearsText)

	agentPlaybackDone := make(chan []tts.AudioChunk, 1)
	callerPlaybackDone := make(chan []tts.AudioChunk, 1)

	go func() {
		var got []tts.AudioChunk
		_, err := adapter.PumpAgentPlayback(ctx, func(chunk tts.AudioChunk) error {
			got = append(got, chunk)
			return nil
		})
		if err != nil {
			t.Errorf("PumpAgentPlayback: %v", err)
		}
		agentPlaybackDone <- got
	}()
	go func() {
		var got []tts.AudioChunk
		_, err := adapter.PumpCallerPlayback(ctx, func(chunk tts.AudioChunk) error {
			got = append(got, chunk)
			return nil
		})
		if err != nil {
			t.Errorf("PumpCallerPlayback: %v", err)
		}
		callerPlaybackDone <- got
	}()

	if _, err := adapter.PumpCallerAudio(ctx, fakeAudioSource(1)); err != nil {
		t.Fatalf("PumpCallerAudio: %v", err)
	}
	if _, err := adapter.PumpAgentAudio(ctx, fakeAudioSource(1)); err != nil {
		t.Fatalf("PumpAgentAudio: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("closing session: %v", err)
	}

	var agentChunks, callerChunks []tts.AudioChunk
	select {
	case agentChunks = <-agentPlaybackDone:
	case <-time.After(3 * time.Second):
		t.Fatal("agent playback pump did not return after session close")
	}
	select {
	case callerChunks = <-callerPlaybackDone:
	case <-time.After(3 * time.Second):
		t.Fatal("caller playback pump did not return after session close")
	}

	checkChunks := func(t *testing.T, label string, got []tts.AudioChunk, wantCount int) {
		t.Helper()
		if len(got) != wantCount {
			t.Fatalf("%s: got %d chunk(s), want exactly %d", label, len(got), wantCount)
		}
		for i, c := range got {
			if c.SampleRate != 8000 {
				t.Errorf("%s: chunk[%d].SampleRate = %d, want 8000", label, i, c.SampleRate)
			}
			wantFinal := i == len(got)-1
			if c.IsFinal != wantFinal {
				t.Errorf("%s: chunk[%d].IsFinal = %v, want %v", label, i, c.IsFinal, wantFinal)
			}
			if !pcmEqual(c.PCM, expectedMockFakePCM(i)) {
				t.Errorf("%s: chunk[%d].PCM does not match the deterministic mock TTS payload for index %d", label, i, i)
			}
		}
	}

	checkChunks(t, "agent playback (translated caller speech)", agentChunks, wantAgentChunks)
	checkChunks(t, "caller playback (translated agent speech)", callerChunks, wantCallerChunks)
}
