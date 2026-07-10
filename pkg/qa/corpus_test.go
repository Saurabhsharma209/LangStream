package qa

import (
	"math"
	"testing"
)

// TestFixedCorpus_EntriesAreWellFormed guards against the exact class of
// silent-fixture bug this repo has been bitten by before (an empty/
// malformed fixture that "passes" trivially): every entry must have a
// name, language, non-empty reference/hypothesis text, and a non-empty PCM
// frame, and names must be unique so a future accidental duplicate doesn't
// silently shadow another entry in test output.
func TestFixedCorpus_EntriesAreWellFormed(t *testing.T) {
	entries := FixedCorpus()
	if len(entries) < 3 {
		t.Fatalf("FixedCorpus() returned %d entries, want at least 3", len(entries))
	}

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			t.Errorf("entry has empty Name (reference=%q)", e.Reference)
		}
		if seen[e.Name] {
			t.Errorf("duplicate corpus entry name %q", e.Name)
		}
		seen[e.Name] = true

		if e.Language == "" {
			t.Errorf("entry %q has empty Language", e.Name)
		}
		if e.Reference == "" {
			t.Errorf("entry %q has empty Reference", e.Name)
		}
		if e.Hypothesis == "" {
			t.Errorf("entry %q has empty Hypothesis", e.Name)
		}
		if len(e.PCM) == 0 {
			t.Errorf("entry %q has empty PCM", e.Name)
		}
		if e.SampleRate <= 0 {
			t.Errorf("entry %q has non-positive SampleRate %d", e.Name, e.SampleRate)
		}
	}
}

// TestFixedCorpus_PrecomputedWERMatches locks in the by-hand WER
// computation documented on FixedCorpus for each entry, so a future edit
// to the corpus strings that silently changes the intended error shape
// (e.g. turns a one-word substitution into a two-word one) fails loudly
// here instead of only showing up as an unexplained drift in
// wer_measurement_test.go's reported numbers.
func TestFixedCorpus_PrecomputedWERMatches(t *testing.T) {
	want := map[string]float64{
		"identical_greeting":    0.0,
		"one_word_substitution": 1.0 / 5.0,
		"one_word_deletion":     1.0 / 7.0,
	}

	entries := FixedCorpus()
	if len(want) != len(entries) {
		t.Fatalf("test has expectations for %d entries but FixedCorpus() returned %d — update this test alongside FixedCorpus", len(want), len(entries))
	}

	for _, e := range entries {
		expected, ok := want[e.Name]
		if !ok {
			t.Errorf("no expected WER registered for corpus entry %q — update this test alongside FixedCorpus", e.Name)
			continue
		}
		got := WordErrorRate(e.Reference, e.Hypothesis)
		if math.Abs(got-expected) > werEpsilon {
			t.Errorf("entry %q: WordErrorRate(reference, hypothesis) = %v, want %v", e.Name, got, expected)
		}
	}
}
