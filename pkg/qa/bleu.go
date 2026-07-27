package qa

import (
	"math"
	"strings"
)

// bleuMaxNGram is the highest n-gram order standard BLEU-4 considers
// (unigrams through 4-grams). See BLEUScore's doc comment for what
// happens when a candidate is shorter than this.
const bleuMaxNGram = 4

// bleuSmoothingEpsilon is the small additive constant substituted for a
// zero-match n-gram precision so a single missing higher-order n-gram
// match doesn't collapse the whole score to zero via log(0). This is the
// same constant and mechanism NLTK's BLEU implementation calls
// "smoothing method 1" (Chen & Cherry, 2014, "A Systematic Comparison of
// Smoothing Techniques for Sentence-Level BLEU"): when a precision's
// numerator (matched n-gram count) is 0 but its denominator (total
// n-grams of that order in the candidate) is nonzero, replace the
// precision with epsilon/denominator instead of 0. See BLEUScore's doc
// comment for the full smoothing rationale.
const bleuSmoothingEpsilon = 0.1

// BLEUScore computes a sentence-level BLEU score (Papineni et al., 2002,
// "BLEU: a Method for Automatic Evaluation of Machine Translation")
// between reference (the ground-truth translation) and candidate (what a
// machine-translation system produced in its place): the geometric mean
// of modified (clipped) n-gram precisions for n = 1..4, scaled by a
// brevity penalty that punishes candidates shorter than the reference.
// The result is in [0, 1], where 1.0 means a perfect (or, for short
// candidates missing higher n-gram orders, at-least-as-good-as-can-be-
// measured) match and 0.0 means no useful n-gram overlap at all.
//
// Both strings are tokenized on whitespace (strings.Fields) — no
// punctuation stripping, case-folding, or stemming is performed, exactly
// mirroring wer.go's WordErrorRate tokenization choice and for the same
// reason: it's a simple, honest starting point, not a claim that this is
// how a production translation-quality metric should normalize text. A
// real BLEU deployment would likely want configurable normalization
// (case-folding, punctuation stripping/tokenization rules per the
// standard BLEU/sacrebleu tokenizers) before scoring, which is not
// implemented here.
//
// What's real here: standard BLEU-4 modified n-gram precision (clipping
// each n-gram's matched count to the count it appears in the reference,
// so a candidate can't inflate its score by repeating a word that only
// appears once in the reference) and the standard brevity penalty
// BP = 1 if len(candidate) > len(reference), else
// exp(1 - len(reference)/len(candidate)).
//
// Two documented, deliberate choices for short/degenerate inputs, since a
// literal, unsmoothed BLEU-4 is well known to be a poor metric on short
// sentences:
//
//   - Effective order reduction: a raw BLEU-4 requires 4-gram overlap,
//     which is definitionally impossible for any candidate shorter than
//     four words (there are zero 4-grams to even count), making
//     unsmoothed BLEU-4 always exactly 0 for every short sentence
//     regardless of how good the translation is. Rather than accept that
//     degenerate always-zero result, this implementation only includes
//     n-gram orders up to min(4, len(candidate tokens)) in the geometric
//     mean — the same "effective order" approach sacrebleu uses for
//     short-sentence BLEU. A single-word candidate is scored on unigram
//     precision alone, a two-word candidate on unigram+bigram precision,
//     and so on. This is a corpus-quality-metric design choice, not a
//     hidden bug: it means BLEUScore is not directly comparable across
//     candidates of very different lengths in the same way full BLEU-4
//     is, which matters if this is ever used to rank/compare translations
//     rather than track one translation's quality over time.
//
//   - Epsilon smoothing for a present-but-zero-match order: when an
//     n-gram order does have candidate n-grams (order <= effective
//     order, so the denominator is nonzero) but none of them match the
//     reference (matched count is exactly 0), this implementation
//     substitutes bleuSmoothingEpsilon/denominator for that order's
//     precision instead of a literal 0, so that one missing higher-order
//     match doesn't drag the whole geometric mean to zero via log(0). See
//     bleuSmoothingEpsilon's doc comment for the smoothing method this
//     mirrors. The result is that a candidate sharing most but not all of
//     a reference's higher-order n-grams gets a small nonzero score
//     rather than a hard 0 — legitimate smoothed-BLEU behavior, not an
//     attempt to inflate scores: a candidate with zero unigram overlap at
//     all still scores very close to 0 once every order is smoothed down
//     (see bleu_test.go's completely-unrelated-candidate case for a
//     hand-computed example).
//
// Neither choice changes standard BLEU-4 behavior for the common case of
// a candidate with at least 4 tokens and at least one matching n-gram at
// every order — that case is exactly textbook BLEU-4 with no smoothing
// applied at all.
//
// Degenerate-input behavior, mirroring WordErrorRate's explicit handling
// of its own degenerate inputs:
//   - BLEUScore("", "") returns 1.0 (nothing to translate, nothing
//     mistranslated — trivially perfect, the BLEU-direction mirror of
//     WordErrorRate("", "") returning 0.0).
//   - BLEUScore("", candidate) for a non-empty candidate returns 0.0
//     (there is no reference for any word of the hallucinated candidate
//     to match, so there is nothing to award credit for).
//   - BLEUScore(reference, "") for a non-empty reference returns 0.0
//     (an empty candidate has no n-grams at any order and, per the
//     standard BLEU brevity penalty, BP = 0 whenever the candidate is
//     empty against a non-empty reference).
//
// GROUNDWORK, NOT A LIVE MEASUREMENT — same caveat as wer.go's package
// doc comment and pkg/qa/translation_corpus.go's doc comment: every score
// this function or its accompanying fixed corpus produces today is
// computed against canned, hand-authored reference/candidate string
// pairs, never against a live machine-translation vendor's actual output.
// This is the BLEU half of ROADMAP's "WER/BLEU/CSAT proxy" translation-
// quality tracking item (see references/workstreams.md's PM charter);
// once real translation vendor traffic exists, this same BLEUScore
// function carries over unchanged, only the source of the candidate
// string changes (fixed corpus -> real vendor response).
func BLEUScore(reference, candidate string) float64 {
	return bleuScore(strings.Fields(reference), strings.Fields(candidate))
}

// bleuScore computes BLEUScore over already-tokenized word sequences.
func bleuScore(ref, cand []string) float64 {
	if len(ref) == 0 {
		if len(cand) == 0 {
			return 1.0
		}
		return 0.0
	}
	if len(cand) == 0 {
		return 0.0
	}

	maxN := bleuMaxNGram
	if len(cand) < maxN {
		maxN = len(cand)
	}

	logPrecisionSum := 0.0
	for n := 1; n <= maxN; n++ {
		// total is guaranteed > 0 here: maxN <= len(cand), so every
		// n <= maxN has at least one n-gram of that order in cand.
		total := len(cand) - n + 1
		match := clippedNGramMatches(ref, cand, n)

		p := float64(match) / float64(total)
		if match == 0 {
			p = bleuSmoothingEpsilon / float64(total)
		}
		logPrecisionSum += math.Log(p)
	}

	geometricMean := math.Exp(logPrecisionSum / float64(maxN))
	return brevityPenalty(len(ref), len(cand)) * geometricMean
}

// brevityPenalty computes the standard BLEU brevity penalty: no penalty
// (1.0) when the candidate is at least as long as the reference, an
// exponentially increasing penalty the shorter the candidate is
// otherwise. Callers must ensure candLen > 0 (a zero-length candidate
// against a non-empty reference is handled directly in bleuScore, since
// exp(1 - refLen/0) is not a meaningful computation).
func brevityPenalty(refLen, candLen int) float64 {
	if candLen > refLen {
		return 1.0
	}
	return math.Exp(1.0 - float64(refLen)/float64(candLen))
}

// nGramCounts returns the count of each distinct n-gram (n consecutive
// tokens, joined with a single space — safe as an unambiguous map key
// since strings.Fields tokens never themselves contain whitespace) in
// tokens. Returns an empty map if tokens is shorter than n.
func nGramCounts(tokens []string, n int) map[string]int {
	counts := make(map[string]int)
	for i := 0; i+n <= len(tokens); i++ {
		key := strings.Join(tokens[i:i+n], " ")
		counts[key]++
	}
	return counts
}

// clippedNGramMatches computes the modified (clipped) n-gram match count
// between ref and cand at order n: for each distinct n-gram in cand, the
// number of matches credited is capped at the number of times that same
// n-gram appears in ref, so a candidate can't inflate its precision by
// repeating a matching n-gram more times than the reference actually
// contains it (the classic BLEU "clipping" rule, e.g. a candidate
// repeating "the" seven times against a reference containing "the" only
// twice is credited with only 2 matches, not 7).
func clippedNGramMatches(ref, cand []string, n int) int {
	refCounts := nGramCounts(ref, n)
	candCounts := nGramCounts(cand, n)

	match := 0
	for gram, candCount := range candCounts {
		refCount, ok := refCounts[gram]
		if !ok {
			continue
		}
		if candCount < refCount {
			match += candCount
		} else {
			match += refCount
		}
	}
	return match
}
