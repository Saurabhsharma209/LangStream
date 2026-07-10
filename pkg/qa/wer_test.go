package qa

import (
	"math"
	"testing"
)

const werEpsilon = 1e-9

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < werEpsilon
}

func TestWordErrorRate_IdenticalStringsIsZero(t *testing.T) {
	got := WordErrorRate("the cat sat down", "the cat sat down")
	if !approxEqual(got, 0.0) {
		t.Fatalf("WordErrorRate(identical) = %v, want 0.0", got)
	}
}

func TestWordErrorRate_CompletelyDifferentIsOne(t *testing.T) {
	// Same word count (4), every word substituted -> 4 substitutions / 4
	// reference words = 1.0.
	got := WordErrorRate("the cat sat down", "one two three four")
	if !approxEqual(got, 1.0) {
		t.Fatalf("WordErrorRate(completely different, same length) = %v, want 1.0", got)
	}
}

func TestWordErrorRate_OneSubstitutionInFourWords(t *testing.T) {
	got := WordErrorRate("the cat sat down", "the cat sad down")
	want := 0.25
	if !approxEqual(got, want) {
		t.Fatalf("WordErrorRate(one substitution / 4 words) = %v, want %v", got, want)
	}
}

func TestWordErrorRate_OneDeletion(t *testing.T) {
	// reference "a b c d" (4 words), hypothesis drops "c" -> 1 deletion / 4.
	got := WordErrorRate("a b c d", "a b d")
	want := 0.25
	if !approxEqual(got, want) {
		t.Fatalf("WordErrorRate(one deletion / 4 words) = %v, want %v", got, want)
	}
}

func TestWordErrorRate_OneInsertion(t *testing.T) {
	// reference "a b c" (3 words), hypothesis has an extra inserted word
	// -> 1 insertion / 3 reference words.
	got := WordErrorRate("a b c", "a b x c")
	want := 1.0 / 3.0
	if !approxEqual(got, want) {
		t.Fatalf("WordErrorRate(one insertion / 3 words) = %v, want %v", got, want)
	}
}

func TestWordErrorRate_BothEmptyIsZero(t *testing.T) {
	got := WordErrorRate("", "")
	if !approxEqual(got, 0.0) {
		t.Fatalf("WordErrorRate(\"\", \"\") = %v, want 0.0", got)
	}
}

func TestWordErrorRate_EmptyReferenceNonEmptyHypothesisIsOne(t *testing.T) {
	got := WordErrorRate("", "a b c")
	if !approxEqual(got, 1.0) {
		t.Fatalf("WordErrorRate(\"\", \"a b c\") = %v, want 1.0", got)
	}
}

func TestWordErrorRate_NonEmptyReferenceEmptyHypothesisIsOne(t *testing.T) {
	// Every reference word is a deletion: N deletions / N = 1.0.
	got := WordErrorRate("a b c", "")
	if !approxEqual(got, 1.0) {
		t.Fatalf("WordErrorRate(\"a b c\", \"\") = %v, want 1.0", got)
	}
}

func TestWordErrorRate_IsCaseSensitive(t *testing.T) {
	// Documented behavior: no case-folding, so a pure case difference
	// counts as a substitution.
	got := WordErrorRate("Hello world", "hello world")
	want := 0.5
	if !approxEqual(got, want) {
		t.Fatalf("WordErrorRate(case difference) = %v, want %v (case-sensitive by design)", got, want)
	}
}

func TestWordErrorRate_InsertionsCanPushWERAboveOne(t *testing.T) {
	// reference "a" (1 word), hypothesis "b c d" (3 words, none matching)
	// -> at least 3 edits / 1 reference word = 3.0. This is correct,
	// standard WER behavior (see WordErrorRate's doc comment), not a bug.
	got := WordErrorRate("a", "b c d")
	if got <= 1.0 {
		t.Fatalf("WordErrorRate(short reference, long mismatched hypothesis) = %v, want > 1.0", got)
	}
}
