package langstream

import (
	"testing"

	"github.com/exotel/langstream/pkg/tts"
)

func TestPersonaManagerFallback(t *testing.T) {
	pm := NewPersonaManager()

	got := pm.Get("hi")
	if got.VoiceID == "" {
		t.Fatal("expected a non-empty fallback VoiceID")
	}
	if got.Language != "hi" {
		t.Fatalf("fallback Language = %q, want %q", got.Language, "hi")
	}
	if got.Gender != "neutral" {
		t.Fatalf("fallback Gender = %q, want %q", got.Gender, "neutral")
	}
}

func TestPersonaManagerSetOverridesFallback(t *testing.T) {
	pm := NewPersonaManager()
	want := tts.Persona{VoiceID: "custom-voice", Language: "en", Gender: "male"}
	pm.Set("en", want)

	got := pm.Get("en")
	if got != want {
		t.Fatalf("Get(en) = %+v, want %+v", got, want)
	}

	// Unrelated languages remain unaffected.
	other := pm.Get("hi")
	if other.VoiceID == want.VoiceID {
		t.Fatal("Set(en, ...) leaked into Get(hi)")
	}
}

func TestPersonaManagerConcurrentAccess(t *testing.T) {
	pm := NewPersonaManager()
	done := make(chan struct{})

	go func() {
		for i := 0; i < 1000; i++ {
			pm.Set("en", tts.Persona{VoiceID: "voice-a", Language: "en"})
		}
		close(done)
	}()

	for i := 0; i < 1000; i++ {
		_ = pm.Get("en")
	}
	<-done
}
