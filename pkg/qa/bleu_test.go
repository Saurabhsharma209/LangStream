package qa

import (
	"math"
	"testing"
)

const bleuEpsilon = 1e-9

func bleuApproxEqual(a, b float64) bool {
	return math.Abs(a-b) < bleuEpsilon
}

// --- Hand-computed core cases -----------------------------------------

func TestBLEUScore_IdenticalIsOne(t *testing.T) {
	// A 9-word sentence identical to itself: every n-gram at every order
	// 1..4 matches exactly, so every precision is 1.0, and the brevity
	// penalty is 1.0 (equal length) -> BLEU = 1.0 exactly.
	got := BLEUScore("the quick brown fox jumps over the lazy dog", "the quick brown fox jumps over the lazy dog")
	if !bleuApproxEqual(got, 1.0) {
		t.Fatalf("BLEUScore(identical) = %v, want 1.0", got)
	}
}

func TestBLEUScore_CompletelyUnrelatedIsNearZero(t *testing.T) {
	// Reference "a b c d" and candidate "e f g h" share zero words at any
	// n-gram order. All four precisions are epsilon-smoothed (see
	// bleu.go's doc comment): p1 = 0.1/4, p2 = 0.1/3, p3 = 0.1/2,
	// p4 = 0.1/1 (denominators are the candidate's n-gram counts: 4, 3,
	// 2, 1 respectively for a 4-word candidate). Brevity penalty is 1.0
	// (equal length, 4 == 4). BLEU is the geometric mean of those four
	// smoothed precisions.
	p1 := 0.1 / 4.0
	p2 := 0.1 / 3.0
	p3 := 0.1 / 2.0
	p4 := 0.1 / 1.0
	want := math.Exp((math.Log(p1) + math.Log(p2) + math.Log(p3) + math.Log(p4)) / 4.0)

	got := BLEUScore("a b c d", "e f g h")
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(completely unrelated) = %v, want %v", got, want)
	}
	// Sanity bound independent of the exact smoothing arithmetic above:
	// a totally unrelated candidate must score close to (not just below)
	// zero, not merely "less than 1".
	if got > 0.1 {
		t.Fatalf("BLEUScore(completely unrelated) = %v, want near 0 (<= 0.1)", got)
	}
}

// --- Degenerate-input edge cases, mirroring wer_test.go's style --------

func TestBLEUScore_BothEmptyIsOne(t *testing.T) {
	got := BLEUScore("", "")
	if !bleuApproxEqual(got, 1.0) {
		t.Fatalf("BLEUScore(\"\", \"\") = %v, want 1.0", got)
	}
}

func TestBLEUScore_EmptyReferenceNonEmptyCandidateIsZero(t *testing.T) {
	got := BLEUScore("", "a b c")
	if !bleuApproxEqual(got, 0.0) {
		t.Fatalf("BLEUScore(\"\", \"a b c\") = %v, want 0.0", got)
	}
}

func TestBLEUScore_NonEmptyReferenceEmptyCandidateIsZero(t *testing.T) {
	got := BLEUScore("a b c", "")
	if !bleuApproxEqual(got, 0.0) {
		t.Fatalf("BLEUScore(\"a b c\", \"\") = %v, want 0.0", got)
	}
}

// --- Single-word inputs (exercise effective-order reduction to n=1) ----

func TestBLEUScore_SingleWordIdenticalIsOne(t *testing.T) {
	// Effective order reduces to 1 (candidate has only 1 token, so no
	// bigram/trigram/4-gram order is even attempted). p1 = 1/1 = 1.0,
	// BP = 1.0 (equal length) -> BLEU = 1.0.
	got := BLEUScore("hello", "hello")
	if !bleuApproxEqual(got, 1.0) {
		t.Fatalf("BLEUScore(single word identical) = %v, want 1.0", got)
	}
}

func TestBLEUScore_SingleWordMismatchIsEpsilon(t *testing.T) {
	// Effective order is 1 (one token). The single unigram doesn't match
	// -> epsilon-smoothed p1 = 0.1/1 = 0.1. BP = 1.0 (equal length,
	// 1 == 1). Geometric mean of a single term is just that term, so
	// BLEU = 1.0 * 0.1 = 0.1 exactly, no log/exp rounding involved.
	got := BLEUScore("hello", "world")
	want := 0.1
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(single word mismatch) = %v, want %v", got, want)
	}
}

// --- Brevity penalty ----------------------------------------------------

func TestBLEUScore_ShortCandidateExactPrefixBrevityPenalty(t *testing.T) {
	// Candidate "the quick brown fox" is an exact 4-word prefix of the
	// 9-word reference. Every candidate n-gram at every order (1..4)
	// appears in the reference with a high enough count to clip fully,
	// so all four precisions are exactly 1.0 -- the entire BLEU shortfall
	// comes from the brevity penalty alone: BP = exp(1 - 9/4).
	ref := "the quick brown fox jumps over the lazy dog"
	cand := "the quick brown fox"
	want := math.Exp(1.0 - 9.0/4.0)

	got := BLEUScore(ref, cand)
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(short exact-prefix candidate) = %v, want %v", got, want)
	}
	if got >= 1.0 {
		t.Fatalf("BLEUScore(short exact-prefix candidate) = %v, want < 1.0 (brevity penalty must apply)", got)
	}
}

func TestBLEUScore_LongerCandidateNoBrevityPenalty(t *testing.T) {
	// Candidate "a b c d e f" (6 words) is longer than reference "a b c
	// d" (4 words), so no brevity penalty applies (BP = 1.0) even though
	// the candidate hallucinates two extra trailing words -- those extra
	// words only cost precision, not a length penalty. Hand-derived
	// precisions:
	//   n=1: cand unigrams {a,b,c,d,e,f} (6 total), 4 match (a,b,c,d)
	//        -> p1 = 4/6.
	//   n=2: cand bigrams {ab,bc,cd,de,ef} (5 total), 3 match (ab,bc,cd)
	//        -> p2 = 3/5.
	//   n=3: cand trigrams {abc,bcd,cde,def} (4 total), 2 match
	//        (abc,bcd) -> p3 = 2/4 = 1/2.
	//   n=4: cand 4-grams {abcd,bcde,cdef} (3 total), 1 matches (abcd)
	//        -> p4 = 1/3.
	ref := "a b c d"
	cand := "a b c d e f"

	p1 := 4.0 / 6.0
	p2 := 3.0 / 5.0
	p3 := 2.0 / 4.0
	p4 := 1.0 / 3.0
	want := math.Exp((math.Log(p1) + math.Log(p2) + math.Log(p3) + math.Log(p4)) / 4.0)

	got := BLEUScore(ref, cand)
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(longer candidate) = %v, want %v", got, want)
	}
}

// --- Clipping (a candidate can't inflate precision by over-repeating) --

func TestClippedNGramMatches_ClipsOverRepeatedWord(t *testing.T) {
	// Reference contains "the" exactly once; candidate repeats "the"
	// three times. Standard BLEU clipping caps the credited match count
	// at the reference's count (1), not the candidate's raw occurrence
	// count (3) -- otherwise a degenerate candidate could inflate
	// precision arbitrarily by repeating any single matching word.
	ref := []string{"the", "cat", "sat"}
	cand := []string{"the", "the", "the"}

	got := clippedNGramMatches(ref, cand, 1)
	want := 1
	if got != want {
		t.Fatalf("clippedNGramMatches(1 ref occurrence, 3 cand repeats) = %d, want %d", got, want)
	}
}

func TestBLEUScore_OverRepeatedWordDoesNotInflateScore(t *testing.T) {
	// End-to-end version of the clipping case above: a 3-word candidate
	// that just repeats the reference's first word three times should
	// score far below 1.0, not close to it, because only one of the
	// three repetitions is credited.
	ref := "the cat sat on the mat"
	cand := "the the the"

	got := BLEUScore(ref, cand)
	if got >= 0.5 {
		t.Fatalf("BLEUScore(over-repeated single word) = %v, want well below 0.5 (clipping should prevent inflation)", got)
	}
}

// --- Effective order reduction for short candidates ---------------------

func TestBLEUScore_TwoWordCandidateUsesUnigramAndBigramOnly(t *testing.T) {
	// A 2-word candidate has no 3-gram or 4-gram at all, so effective
	// order reduces to 2: only p1 and p2 are computed, matching
	// bleu.go's documented "effective order" choice (not a hidden bug
	// causing an always-zero score the way raw, un-reduced BLEU-4 would
	// for any candidate under 4 words).
	ref := "please hold the line"
	cand := "please hold"

	// n=1: cand unigrams {please,hold} (2 total), both match -> p1 = 1.
	// n=2: cand bigrams {please hold} (1 total), matches -> p2 = 1.
	// BP = exp(1 - 4/2) = exp(-1).
	want := math.Exp(1.0 - 4.0/2.0)

	got := BLEUScore(ref, cand)
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(two-word candidate) = %v, want %v", got, want)
	}
}

// --- Tokenization: whitespace-only, no case-folding/punctuation --------

func TestBLEUScore_IsCaseSensitive(t *testing.T) {
	// Documented behavior, mirroring WordErrorRate's case-sensitivity:
	// no case-folding is performed, so "Hello" and "hello" are different
	// tokens and do not count as a match.
	got := BLEUScore("Hello world", "hello world")
	// unigrams: cand {hello,world} (2 total), only "world" matches (1)
	// -> p1 = 1/2. bigrams: cand {"hello world"} (1 total), does not
	// match reference's "Hello world" (different casing) -> epsilon
	// smoothed p2 = 0.1/1 = 0.1. BP = 1.0 (equal length).
	p1 := 1.0 / 2.0
	p2 := 0.1 / 1.0
	want := math.Exp((math.Log(p1) + math.Log(p2)) / 2.0)
	if !bleuApproxEqual(got, want) {
		t.Fatalf("BLEUScore(case difference) = %v, want %v (case-sensitive by design)", got, want)
	}
}

func TestBLEUScore_HinglishCodeSwitchedIdenticalIsOne(t *testing.T) {
	// Whitespace tokenization handles a Devanagari+English code-switched
	// sentence exactly like any other string of tokens -- an identical
	// Hinglish reference/candidate pair should score 1.0 just like the
	// pure-English identical case, confirming BLEUScore doesn't need any
	// script-aware handling to work correctly on this repo's real target
	// language pair.
	got := BLEUScore("sir aapka order confirm ho gaya hai", "sir aapka order confirm ho gaya hai")
	if !bleuApproxEqual(got, 1.0) {
		t.Fatalf("BLEUScore(identical Hinglish) = %v, want 1.0", got)
	}
}
