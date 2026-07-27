package qa

import (
	"math"
	"testing"
)

// TestFixedTranslationCorpus_EntriesAreWellFormed guards against the same
// class of silent-fixture bug corpus_test.go's
// TestFixedCorpus_EntriesAreWellFormed guards ASR entries against: every
// translation entry must have a name, both language tags, and non-empty
// Source/Reference/Candidate text, and names must be unique.
func TestFixedTranslationCorpus_EntriesAreWellFormed(t *testing.T) {
	entries := FixedTranslationCorpus()
	if len(entries) < 6 {
		t.Fatalf("FixedTranslationCorpus() returned %d entries, want at least 6", len(entries))
	}

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			t.Errorf("entry has empty Name (source=%q)", e.Source)
		}
		if seen[e.Name] {
			t.Errorf("duplicate translation corpus entry name %q", e.Name)
		}
		seen[e.Name] = true

		if e.SourceLanguage == "" {
			t.Errorf("entry %q has empty SourceLanguage", e.Name)
		}
		if e.TargetLanguage == "" {
			t.Errorf("entry %q has empty TargetLanguage", e.Name)
		}
		if e.Source == "" {
			t.Errorf("entry %q has empty Source", e.Name)
		}
		if e.Reference == "" {
			t.Errorf("entry %q has empty Reference", e.Name)
		}
		if e.Candidate == "" {
			t.Errorf("entry %q has empty Candidate", e.Name)
		}
	}
}

// TestFixedTranslationCorpus_PrecomputedBLEUMatches locks in the by-hand
// BLEU computation documented on FixedTranslationCorpus for each entry --
// the BLEU counterpart to corpus_test.go's
// TestFixedCorpus_PrecomputedWERMatches -- so a future edit to the corpus
// strings that silently changes the intended error/overlap shape fails
// loudly here.
func TestFixedTranslationCorpus_PrecomputedBLEUMatches(t *testing.T) {
	// perfect_identical_translation: Candidate == Reference exactly, all
	// four precisions 1.0, BP 1.0 -> BLEU 1.0.
	wantPerfectIdentical := 1.0

	// one_word_substitution_currency_mismatch: 8-word Reference/Candidate,
	// differing only in the final word ("rupees" vs "dollars"). Because
	// the mismatch sits at the very last token, it breaks exactly one
	// n-gram at every order:
	//   n=1: 8 total, 7 match -> p1 = 7/8.
	//   n=2: 7 total, 6 match -> p2 = 6/7.
	//   n=3: 6 total, 5 match -> p3 = 5/6.
	//   n=4: 5 total, 4 match -> p4 = 4/5.
	// BP = 1.0 (equal length, 8 == 8).
	oneSubP1 := 7.0 / 8.0
	oneSubP2 := 6.0 / 7.0
	oneSubP3 := 5.0 / 6.0
	oneSubP4 := 4.0 / 5.0
	wantOneWordSubstitution := math.Exp((math.Log(oneSubP1) + math.Log(oneSubP2) + math.Log(oneSubP3) + math.Log(oneSubP4)) / 4.0)

	// missing_clause_deletion_within_48_hours: 10-word Candidate is an
	// exact prefix of the 13-word Reference (the trailing "within 48
	// hours" clause is dropped). Every candidate n-gram at every order
	// matches the reference exactly, so all four precisions are 1.0; the
	// entire shortfall is the brevity penalty: BP = exp(1 - 13/10).
	wantClauseDeletion := math.Exp(1.0 - 13.0/10.0)

	// completely_unrelated_candidate: 6-word Reference and 6-word
	// Candidate share zero words at any n-gram order. All four
	// precisions are epsilon-smoothed (denominators are the candidate's
	// n-gram counts for a 6-word candidate: 6, 5, 4, 3):
	//   p1 = 0.1/6, p2 = 0.1/5, p3 = 0.1/4, p4 = 0.1/3.
	// BP = 1.0 (equal length, 6 == 6).
	unrelatedP1 := 0.1 / 6.0
	unrelatedP2 := 0.1 / 5.0
	unrelatedP3 := 0.1 / 4.0
	unrelatedP4 := 0.1 / 3.0
	wantUnrelated := math.Exp((math.Log(unrelatedP1) + math.Log(unrelatedP2) + math.Log(unrelatedP3) + math.Log(unrelatedP4)) / 4.0)

	// paraphrase_semantically_fine_lexically_different: 9-word Reference,
	// 11-word Candidate, sharing exactly one token ("I"):
	//   n=1: 11 total (candidate length), 1 matches -> p1 = 1/11.
	//   n=2: 10 total, 0 match -> epsilon-smoothed p2 = 0.1/10.
	//   n=3: 9 total, 0 match -> epsilon-smoothed p3 = 0.1/9.
	//   n=4: 8 total, 0 match -> epsilon-smoothed p4 = 0.1/8.
	// BP = 1.0 (candidate longer than reference, 11 > 9).
	paraphraseP1 := 1.0 / 11.0
	paraphraseP2 := 0.1 / 10.0
	paraphraseP3 := 0.1 / 9.0
	paraphraseP4 := 0.1 / 8.0
	wantParaphrase := math.Exp((math.Log(paraphraseP1) + math.Log(paraphraseP2) + math.Log(paraphraseP3) + math.Log(paraphraseP4)) / 4.0)

	// very_short_pair_yes_sir: 2-word Reference "yes sir", 1-word
	// Candidate "yes". Effective order reduces to 1 (candidate has only
	// one token): p1 = 1/1 = 1.0. BP = exp(1 - 2/1) = exp(-1).
	wantShortPair := math.Exp(1.0 - 2.0/1.0)

	want := map[string]float64{
		"perfect_identical_translation":                    wantPerfectIdentical,
		"one_word_substitution_currency_mismatch":          wantOneWordSubstitution,
		"missing_clause_deletion_within_48_hours":          wantClauseDeletion,
		"completely_unrelated_candidate":                   wantUnrelated,
		"paraphrase_semantically_fine_lexically_different": wantParaphrase,
		"very_short_pair_yes_sir":                          wantShortPair,
	}

	entries := FixedTranslationCorpus()
	if len(want) != len(entries) {
		t.Fatalf("test has expectations for %d entries but FixedTranslationCorpus() returned %d — update this test alongside FixedTranslationCorpus", len(want), len(entries))
	}

	for _, e := range entries {
		expected, ok := want[e.Name]
		if !ok {
			t.Errorf("no expected BLEU registered for translation corpus entry %q — update this test alongside FixedTranslationCorpus", e.Name)
			continue
		}
		got := BLEUScore(e.Reference, e.Candidate)
		if !bleuApproxEqual(got, expected) {
			t.Errorf("entry %q: BLEUScore(reference, candidate) = %v, want %v", e.Name, got, expected)
		}
	}
}

// TestFixedTranslationCorpus_QualitativeExpectations checks broader,
// less brittle properties each entry is specifically designed to
// demonstrate, independent of the exact hand-computed float in
// TestFixedTranslationCorpus_PrecomputedBLEUMatches above -- e.g. that
// the "unrelated" entry really is near zero and the "deletion" entry
// really is driven below 1.0 specifically by the brevity penalty, not by
// precision loss.
func TestFixedTranslationCorpus_QualitativeExpectations(t *testing.T) {
	entries := make(map[string]TranslationCorpusEntry)
	for _, e := range FixedTranslationCorpus() {
		entries[e.Name] = e
	}

	perfect := entries["perfect_identical_translation"]
	if got := BLEUScore(perfect.Reference, perfect.Candidate); !bleuApproxEqual(got, 1.0) {
		t.Errorf("perfect_identical_translation: BLEUScore = %v, want 1.0", got)
	}

	unrelated := entries["completely_unrelated_candidate"]
	if got := BLEUScore(unrelated.Reference, unrelated.Candidate); got > 0.1 {
		t.Errorf("completely_unrelated_candidate: BLEUScore = %v, want near 0 (<= 0.1)", got)
	}

	// The clause-deletion entry's candidate is an exact prefix of its
	// reference, so every n-gram precision it does have should be a
	// perfect 1.0 -- the shortfall below 1.0 must come entirely from the
	// brevity penalty, not from any mismatched word.
	deletion := entries["missing_clause_deletion_within_48_hours"]
	deletionBLEU := BLEUScore(deletion.Reference, deletion.Candidate)
	if deletionBLEU >= 1.0 {
		t.Errorf("missing_clause_deletion_within_48_hours: BLEUScore = %v, want < 1.0 (brevity penalty must apply)", deletionBLEU)
	}
	explicitBP := math.Exp(1.0 - 13.0/10.0)
	if !bleuApproxEqual(deletionBLEU, explicitBP) {
		t.Errorf("missing_clause_deletion_within_48_hours: BLEUScore = %v, want exactly the brevity penalty %v (all precisions should be 1.0)", deletionBLEU, explicitBP)
	}

	// The paraphrase entry is designed to score roughly as low as the
	// totally-unrelated entry despite being a good translation -- a
	// documented BLEU limitation (see FixedTranslationCorpus's doc
	// comment), not something this package tries to fix. Assert it's low
	// and, notably, not meaningfully higher than the unrelated entry.
	paraphrase := entries["paraphrase_semantically_fine_lexically_different"]
	paraphraseBLEU := BLEUScore(paraphrase.Reference, paraphrase.Candidate)
	unrelatedBLEU := BLEUScore(unrelated.Reference, unrelated.Candidate)
	if paraphraseBLEU > 0.1 {
		t.Errorf("paraphrase_semantically_fine_lexically_different: BLEUScore = %v, want near 0 (<= 0.1) despite being a good translation -- known BLEU limitation", paraphraseBLEU)
	}
	if paraphraseBLEU > unrelatedBLEU*3 {
		t.Errorf("paraphrase_semantically_fine_lexically_different: BLEUScore = %v is not comparably low to completely_unrelated_candidate's %v -- the paraphrase entry is supposed to demonstrate BLEU can't tell a good paraphrase from an unrelated sentence", paraphraseBLEU, unrelatedBLEU)
	}

	// The short-pair entry must show both effective-order reduction (a
	// 1-word candidate can only ever produce a unigram precision) and a
	// real, non-trivial brevity penalty (a 1-word candidate against a
	// 2-word reference).
	shortPair := entries["very_short_pair_yes_sir"]
	shortPairBLEU := BLEUScore(shortPair.Reference, shortPair.Candidate)
	wantShortPairBLEU := math.Exp(1.0 - 2.0/1.0)
	if !bleuApproxEqual(shortPairBLEU, wantShortPairBLEU) {
		t.Errorf("very_short_pair_yes_sir: BLEUScore = %v, want %v", shortPairBLEU, wantShortPairBLEU)
	}
}
