package translate

import (
	"context"
	"fmt"
	"strings"
)

// MockTranslator is a deterministic, in-memory implementation of Translator.
// It performs no real translation. Instead it tags the input text with the
// target language, e.g. Translate(ctx, "hello", "en", "hi", true) returns
// Chunk{Text: "[HI] hello"}. The tag is deliberately visible and derived
// from the *target* language so tests (and humans) can immediately tell
// whether source/target were swapped somewhere upstream — silently echoing
// the input back unchanged would hide exactly that class of bug.
type MockTranslator struct {
	pairs [][2]Language
}

// NewMockTranslator returns a MockTranslator supporting the given
// (source, target) pairs. If none are given, it defaults to hi->en and
// en->hi, the pilot's one supported language pair (see ROADMAP.md).
func NewMockTranslator(pairs ...[2]Language) *MockTranslator {
	if len(pairs) == 0 {
		pairs = [][2]Language{
			{"hi", "en"},
			{"en", "hi"},
		}
	}
	cp := make([][2]Language, len(pairs))
	copy(cp, pairs)
	return &MockTranslator{pairs: cp}
}

// Name implements Translator.
func (m *MockTranslator) Name() string { return "mock" }

// SupportedPairs implements Translator.
func (m *MockTranslator) SupportedPairs() [][2]Language {
	out := make([][2]Language, len(m.pairs))
	copy(out, m.pairs)
	return out
}

// Translate implements Translator. It does a deterministic, reversible
// transform: it prefixes the text with "[<TARGET-LANG-UPPERCASE>] " so
// callers can assert both that translation happened and which target
// language it happened into.
func (m *MockTranslator) Translate(ctx context.Context, text string, source, target Language, isFinal bool) (Chunk, error) {
	select {
	case <-ctx.Done():
		return Chunk{}, ctx.Err()
	default:
	}

	if !m.supports(source, target) {
		return Chunk{}, fmt.Errorf("translate/mock: unsupported pair %q->%q", source, target)
	}

	translated := fmt.Sprintf("[%s] %s", strings.ToUpper(string(target)), text)
	return Chunk{
		Text:       translated,
		SourceLang: source,
		TargetLang: target,
		IsFinal:    isFinal,
	}, nil
}

func (m *MockTranslator) supports(source, target Language) bool {
	for _, p := range m.pairs {
		srcOK := p[0] == source || p[0] == ""
		if srcOK && p[1] == target {
			return true
		}
	}
	return false
}

var _ Translator = (*MockTranslator)(nil)
