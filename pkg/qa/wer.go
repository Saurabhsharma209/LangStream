// Package qa is QA's home for LangStream's eventual per-language-pair
// accuracy regression suite (see references/workstreams.md's QA charter
// and ROADMAP.md Week 4: "Real WER, latency, and CSAT measurement").
//
// What's here today is deliberately narrow: a Word Error Rate (WER)
// calculator (this file) and a small fixed reference/hypothesis transcript
// corpus (corpus.go) used to wire that calculator up against the
// fake-ASR-backed test pipeline already exercised by
// integration_vendor_test.go (see the repo-root wer_measurement_test.go).
//
// GROUNDWORK, NOT A LIVE MEASUREMENT. Every WER number produced by tests in
// or around this package today is computed against canned, hand-authored
// hypothesis strings fed through in-process fake vendor servers (or typed
// directly into WordErrorRate) — never against a live vendor endpoint or
// real recorded/live call audio. That's the Week 4 roadmap item this
// package is *starting*, not finishing: once real vendor traffic exists,
// the same WordErrorRate function and corpus shape carry over unchanged,
// only the source of the hypothesis transcript changes (fake server ->
// real vendor response). Don't cite numbers produced here as real-world
// ASR accuracy.
package qa

import "strings"

// WordErrorRate computes the word error rate between reference (the
// ground-truth transcript) and hypothesis (what an ASR system actually
// produced): WER = (S + D + I) / N, where S/D/I are the substitutions,
// deletions, and insertions in the minimum-edit alignment between the two
// word sequences, and N is the number of words in reference.
//
// Both strings are tokenized on whitespace (strings.Fields) — no
// punctuation stripping or case-folding is performed, so "Hello," and
// "hello" are different tokens. That's a deliberate, simple starting
// point; a real accuracy suite would likely want configurable
// normalization (case-folding, punctuation stripping, number
// normalization) before diffing, which is not implemented here.
//
// WordErrorRate(reference, "") for a non-empty reference returns 1.0 (every
// reference word counts as a deletion). Two empty strings return 0.0.
// WordErrorRate can exceed 1.0 when hypothesis has many more words than
// reference (insertions can outnumber N) — that is correct, standard WER
// behavior, not a bug.
func WordErrorRate(reference, hypothesis string) float64 {
	return wordErrorRate(strings.Fields(reference), strings.Fields(hypothesis))
}

// wordErrorRate computes WER over already-tokenized word sequences via the
// standard dynamic-programming Levenshtein edit distance (unit cost for
// each substitution/deletion/insertion), which for the minimal alignment
// is exactly S+D+I.
func wordErrorRate(ref, hyp []string) float64 {
	n := len(ref)
	m := len(hyp)

	if n == 0 {
		if m == 0 {
			return 0.0
		}
		return 1.0
	}

	// dp[i][j] = edit distance between ref[:i] and hyp[:j].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 0; i <= n; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= m; j++ {
		dp[0][j] = j
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if ref[i-1] == hyp[j-1] {
				dp[i][j] = dp[i-1][j-1]
				continue
			}
			sub := dp[i-1][j-1] + 1
			del := dp[i-1][j] + 1
			ins := dp[i][j-1] + 1
			dp[i][j] = min3(sub, del, ins)
		}
	}

	distance := dp[n][m]
	return float64(distance) / float64(n)
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
