package qa

import (
	"math"
	"strings"
	"testing"
)

// TestFixedTranslationCorpus_EntriesAreWellFormed guards against the same
// class of silent-fixture bug corpus_test.go's
// TestFixedCorpus_EntriesAreWellFormed guards ASR entries against: every
// translation entry must have a name, both language tags, and non-empty
// Source/Reference/Candidate text, and names must be unique.
func TestFixedTranslationCorpus_EntriesAreWellFormed(t *testing.T) {
	entries := FixedTranslationCorpus()
	if len(entries) < 18 {
		t.Fatalf("FixedTranslationCorpus() returned %d entries, want at least 18", len(entries))
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

	// word_reordering_full_reversal_delivery_schedule: 7-word Reference
	// and Candidate share the identical bag of words, fully reversed.
	// Every unigram matches (p1 = 7/7 = 1.0), but zero bigrams/trigrams/
	// 4-grams survive the reversal, so p2/p3/p4 are all epsilon-smoothed
	// against the candidate's respective n-gram totals (6, 5, 4). BP =
	// 1.0 (equal length, 7 == 7).
	reorderP1 := 7.0 / 7.0
	reorderP2 := 0.1 / 6.0
	reorderP3 := 0.1 / 5.0
	reorderP4 := 0.1 / 4.0
	wantReordering := math.Exp((math.Log(reorderP1) + math.Log(reorderP2) + math.Log(reorderP3) + math.Log(reorderP4)) / 4.0)

	// partial_overlap_first_half_matches_delivery_estimate: 8-word
	// Reference and Candidate share an exact 5-word prefix and diverge
	// completely for the trailing 3 words. p1 = 5/8, p2 = 4/7, p3 = 3/6,
	// p4 = 2/5. BP = 1.0 (equal length, 8 == 8).
	partialP1 := 5.0 / 8.0
	partialP2 := 4.0 / 7.0
	partialP3 := 3.0 / 6.0
	partialP4 := 2.0 / 5.0
	wantPartialOverlap := math.Exp((math.Log(partialP1) + math.Log(partialP2) + math.Log(partialP3) + math.Log(partialP4)) / 4.0)

	// hallucinated_added_clause_complaint_registered: 5-word Reference,
	// 10-word Candidate (the Reference plus a wholly invented 6-word
	// trailing clause). BP = 1.0 (candidate longer, 10 > 5) -- the
	// opposite of missing_clause_deletion_within_48_hours, whose
	// shortfall is entirely brevity-penalty-driven. Here every precision
	// itself is diluted by the hallucinated words: p1 = 5/10, p2 = 4/9,
	// p3 = 3/8, p4 = 2/7.
	hallucinatedP1 := 5.0 / 10.0
	hallucinatedP2 := 4.0 / 9.0
	hallucinatedP3 := 3.0 / 8.0
	hallucinatedP4 := 2.0 / 7.0
	wantHallucinatedClause := math.Exp((math.Log(hallucinatedP1) + math.Log(hallucinatedP2) + math.Log(hallucinatedP3) + math.Log(hallucinatedP4)) / 4.0)

	// named_entity_mismatch_relationship_manager_name: 11-word Reference
	// and Candidate differing only in one mid-sentence proper noun
	// (word 4 of 11: "rahul" vs "suresh"). Unlike
	// one_word_substitution_currency_mismatch's last-word mismatch (which
	// only ever breaks one n-gram per order), a mid-sentence mismatch
	// breaks every n-gram overlapping that position: p1 = 10/11, p2 =
	// 8/10, p3 = 6/9, p4 = 4/8. BP = 1.0 (equal length, 11 == 11).
	namedEntityP1 := 10.0 / 11.0
	namedEntityP2 := 8.0 / 10.0
	namedEntityP3 := 6.0 / 9.0
	namedEntityP4 := 4.0 / 8.0
	wantNamedEntityMismatch := math.Exp((math.Log(namedEntityP1) + math.Log(namedEntityP2) + math.Log(namedEntityP3) + math.Log(namedEntityP4)) / 4.0)

	// case_only_difference_payment_capitalization: 8-word Reference and
	// Candidate identical except one word's capitalization ("payment" vs
	// "Payment"). BLEUScore performs no case-folding, so this counts as a
	// full token mismatch at every order it touches: p1 = 7/8, p2 = 5/7,
	// p3 = 4/6, p4 = 3/5. BP = 1.0 (equal length, 8 == 8).
	caseOnlyP1 := 7.0 / 8.0
	caseOnlyP2 := 5.0 / 7.0
	caseOnlyP3 := 4.0 / 6.0
	caseOnlyP4 := 3.0 / 5.0
	wantCaseOnlyDifference := math.Exp((math.Log(caseOnlyP1) + math.Log(caseOnlyP2) + math.Log(caseOnlyP3) + math.Log(caseOnlyP4)) / 4.0)

	// over_repeated_word_clipping_billing_transfer: 8-word Reference
	// containing "the" exactly twice, 8-word Candidate that is just "the"
	// repeated eight times. Clipping caps unigram matches at the
	// reference's own count of "the" (2), not the candidate's raw repeat
	// count (8): p1 = 2/8. No repeated-"the" bigram/trigram/4-gram exists
	// in the reference, so p2/p3/p4 are all epsilon-smoothed against the
	// candidate's n-gram totals (7, 6, 5). BP = 1.0 (equal length, 8 ==
	// 8).
	clippingP1 := 2.0 / 8.0
	clippingP2 := 0.1 / 7.0
	clippingP3 := 0.1 / 6.0
	clippingP4 := 0.1 / 5.0
	wantOverRepeatedClipping := math.Exp((math.Log(clippingP1) + math.Log(clippingP2) + math.Log(clippingP3) + math.Log(clippingP4)) / 4.0)

	// word_order_adjacent_swap_end_delivery_schedule: 6-word Reference
	// and Candidate share the identical bag of words with only the
	// final two words ("delivered", "tomorrow") swapped. p1 = 6/6 = 1.0
	// (every unigram matches). Bigrams: 5 total in each; ref bigrams are
	// "your order","order will","will be","be delivered","delivered
	// tomorrow"; candidate bigrams are "your order","order
	// will","will be","be tomorrow","tomorrow delivered" -- the first
	// three match, the last two (touching the swapped pair) don't ->
	// p2 = 3/5. Trigrams: 4 total each; the first two ("your order
	// will","order will be") match, the last two don't -> p3 = 2/4 =
	// 1/2. 4-grams: 3 total each; only the first ("your order will be")
	// matches -> p4 = 1/3. BP = 1.0 (equal length, 6 == 6).
	swapEndP1 := 6.0 / 6.0
	swapEndP2 := 3.0 / 5.0
	swapEndP3 := 2.0 / 4.0
	swapEndP4 := 1.0 / 3.0
	wantSwapEnd := math.Exp((math.Log(swapEndP1) + math.Log(swapEndP2) + math.Log(swapEndP3) + math.Log(swapEndP4)) / 4.0)

	// two_scattered_substitutions_currency_and_status_refund: 9-word
	// Reference and Candidate, equal length, differing at word 6
	// ("rupees"->"dollars") and word 9 ("processed"->"completed"), no
	// other repeated/overlapping vocabulary. At each n-gram order, the
	// "broken" n-grams are exactly those whose window overlaps either
	// error position:
	//   n=1: total 9, broken {6,9} -> match 7 -> p1 = 7/9.
	//   n=2: total 8, broken starts touching pos 6 {5,6} or pos 9 {8}
	//        -> 3 distinct -> match 5 -> p2 = 5/8.
	//   n=3: total 7, broken starts touching pos 6 {4,5,6} or pos 9 {7}
	//        -> 4 distinct -> match 3 -> p3 = 3/7.
	//   n=4: total 6, broken starts touching pos 6 {3,4,5,6} or pos 9
	//        {6} -> 4 distinct (start 6 covers both positions at once)
	//        -> match 2 -> p4 = 2/6 = 1/3.
	// BP = 1.0 (equal length, 9 == 9).
	scatteredP1 := 7.0 / 9.0
	scatteredP2 := 5.0 / 8.0
	scatteredP3 := 3.0 / 7.0
	scatteredP4 := 2.0 / 6.0
	wantScatteredSubstitutions := math.Exp((math.Log(scatteredP1) + math.Log(scatteredP2) + math.Log(scatteredP3) + math.Log(scatteredP4)) / 4.0)

	// short_pair_leading_word_insertion_call_disconnected: 3-word
	// Reference "call is disconnected", 4-word Candidate "the call is
	// disconnected" (one hallucinated leading word). Effective order
	// reaches the full 4 (candidate has >= 4 tokens):
	//   n=1: 4 total, matches call/is/disconnected (3), "the" doesn't
	//        -> p1 = 3/4.
	//   n=2: 3 total ("the call","call is","is disconnected"), 2 match
	//        -> p2 = 2/3.
	//   n=3: 2 total ("the call is","call is disconnected"), 1 matches
	//        (the reference's only trigram) -> p3 = 1/2.
	//   n=4: 1 total ("the call is disconnected"), reference has zero
	//        4-grams (length 3 < 4) so 0 match -> epsilon-smoothed
	//        p4 = 0.1/1.
	// BP = 1.0 (candidate longer than reference, 4 > 3).
	shortInsertP1 := 3.0 / 4.0
	shortInsertP2 := 2.0 / 3.0
	shortInsertP3 := 1.0 / 2.0
	shortInsertP4 := 0.1 / 1.0
	wantShortPairLeadingInsertion := math.Exp((math.Log(shortInsertP1) + math.Log(shortInsertP2) + math.Log(shortInsertP3) + math.Log(shortInsertP4)) / 4.0)

	// stutter_duplicated_trailing_word_confirmed_confirmed: 5-word
	// Reference "your order has been confirmed", 6-word Candidate
	// repeating the final word once more ("...confirmed confirmed").
	// Clipping caps the credited match for "confirmed" at the
	// reference's own count (1), not the candidate's count (2):
	//   n=1: 6 total, "your"/"order"/"has"/"been" match (4) plus
	//        "confirmed" clipped to 1 -> match 5 -> p1 = 5/6.
	//   n=2: 5 total ("your order","order has","has been","been
	//        confirmed","confirmed confirmed"), first four match, the
	//        fifth doesn't -> p2 = 4/5.
	//   n=3: 4 total, first three match, the fourth doesn't -> p3 =
	//        3/4.
	//   n=4: 3 total, first two match, the third doesn't -> p4 = 2/3.
	// BP = 1.0 (candidate longer, 6 > 5).
	stutterP1 := 5.0 / 6.0
	stutterP2 := 4.0 / 5.0
	stutterP3 := 3.0 / 4.0
	stutterP4 := 2.0 / 3.0
	wantStutterDuplication := math.Exp((math.Log(stutterP1) + math.Log(stutterP2) + math.Log(stutterP3) + math.Log(stutterP4)) / 4.0)

	// very_short_pair_substitution_yes_maam: 2-word Reference "yes
	// sir", 2-word Candidate "yes ma'am" (substituting the address
	// term, unlike very_short_pair_yes_sir's deletion). Effective order
	// reaches 2 (candidate has exactly 2 tokens): p1 = 1/2 ("yes"
	// matches, "ma'am" doesn't); the candidate's only bigram ("yes
	// ma'am") doesn't match the reference's ("yes sir") -> epsilon-
	// smoothed p2 = 0.1/1. BP = 1.0 (equal length, 2 == 2).
	shortSubP1 := 1.0 / 2.0
	shortSubP2 := 0.1 / 1.0
	wantShortPairSubstitution := math.Exp((math.Log(shortSubP1) + math.Log(shortSubP2)) / 2.0)

	// combined_substitution_and_trailing_hallucination_complaint_resolved_closed:
	// 5-word Reference "your complaint has been resolved", 6-word
	// Candidate substituting the final word and appending one more
	// ("your complaint has been closed now"):
	//   n=1: 6 total, "your"/"complaint"/"has"/"been" match (4),
	//        "closed"/"now" don't -> p1 = 4/6.
	//   n=2: 5 total, "your complaint","complaint has","has been"
	//        match (3), "been closed","closed now" don't -> p2 = 3/5.
	//   n=3: 4 total, "your complaint has","complaint has been" match
	//        (2), the other two don't -> p3 = 2/4 = 1/2.
	//   n=4: 3 total, only "your complaint has been" matches -> p4 =
	//        1/3.
	// BP = 1.0 (candidate longer, 6 > 5).
	combinedP1 := 4.0 / 6.0
	combinedP2 := 3.0 / 5.0
	combinedP3 := 2.0 / 4.0
	combinedP4 := 1.0 / 3.0
	wantCombinedSubstitutionAndHallucination := math.Exp((math.Log(combinedP1) + math.Log(combinedP2) + math.Log(combinedP3) + math.Log(combinedP4)) / 4.0)

	// word_splitting_helpline_compound_translation: 7-word Reference,
	// 8-word Candidate splitting "helpline" into "help line". p1 = 6/8
	// ("please","call","our","number","for","support" match; "help" and
	// "line" don't), p2 = 4/7, p3 = 2/6, p4 is epsilon-smoothed (0.1/5)
	// -- no 4-gram survives the split intact. BP = 1.0 (candidate
	// longer, 8 > 7).
	splitP1 := 6.0 / 8.0
	splitP2 := 4.0 / 7.0
	splitP3 := 2.0 / 6.0
	splitP4 := 0.1 / 5.0
	wantWordSplitting := math.Exp((math.Log(splitP1) + math.Log(splitP2) + math.Log(splitP3) + math.Log(splitP4)) / 4.0)

	// word_merging_signup_process_confirmation: 7-word Reference,
	// 6-word Candidate merging "sign up" into "signup". p1 = 5/6, p2 =
	// 3/5, p3 = 1/2, p4 = 1/3. BP = exp(1 - 7/6) since the merged
	// Candidate is one word shorter than the Reference.
	mergeP1 := 5.0 / 6.0
	mergeP2 := 3.0 / 5.0
	mergeP3 := 1.0 / 2.0
	mergeP4 := 1.0 / 3.0
	mergeBP := math.Exp(1.0 - 7.0/6.0)
	wantWordMerging := mergeBP * math.Exp((math.Log(mergeP1)+math.Log(mergeP2)+math.Log(mergeP3)+math.Log(mergeP4))/4.0)

	// transposition_mid_sentence_will_be_delivered: 8-word Reference and
	// Candidate share the identical bag of words with "will be" swapped
	// to "be will" in the middle of the sentence. p1 = 8/8 = 1.0, p2 =
	// 4/7, p3 = 1/3, p4 = 1/5. BP = 1.0 (equal length, 8 == 8).
	transpositionP1 := 8.0 / 8.0
	transpositionP2 := 4.0 / 7.0
	transpositionP3 := 1.0 / 3.0
	transpositionP4 := 1.0 / 5.0
	wantTransposition := math.Exp((math.Log(transpositionP1) + math.Log(transpositionP2) + math.Log(transpositionP3) + math.Log(transpositionP4)) / 4.0)

	// code_switching_untranslated_source_word_khata: 7-word Reference
	// and Candidate, equal length, with the Reference's "account"
	// replaced by the untranslated Hindi word "khata" at word 5. p1 =
	// 6/7, p2 = 4/6, p3 = 2/5, p4 = 1/4. BP = 1.0 (equal length,
	// 7 == 7).
	codeSwitchP1 := 6.0 / 7.0
	codeSwitchP2 := 4.0 / 6.0
	codeSwitchP3 := 2.0 / 5.0
	codeSwitchP4 := 1.0 / 4.0
	wantCodeSwitching := math.Exp((math.Log(codeSwitchP1) + math.Log(codeSwitchP2) + math.Log(codeSwitchP3) + math.Log(codeSwitchP4)) / 4.0)

	// homophone_confusion_target_language_their_there: 8-word Reference
	// and Candidate, equal length, substituting the homophone "their"
	// for "there" at word 5. p1 = 7/8, p2 = 5/7, p3 = 1/2, p4 = 1/5.
	// BP = 1.0 (equal length, 8 == 8).
	homophoneP1 := 7.0 / 8.0
	homophoneP2 := 5.0 / 7.0
	homophoneP3 := 1.0 / 2.0
	homophoneP4 := 1.0 / 5.0
	wantHomophoneConfusion := math.Exp((math.Log(homophoneP1) + math.Log(homophoneP2) + math.Log(homophoneP3) + math.Log(homophoneP4)) / 4.0)

	// number_date_formatting_difference_24hr_vs_12hr: 7-word Reference,
	// 6-word Candidate collapsing the spoken 12-hour time "5 pm" into
	// the 24-hour numeral "17:00". p1 = 5/6, p2 = 3/5, p3 = 1/2,
	// p4 = 1/3. BP = exp(1 - 7/6) since the reformatted Candidate is one
	// word shorter than the Reference.
	dateFormatP1 := 5.0 / 6.0
	dateFormatP2 := 3.0 / 5.0
	dateFormatP3 := 1.0 / 2.0
	dateFormatP4 := 1.0 / 3.0
	dateFormatBP := math.Exp(1.0 - 7.0/6.0)
	wantDateFormatting := dateFormatBP * math.Exp((math.Log(dateFormatP1)+math.Log(dateFormatP2)+math.Log(dateFormatP3)+math.Log(dateFormatP4))/4.0)

	// honorific_marker_deletion_sir_dropped: 6-word Reference "sir your
	// order has been confirmed", 5-word Candidate dropping the leading
	// honorific "sir" entirely. Every remaining Candidate n-gram is a
	// contiguous substring of the Reference (only a leading-edge word was
	// removed), so all four precisions are 1.0: p1 = 5/5, p2 = 4/4,
	// p3 = 3/3, p4 = 2/2. BP = exp(1 - 6/5) since the Candidate is one
	// word shorter than the Reference.
	wantHonorificDeletion := math.Exp(1.0 - 6.0/5.0)

	// unit_of_measurement_km_miles_substitution_trailing: 9-word
	// Reference and Candidate, equal length, differing only in the
	// trailing unit word ("kilometers" -> "miles") -- a wrong unit of
	// measurement, not a wrong quantity or currency. p1 = 8/9, p2 = 7/8,
	// p3 = 6/7, p4 = 5/6. BP = 1.0 (equal length, 9 == 9).
	kmMilesP1 := 8.0 / 9.0
	kmMilesP2 := 7.0 / 8.0
	kmMilesP3 := 6.0 / 7.0
	kmMilesP4 := 5.0 / 6.0
	wantKmMilesSubstitution := math.Exp((math.Log(kmMilesP1) + math.Log(kmMilesP2) + math.Log(kmMilesP3) + math.Log(kmMilesP4)) / 4.0)

	// numeral_formatting_inconsistency_word_to_digit_collapse: 8-word
	// Reference "sir your bill amount is ten thousand rupees", 7-word
	// Candidate collapsing the spelled-out "ten thousand" into the
	// single digit token "10000". p1 = 6/7, p2 = 4/6, p3 = 3/5, p4 = 2/4.
	// BP = exp(1 - 8/7) since the Candidate is one word shorter than the
	// Reference.
	numeralFormatP1 := 6.0 / 7.0
	numeralFormatP2 := 4.0 / 6.0
	numeralFormatP3 := 3.0 / 5.0
	numeralFormatP4 := 2.0 / 4.0
	numeralFormatBP := math.Exp(1.0 - 8.0/7.0)
	wantNumeralFormatting := numeralFormatBP * math.Exp((math.Log(numeralFormatP1)+math.Log(numeralFormatP2)+math.Log(numeralFormatP3)+math.Log(numeralFormatP4))/4.0)

	// alphanumeric_bank_code_substitution_ifsc: 6-word Reference and
	// Candidate, equal length, differing only in the trailing
	// alphanumeric IFSC code token ("hdfc0001234" -> "hdfc0004321").
	// p1 = 5/6, p2 = 4/5, p3 = 3/4, p4 = 2/3. BP = 1.0 (equal length,
	// 6 == 6).
	ifscP1 := 5.0 / 6.0
	ifscP2 := 4.0 / 5.0
	ifscP3 := 3.0 / 4.0
	ifscP4 := 2.0 / 3.0
	wantIfscSubstitution := math.Exp((math.Log(ifscP1) + math.Log(ifscP2) + math.Log(ifscP3) + math.Log(ifscP4)) / 4.0)

	// currency_subunit_paise_rupees_substitution_trailing: 9-word
	// Reference and Candidate, equal length, differing only in the
	// trailing currency-subunit word ("paise" -> "rupees") -- a
	// main-unit-for-subunit confusion, distinct from a wrong currency or
	// wrong quantity. p1 = 8/9, p2 = 7/8, p3 = 6/7, p4 = 5/6. BP = 1.0
	// (equal length, 9 == 9).
	subunitP1 := 8.0 / 9.0
	subunitP2 := 7.0 / 8.0
	subunitP3 := 6.0 / 7.0
	subunitP4 := 5.0 / 6.0
	wantCurrencySubunitSubstitution := math.Exp((math.Log(subunitP1) + math.Log(subunitP2) + math.Log(subunitP3) + math.Log(subunitP4)) / 4.0)

	// acronym_expansion_mismatch_kyc_full_form: 6-word Reference "please
	// complete your kyc verification today", 8-word Candidate expanding
	// the acronym "kyc" into its full form "know your customer" instead
	// of leaving it as-is. Candidate unigrams (clipped against Reference
	// counts): please, complete, your (Reference has "your" once,
	// Candidate has it twice -> clipped to 1), verification, today all
	// match (5 total); know/customer don't -> p1 = 5/8. Candidate
	// bigrams: only "please complete" and "complete your" and
	// "verification today" survive -> p2 = 3/7. Candidate trigrams: only
	// "please complete your" survives -> p3 = 1/6. Candidate 4-grams:
	// none survive, epsilon-smoothed -> p4 = 0.1/5. BP = 1.0 (Candidate
	// longer, 8 > 6).
	kycP1 := 5.0 / 8.0
	kycP2 := 3.0 / 7.0
	kycP3 := 1.0 / 6.0
	kycP4 := 0.1 / 5.0
	wantKycExpansionMismatch := math.Exp((math.Log(kycP1) + math.Log(kycP2) + math.Log(kycP3) + math.Log(kycP4)) / 4.0)

	// filler_word_insertion_midsentence_actually: 6-word Reference "sir
	// your refund has been processed", 7-word Candidate inserting the
	// filler word "actually" mid-sentence (before "been"). p1 = 6/7
	// (every Reference word still appears once); p2 = 4/6 (the two
	// bigrams touching the inserted word, "has actually" and "actually
	// been", don't match); p3 = 2/5; p4 = 1/4. BP = 1.0 (Candidate
	// longer, 7 > 6).
	fillerInsertP1 := 6.0 / 7.0
	fillerInsertP2 := 4.0 / 6.0
	fillerInsertP3 := 2.0 / 5.0
	fillerInsertP4 := 1.0 / 4.0
	wantFillerInsertion := math.Exp((math.Log(fillerInsertP1) + math.Log(fillerInsertP2) + math.Log(fillerInsertP3) + math.Log(fillerInsertP4)) / 4.0)

	// negation_deletion_mistranslation_refund_not_processed: 6-word
	// Reference "your refund will not be processed", 5-word Candidate
	// dropping the negation word "not" entirely. p1 = 5/5, p2 = 3/4,
	// p3 = 1/3, p4 is epsilon-smoothed (0.1/2) since neither of the
	// Candidate's two 4-grams survives. BP = exp(1 - 6/5) (Candidate
	// one word shorter than Reference).
	negDelP1 := 5.0 / 5.0
	negDelP2 := 3.0 / 4.0
	negDelP3 := 1.0 / 3.0
	negDelP4 := 0.1 / 2.0
	wantNegationDeletion := math.Exp(1.0-6.0/5.0) * math.Exp((math.Log(negDelP1)+math.Log(negDelP2)+math.Log(negDelP3)+math.Log(negDelP4))/4.0)

	// ordinal_number_word_vs_digit_translation_third_complaint: 5-word
	// Reference "this is your third complaint", 5-word Candidate
	// substituting the ordinal "third" for the digit-ordinal "3rd".
	// p1 = 4/5, p2 = 2/4, p3 = 1/3, p4 is epsilon-smoothed (0.1/2).
	// BP = 1.0 (equal length, 5 == 5).
	ordP1 := 4.0 / 5.0
	ordP2 := 2.0 / 4.0
	ordP3 := 1.0 / 3.0
	ordP4 := 0.1 / 2.0
	wantOrdinalNumber := math.Exp((math.Log(ordP1) + math.Log(ordP2) + math.Log(ordP3) + math.Log(ordP4)) / 4.0)

	// full_utterance_echo_duplication_order_confirmed: 4-word Reference
	// "your order is confirmed" repeated twice by the Candidate (8
	// words total). p1 = 4/8, p2 = 3/7, p3 = 2/6, p4 = 1/5.
	// BP = 1.0 (Candidate longer, 8 > 4).
	echoP1 := 4.0 / 8.0
	echoP2 := 3.0 / 7.0
	echoP3 := 2.0 / 6.0
	echoP4 := 1.0 / 5.0
	wantEchoDuplication := math.Exp((math.Log(echoP1) + math.Log(echoP2) + math.Log(echoP3) + math.Log(echoP4)) / 4.0)

	// phone_number_spaced_digit_grouping_collapse_translation: 9-word
	// Reference "please confirm your number nine eight seven six five",
	// 5-word Candidate collapsing the five spoken digit words into a
	// single concatenated numeral "98765". p1 = 4/5, p2 = 3/4, p3 = 2/3,
	// p4 = 1/2. BP = exp(1 - 9/5) (Candidate four words shorter than
	// Reference).
	phoneP1 := 4.0 / 5.0
	phoneP2 := 3.0 / 4.0
	phoneP3 := 2.0 / 3.0
	phoneP4 := 1.0 / 2.0
	wantPhoneNumberGrouping := math.Exp(1.0-9.0/5.0) * math.Exp((math.Log(phoneP1)+math.Log(phoneP2)+math.Log(phoneP3)+math.Log(phoneP4))/4.0)

	// magnitude_confusion_lakh_thousand_substitution_bill_amount:
	// 6-word Reference "your bill is one lakh rupees", 6-word Candidate
	// substituting the magnitude word "lakh" for "thousand". p1 = 5/6,
	// p2 = 3/5, p3 = 2/4, p4 = 1/3. BP = 1.0 (equal length, 6 == 6).
	magP1 := 5.0 / 6.0
	magP2 := 3.0 / 5.0
	magP3 := 2.0 / 4.0
	magP4 := 1.0 / 3.0
	wantMagnitudeConfusion := math.Exp((math.Log(magP1) + math.Log(magP2) + math.Log(magP3) + math.Log(magP4)) / 4.0)

	// contraction_expansion_wont_will_not_translation_refund: 7-word
	// Reference "the payment will not be processed today", 6-word
	// Candidate merging the negation "will not" into the contracted
	// token "wont". p1 = 5/6, p2 = 3/5, p3 = 1/4, p4 is
	// epsilon-smoothed (0.1/3) since none of the Candidate's three
	// 4-grams survive the merge. BP = exp(1 - 7/6) (Candidate one word
	// shorter than Reference).
	contrP1 := 5.0 / 6.0
	contrP2 := 3.0 / 5.0
	contrP3 := 1.0 / 4.0
	contrP4 := 0.1 / 3.0
	wantContractionExpansion := math.Exp(1.0-7.0/6.0) * math.Exp((math.Log(contrP1)+math.Log(contrP2)+math.Log(contrP3)+math.Log(contrP4))/4.0)

	// tense_mismatch_future_vs_past_delivery_status_translation: 6-word
	// Reference "your parcel will be delivered tomorrow", 5-word
	// Candidate "your parcel was delivered tomorrow". p1 = 4/5, p2 = 2/4,
	// p3 and p4 are epsilon-smoothed (0.1/3, 0.1/2) since no 3-gram or
	// 4-gram survives the tense change. BP = exp(1 - 6/5).
	tenseP1 := 4.0 / 5.0
	tenseP2 := 2.0 / 4.0
	tenseP3 := 0.1 / 3.0
	tenseP4 := 0.1 / 2.0
	wantTenseMismatch := math.Exp(1.0-6.0/5.0) * math.Exp((math.Log(tenseP1)+math.Log(tenseP2)+math.Log(tenseP3)+math.Log(tenseP4))/4.0)

	// pluralization_mismatch_singular_plural_document_upload: 4-word
	// Reference/Candidate differing only in the final word ("document"
	// vs "documents"). p1 = 3/4, p2 = 2/3, p3 = 1/2, p4 is
	// epsilon-smoothed (0.1/1). BP = 1.0 (equal length, 4 == 4).
	pluralP1 := 3.0 / 4.0
	pluralP2 := 2.0 / 3.0
	pluralP3 := 1.0 / 2.0
	pluralP4 := 0.1 / 1.0
	wantPluralizationMismatch := math.Exp((math.Log(pluralP1) + math.Log(pluralP2) + math.Log(pluralP3) + math.Log(pluralP4)) / 4.0)

	// digit_value_rounding_error_bill_amount_499_vs_500: 6-word
	// Reference/Candidate differing only in one digit value ("499" vs
	// "500"). p1 = 5/6, p2 = 3/5, p3 = 2/4, p4 = 1/3. BP = 1.0 (equal
	// length, 6 == 6).
	digitP1 := 5.0 / 6.0
	digitP2 := 3.0 / 5.0
	digitP3 := 2.0 / 4.0
	digitP4 := 1.0 / 3.0
	wantDigitValueRounding := math.Exp((math.Log(digitP1) + math.Log(digitP2) + math.Log(digitP3) + math.Log(digitP4)) / 4.0)

	// place_name_substitution_city_mumbai_pune_translation: 6-word
	// Reference/Candidate differing only in the final word ("mumbai" vs
	// "pune"). p1 = 5/6, p2 = 4/5, p3 = 3/4, p4 = 2/3. BP = 1.0 (equal
	// length, 6 == 6).
	placeP1 := 5.0 / 6.0
	placeP2 := 4.0 / 5.0
	placeP3 := 3.0 / 4.0
	placeP4 := 2.0 / 3.0
	wantPlaceNameSubstitution := math.Exp((math.Log(placeP1) + math.Log(placeP2) + math.Log(placeP3) + math.Log(placeP4)) / 4.0)

	// reference_repeated_word_collapsed_bilkul_bilkul_translation:
	// 8-word Reference "sure sure sir i will check right now" has a
	// genuine repeated word the 7-word Candidate collapses to one
	// "sure". Every n-gram the Candidate does contain matches the
	// Reference exactly (all four precisions 1.0), so the shortfall is
	// entirely the brevity penalty: BP = exp(1 - 8/7).
	wantRepeatedWordCollapsed := math.Exp(1.0 - 8.0/7.0)

	// long_utterance_single_substitution_technical_support_ticket:
	// 17-word Reference/Candidate differing only in one mid-sentence
	// word ("escalated" vs "forwarded"). p1 = 16/17, p2 = 14/16,
	// p3 = 12/15, p4 = 10/14. BP = 1.0 (equal length, 17 == 17).
	longSubP1 := 16.0 / 17.0
	longSubP2 := 14.0 / 16.0
	longSubP3 := 12.0 / 15.0
	longSubP4 := 10.0 / 14.0
	wantLongUtteranceSingleSubstitution := math.Exp((math.Log(longSubP1) + math.Log(longSubP2) + math.Log(longSubP3) + math.Log(longSubP4)) / 4.0)

	// voice_conversion_passive_to_active_mismatch_order_shipped: 5-word
	// Reference "your order has been shipped" vs 5-word Candidate "we
	// have shipped your order" (a passive-to-active voice rephrasing
	// sharing every word but reordered). p1 = 3/5, p2 = 1/4, p3 and p4
	// are epsilon-smoothed (0.1/3, 0.1/2) since no 3-gram or 4-gram
	// survives the reordering. BP = 1.0 (equal length, 5 == 5).
	voiceP1 := 3.0 / 5.0
	voiceP2 := 1.0 / 4.0
	voiceP3 := 0.1 / 3.0
	voiceP4 := 0.1 / 2.0
	wantVoiceConversion := math.Exp((math.Log(voiceP1) + math.Log(voiceP2) + math.Log(voiceP3) + math.Log(voiceP4)) / 4.0)

	want := map[string]float64{
		"perfect_identical_translation":                    wantPerfectIdentical,
		"one_word_substitution_currency_mismatch":          wantOneWordSubstitution,
		"missing_clause_deletion_within_48_hours":          wantClauseDeletion,
		"completely_unrelated_candidate":                   wantUnrelated,
		"paraphrase_semantically_fine_lexically_different": wantParaphrase,
		"very_short_pair_yes_sir":                          wantShortPair,

		// Sprint 2026-07-28 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"word_reordering_full_reversal_delivery_schedule":      wantReordering,
		"partial_overlap_first_half_matches_delivery_estimate": wantPartialOverlap,
		"hallucinated_added_clause_complaint_registered":       wantHallucinatedClause,
		"named_entity_mismatch_relationship_manager_name":      wantNamedEntityMismatch,
		"case_only_difference_payment_capitalization":          wantCaseOnlyDifference,
		"over_repeated_word_clipping_billing_transfer":         wantOverRepeatedClipping,

		// Sprint 2026-08-10 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"word_order_adjacent_swap_end_delivery_schedule":                             wantSwapEnd,
		"two_scattered_substitutions_currency_and_status_refund":                     wantScatteredSubstitutions,
		"short_pair_leading_word_insertion_call_disconnected":                        wantShortPairLeadingInsertion,
		"stutter_duplicated_trailing_word_confirmed_confirmed":                       wantStutterDuplication,
		"very_short_pair_substitution_yes_maam":                                      wantShortPairSubstitution,
		"combined_substitution_and_trailing_hallucination_complaint_resolved_closed": wantCombinedSubstitutionAndHallucination,

		// Sprint 2026-08-11 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"word_splitting_helpline_compound_translation":    wantWordSplitting,
		"word_merging_signup_process_confirmation":        wantWordMerging,
		"transposition_mid_sentence_will_be_delivered":    wantTransposition,
		"code_switching_untranslated_source_word_khata":   wantCodeSwitching,
		"homophone_confusion_target_language_their_there": wantHomophoneConfusion,
		"number_date_formatting_difference_24hr_vs_12hr":  wantDateFormatting,

		// Sprint 2026-08-12 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"honorific_marker_deletion_sir_dropped":                   wantHonorificDeletion,
		"unit_of_measurement_km_miles_substitution_trailing":      wantKmMilesSubstitution,
		"numeral_formatting_inconsistency_word_to_digit_collapse": wantNumeralFormatting,
		"alphanumeric_bank_code_substitution_ifsc":                wantIfscSubstitution,
		"currency_subunit_paise_rupees_substitution_trailing":     wantCurrencySubunitSubstitution,
		"acronym_expansion_mismatch_kyc_full_form":                wantKycExpansionMismatch,
		"filler_word_insertion_midsentence_actually":              wantFillerInsertion,

		// Sprint 2026-08-13 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"negation_deletion_mistranslation_refund_not_processed":      wantNegationDeletion,
		"ordinal_number_word_vs_digit_translation_third_complaint":   wantOrdinalNumber,
		"full_utterance_echo_duplication_order_confirmed":            wantEchoDuplication,
		"phone_number_spaced_digit_grouping_collapse_translation":    wantPhoneNumberGrouping,
		"magnitude_confusion_lakh_thousand_substitution_bill_amount": wantMagnitudeConfusion,
		"contraction_expansion_wont_will_not_translation_refund":     wantContractionExpansion,

		// Sprint 2026-08-17 (QA) additions -- see FixedTranslationCorpus's
		// doc comment for each entry's reasoning and hand-computed BLEU.
		"tense_mismatch_future_vs_past_delivery_status_translation":   wantTenseMismatch,
		"pluralization_mismatch_singular_plural_document_upload":      wantPluralizationMismatch,
		"digit_value_rounding_error_bill_amount_499_vs_500":           wantDigitValueRounding,
		"place_name_substitution_city_mumbai_pune_translation":        wantPlaceNameSubstitution,
		"reference_repeated_word_collapsed_bilkul_bilkul_translation": wantRepeatedWordCollapsed,
		"long_utterance_single_substitution_technical_support_ticket": wantLongUtteranceSingleSubstitution,
		"voice_conversion_passive_to_active_mismatch_order_shipped":   wantVoiceConversion,
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

	// word_reordering_full_reversal_delivery_schedule must show perfect
	// unigram precision (the vocabulary is identical) despite scoring
	// nearly as low as the completely-unrelated entry overall, since a
	// full reversal shares zero higher-order n-grams with the reference.
	reordering := entries["word_reordering_full_reversal_delivery_schedule"]
	reorderingBLEU := BLEUScore(reordering.Reference, reordering.Candidate)
	if reorderingBLEU > 0.1 {
		t.Errorf("word_reordering_full_reversal_delivery_schedule: BLEUScore = %v, want near 0 (<= 0.1) despite identical vocabulary -- reordering should collapse higher-order n-gram precision", reorderingBLEU)
	}
	refWords := len(splitFields(reordering.Reference))
	candWords := len(splitFields(reordering.Candidate))
	if refWords != candWords {
		t.Fatalf("word_reordering_full_reversal_delivery_schedule: reference has %d words, candidate has %d -- this entry is supposed to be a pure reordering of the same bag of words", refWords, candWords)
	}

	// partial_overlap_first_half_matches_delivery_estimate must land
	// strictly between the identical (1.0) and unrelated (~0.023)
	// extremes -- the middle-ground case no other entry in this corpus
	// occupies.
	partial := entries["partial_overlap_first_half_matches_delivery_estimate"]
	partialBLEU := BLEUScore(partial.Reference, partial.Candidate)
	if partialBLEU <= unrelatedBLEU || partialBLEU >= 1.0 {
		t.Errorf("partial_overlap_first_half_matches_delivery_estimate: BLEUScore = %v, want strictly between completely_unrelated_candidate's %v and 1.0", partialBLEU, unrelatedBLEU)
	}

	// hallucinated_added_clause_complaint_registered is the brevity-
	// penalty mirror image of missing_clause_deletion_within_48_hours:
	// its candidate is longer than the reference, so BP must be exactly
	// 1.0 (no brevity penalty at all), with the entire shortfall below
	// 1.0 coming from precision dilution instead.
	hallucinated := entries["hallucinated_added_clause_complaint_registered"]
	hallucinatedBLEU := BLEUScore(hallucinated.Reference, hallucinated.Candidate)
	if len(splitFields(hallucinated.Candidate)) <= len(splitFields(hallucinated.Reference)) {
		t.Fatalf("hallucinated_added_clause_complaint_registered: candidate must be longer than reference (BP = 1.0 requires it) — got reference %q, candidate %q", hallucinated.Reference, hallucinated.Candidate)
	}
	if hallucinatedBLEU >= 1.0 {
		t.Errorf("hallucinated_added_clause_complaint_registered: BLEUScore = %v, want < 1.0 (the hallucinated clause must cost precision even though BP == 1.0)", hallucinatedBLEU)
	}

	// named_entity_mismatch_relationship_manager_name: a single
	// mid-sentence proper-noun substitution should cost noticeably more
	// than one_word_substitution_currency_mismatch's last-word
	// substitution, since a mid-sentence error contaminates more
	// n-grams at every order.
	namedEntity := entries["named_entity_mismatch_relationship_manager_name"]
	namedEntityBLEU := BLEUScore(namedEntity.Reference, namedEntity.Candidate)
	oneWordSub := entries["one_word_substitution_currency_mismatch"]
	oneWordSubBLEU := BLEUScore(oneWordSub.Reference, oneWordSub.Candidate)
	if namedEntityBLEU >= oneWordSubBLEU {
		t.Errorf("named_entity_mismatch_relationship_manager_name: BLEUScore = %v, want lower than one_word_substitution_currency_mismatch's %v (a mid-sentence mismatch should break more n-grams than a last-word one)", namedEntityBLEU, oneWordSubBLEU)
	}
	if namedEntityBLEU >= 1.0 {
		t.Errorf("named_entity_mismatch_relationship_manager_name: BLEUScore = %v, want < 1.0", namedEntityBLEU)
	}

	// case_only_difference_payment_capitalization: a single word's
	// capitalization difference must be scored as a real mismatch (no
	// case-folding), so this must be well below 1.0 despite being a
	// semantically perfect translation.
	caseOnly := entries["case_only_difference_payment_capitalization"]
	caseOnlyBLEU := BLEUScore(caseOnly.Reference, caseOnly.Candidate)
	if caseOnlyBLEU >= 1.0 {
		t.Errorf("case_only_difference_payment_capitalization: BLEUScore = %v, want < 1.0 (BLEUScore is case-sensitive, no folding is performed)", caseOnlyBLEU)
	}

	// over_repeated_word_clipping_billing_transfer: clipping must prevent
	// a degenerate all-one-word candidate from scoring anywhere near as
	// well as its raw (unclipped) unigram overlap might suggest -- it
	// should land close to the unrelated entry's near-zero score.
	overRepeated := entries["over_repeated_word_clipping_billing_transfer"]
	overRepeatedBLEU := BLEUScore(overRepeated.Reference, overRepeated.Candidate)
	if overRepeatedBLEU > 0.1 {
		t.Errorf("over_repeated_word_clipping_billing_transfer: BLEUScore = %v, want near 0 (<= 0.1) -- clipping should prevent the repeated word from inflating the score", overRepeatedBLEU)
	}
}

// splitFields is a tiny local helper so
// TestFixedTranslationCorpus_QualitativeExpectations can compare word
// counts without importing strings solely for Fields in this test file.
func splitFields(s string) []string {
	return strings.Fields(s)
}
