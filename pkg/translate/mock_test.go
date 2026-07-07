package translate

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestMockTranslator_SupportedPairs(t *testing.T) {
	tr := NewMockTranslator()
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

func TestMockTranslator_Translate_HiToEn(t *testing.T) {
	tr := NewMockTranslator()
	ctx := context.Background()

	chunk, err := tr.Translate(ctx, "namaste", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if chunk.SourceLang != "hi" || chunk.TargetLang != "en" {
		t.Errorf("expected source=hi target=en, got source=%v target=%v", chunk.SourceLang, chunk.TargetLang)
	}
	if !chunk.IsFinal {
		t.Error("expected IsFinal=true to be propagated")
	}
	if !strings.Contains(chunk.Text, "namaste") {
		t.Errorf("expected translated text to retain original text, got %q", chunk.Text)
	}
	if !strings.HasPrefix(chunk.Text, "[EN]") {
		t.Errorf("expected translated text to be tagged with target language, got %q", chunk.Text)
	}
	if chunk.Text == "namaste" {
		t.Error("Translate must not echo input unchanged")
	}
}

func TestMockTranslator_Translate_EnToHi(t *testing.T) {
	tr := NewMockTranslator()
	ctx := context.Background()

	chunk, err := tr.Translate(ctx, "hello", "en", "hi", false)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if chunk.IsFinal {
		t.Error("expected IsFinal=false to be propagated")
	}
	if !strings.HasPrefix(chunk.Text, "[HI]") {
		t.Errorf("expected translated text tagged [HI], got %q", chunk.Text)
	}
}

func TestMockTranslator_Translate_UnsupportedPair(t *testing.T) {
	tr := NewMockTranslator()
	ctx := context.Background()

	if _, err := tr.Translate(ctx, "bonjour", "fr", "en", true); err == nil {
		t.Fatal("expected error for unsupported language pair, got nil")
	}
}

func TestMockTranslator_DoesNotSwapDirection(t *testing.T) {
	// Regression guard: translating hi->en and en->hi for the same text
	// must produce distinguishable outputs, otherwise a source/target swap
	// bug upstream would go unnoticed.
	tr := NewMockTranslator()
	ctx := context.Background()

	toEn, err := tr.Translate(ctx, "text", "hi", "en", true)
	if err != nil {
		t.Fatalf("Translate hi->en: %v", err)
	}
	toHi, err := tr.Translate(ctx, "text", "en", "hi", true)
	if err != nil {
		t.Fatalf("Translate en->hi: %v", err)
	}
	if toEn.Text == toHi.Text {
		t.Errorf("expected different output per target language, got identical %q", toEn.Text)
	}
}

// TestMockTranslator_ConcurrentUse exercises Translate from multiple
// goroutines at once; MockTranslator holds no mutable state per call so
// this should be race-free (checked under `go test -race`).
func TestMockTranslator_ConcurrentUse(t *testing.T) {
	tr := NewMockTranslator()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.Translate(ctx, "hello", "en", "hi", true); err != nil {
				t.Errorf("Translate: %v", err)
			}
		}()
	}
	wg.Wait()
}
