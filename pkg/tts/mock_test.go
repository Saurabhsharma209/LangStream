package tts

import (
	"context"
	"testing"
	"time"
)

func TestMockSynthesizer_SupportedLanguages(t *testing.T) {
	s := NewMockSynthesizer()
	langs := s.SupportedLanguages()

	want := map[Language]bool{"en": false, "hi": false}
	for _, l := range langs {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, found := range want {
		if !found {
			t.Errorf("expected SupportedLanguages() to include %q, got %v", l, langs)
		}
	}
}

func TestMockSynthesizer_SynthesizeStream_EndToEnd(t *testing.T) {
	s := NewMockSynthesizer()
	ctx := context.Background()
	persona := Persona{VoiceID: "agent-1", Language: "en", Gender: "neutral"}

	ch, err := s.SynthesizeStream(ctx, "hello there how are you today", persona)
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	var chunks []AudioChunk
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				break loop
			}
			chunks = append(chunks, c)
		case <-timeout:
			t.Fatal("timed out waiting for audio chunks; SynthesizeStream may be hanging")
		}
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least one audio chunk")
	}

	for i, c := range chunks {
		if len(c.PCM) != mockFrameBytes {
			t.Errorf("chunk %d: expected %d bytes of PCM, got %d", i, mockFrameBytes, len(c.PCM))
		}
		if c.SampleRate != mockSampleRate {
			t.Errorf("chunk %d: expected sample rate %d, got %d", i, mockSampleRate, c.SampleRate)
		}
		isLast := i == len(chunks)-1
		if c.IsFinal != isLast {
			t.Errorf("chunk %d: IsFinal=%v, expected %v (only the last chunk should be final)", i, c.IsFinal, isLast)
		}
	}
}

func TestMockSynthesizer_UnsupportedLanguage(t *testing.T) {
	s := NewMockSynthesizer("en", "hi")
	ctx := context.Background()
	persona := Persona{VoiceID: "agent-1", Language: "fr"}

	if _, err := s.SynthesizeStream(ctx, "bonjour", persona); err == nil {
		t.Fatal("expected error for unsupported language, got nil")
	}
}

func TestMockSynthesizer_EmptyText(t *testing.T) {
	s := NewMockSynthesizer()
	ctx := context.Background()
	persona := Persona{VoiceID: "agent-1", Language: "en"}

	if _, err := s.SynthesizeStream(ctx, "", persona); err == nil {
		t.Fatal("expected error for empty text, got nil")
	}
}

func TestMockSynthesizer_ContextCancelDoesNotHang(t *testing.T) {
	s := NewMockSynthesizer()
	ctx, cancel := context.WithCancel(context.Background())
	persona := Persona{VoiceID: "agent-1", Language: "hi"}

	ch, err := s.SynthesizeStream(ctx, "this is a reasonably long sentence with many words in it", persona)
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}

	// Read one chunk, then cancel; the channel must still close promptly
	// instead of hanging the producer goroutine forever.
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}
	cancel()

	closed := false
	timeout := time.After(1 * time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-timeout:
			t.Fatal("channel did not close after context cancellation")
		}
	}
}

func TestMockSynthesizer_LongerTextProducesMoreChunks(t *testing.T) {
	s := NewMockSynthesizer()
	ctx := context.Background()
	persona := Persona{VoiceID: "agent-1", Language: "en"}

	short, err := s.SynthesizeStream(ctx, "hi", persona)
	if err != nil {
		t.Fatalf("SynthesizeStream(short): %v", err)
	}
	shortCount := drainCount(t, short)

	long, err := s.SynthesizeStream(ctx, "this is a much longer sentence with quite a few more words in it than the short one", persona)
	if err != nil {
		t.Fatalf("SynthesizeStream(long): %v", err)
	}
	longCount := drainCount(t, long)

	if longCount <= shortCount {
		t.Errorf("expected longer text to produce more chunks: short=%d long=%d", shortCount, longCount)
	}
}

func drainCount(t *testing.T, ch <-chan AudioChunk) int {
	t.Helper()
	count := 0
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return count
			}
			count++
		case <-timeout:
			t.Fatal("timed out draining channel")
		}
	}
}
