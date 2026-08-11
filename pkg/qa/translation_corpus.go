package qa

// TranslationCorpusEntry is one fixed translation-quality fixture: a
// Source sentence in one language, the ground-truth Reference ("gold")
// translation of that sentence, and the Candidate translation a machine
// translation (MT) system is scripted to have produced in its place --
// the BLEUScore analogue of corpus.go's CorpusEntry (Reference/Hypothesis
// for ASR).
//
// Source is included for realism and readability (so each entry reads as
// a real Hindi->English or English->Hindi contact-center utterance, not a
// bare string pair) but, like CorpusEntry's PCM field, is not itself
// consumed by BLEUScore -- only Reference and Candidate are compared.
// Source is not currently fed through any real or fake MT backend by this
// package; it exists purely as fixture documentation of what the
// Reference is supposedly a translation of.
type TranslationCorpusEntry struct {
	// Name uniquely identifies this entry (used in test names/output).
	Name string

	// SourceLanguage and TargetLanguage are the language hints Source and
	// Reference/Candidate are respectively written in, e.g. "hi" and
	// "en". Matches the plain BCP-47-ish primary-subtag style
	// corpus.go's CorpusEntry.Language already uses.
	SourceLanguage string
	TargetLanguage string

	// Source is the original-language sentence being translated. See the
	// type doc comment: documentation only, not analyzed by BLEUScore.
	Source string

	// Reference is the ground-truth ("gold") translation of Source into
	// TargetLanguage.
	Reference string

	// Candidate is what an MT system is scripted to have produced as its
	// translation of Source -- identical to Reference for a "perfect"
	// entry, or a deliberately constructed variant (one substitution, a
	// dropped clause, a wholly unrelated sentence, a lexically different
	// paraphrase, or a truncated short sentence) to give a non-trivial,
	// hand-verifiable expected BLEU score. See FixedTranslationCorpus's
	// doc comment for each entry's specific rationale.
	Candidate string
}

// FixedTranslationCorpus returns a small, fixed set of Hindi<->English
// reference/candidate translation pairs for exercising BLEUScore, the
// translation-quality-proxy counterpart to corpus.go's FixedCorpus (ASR/
// WER). Every entry's BLEU score is hand-computed in
// translation_corpus_test.go from the exact n-gram match/total counts
// documented on that entry, the same "precomputed and asserted exactly"
// discipline corpus_test.go already applies to FixedCorpus.
//
// GROUNDWORK, NOT A LIVE MEASUREMENT -- same caveat as wer.go's and
// bleu.go's package/function doc comments: every Candidate string here is
// canned and hand-authored, standing in for what a real MT vendor
// (GPT-4o/NLLB per references/workstreams.md's PE charter) might have
// produced, never an actual live translation. This is the BLEU half of
// the "WER/BLEU/CSAT proxy" translation-quality tracking item the PM
// charter names as one of the two things that "kill this product if
// ignored" (the other being DPDP data residency) -- once real MT vendor
// traffic exists, BLEUScore and this corpus's shape carry over unchanged,
// only the source of the Candidate string changes (fixed corpus -> real
// vendor response).
//
// The original six entries:
//
//   - perfect_identical_translation: Candidate == Reference exactly ->
//     BLEU 1.0, the same "should-score-perfectly" baseline
//     identical_greeting plays in FixedCorpus.
//
//   - one_word_substitution_currency_mismatch: a single substituted word
//     at the very end of an otherwise word-for-word-identical 8-word
//     translation ("rupees" mistranslated as "dollars") -- a realistic,
//     high-stakes single-token MT error (wrong currency) that costs
//     exactly one n-gram at every order (1..4), since the substitution
//     sits at the last token position and so breaks exactly one n-gram
//     of each length. BLEU ~0.841, well below 1.0 despite only one wrong
//     word out of eight, showing how much a single substitution can cost
//     once it contaminates every higher-order n-gram it touches.
//
//   - missing_clause_deletion_within_48_hours: the Candidate is an exact
//     prefix of the 13-word Reference with the trailing three-word
//     clause "within 48 hours" dropped entirely (a real clause-level
//     omission, not a single-word slip -- the MT-quality analogue of
//     corpus.go's multi-word deletion entries). Every n-gram the 10-word
//     Candidate does contain matches the Reference exactly (all four
//     precisions are 1.0), so the entire BLEU shortfall here comes from
//     the brevity penalty alone (exp(1 - 13/10) ~ 0.741) -- a clean,
//     hand-verifiable demonstration that BLEU's brevity penalty, not
//     n-gram precision, is what catches a dropped clause when everything
//     that *does* survive translation is otherwise perfect.
//
//   - completely_unrelated_candidate: the Candidate shares zero words
//     with the Reference at all (modeling a total MT failure or a
//     response routed to the wrong utterance entirely) -- BLEU ~0.023,
//     near zero as expected, driven entirely by this implementation's
//     epsilon smoothing (see bleu.go's doc comment) rather than any
//     genuine overlap.
//
//   - paraphrase_semantically_fine_lexically_different: a semantically
//     correct, fluent paraphrase ("please hold the line while I transfer
//     your call" vs. "kindly wait a moment as I connect you to another
//     agent") that a human reviewer would accept as a good translation,
//     but which shares only one token ("I") with the Reference -> BLEU
//     ~0.019, about as low as the totally-unrelated entry above. This is
//     a well-known, deliberately-not-fixed BLEU limitation, not a bug in
//     this implementation: BLEU (with or without this file's smoothing)
//     measures surface n-gram overlap, not semantic equivalence, so a
//     perfectly good paraphrase that happens to reuse few of the
//     Reference's exact words scores about as badly as a wrong
//     translation. Real translation-quality pipelines mitigate this with
//     multiple references, embedding-based metrics (BERTScore, COMET), or
//     human/LLM-judge scoring alongside BLEU -- none of which are
//     implemented here; this entry exists precisely to make that gap
//     visible and documented rather than silently surprising, per this
//     package's "groundwork against fakes, not a finished accuracy
//     suite" ethos.
//
//   - very_short_pair_yes_sir: a 2-word Reference ("yes sir") against a
//     1-word Candidate ("yes", dropping the address term "sir") --
//     exercises both bleu.go's documented "effective order" reduction
//     (only unigram precision is considered for a 1-word candidate; no
//     bigram/trigram/4-gram order is even attempted) and the brevity
//     penalty at the shortest realistic scale (BP = exp(1 - 2/1) ~
//     0.368), together giving BLEU ~0.368 for an otherwise-correct
//     single surviving word.
//
// Sprint 2026-07-28 (QA) adds six further entries covering
// translation-quality failure shapes the original six didn't exercise,
// found while auditing every existing entry (per this doc comment) for
// gaps rather than retreading an already-covered mechanic under a new
// name:
//
//   - word_reordering_full_reversal_delivery_schedule: Reference and
//     Candidate share the exact same 7-word bag of words with the
//     Candidate's order completely reversed -- distinct from every
//     existing entry, none of which tests reordering in isolation (the
//     substitution/deletion/unrelated entries all change *which* words
//     are present, not merely their order). Every unigram matches (the
//     vocabulary is identical), so p1 = 7/7 = 1.0, but a full reversal
//     shares zero bigrams, trigrams, or 4-grams with the reference, so
//     p2/p3/p4 all fall to the epsilon-smoothed floor (0.1/6, 0.1/5,
//     0.1/4). BP = 1.0 (equal length). BLEU ~0.054 -- demonstrating that
//     a candidate can have perfect unigram precision (every word is
//     "correct" in isolation) yet still score almost as low as the
//     completely-unrelated-candidate entry once word order is accounted
//     for, since BLEU's higher-order n-grams are what actually encode
//     word order.
//
//   - partial_overlap_first_half_matches_delivery_estimate: the
//     Candidate matches the Reference exactly for its first five words
//     and diverges completely for its last three -- a genuine middle
//     ground between perfect_identical_translation (BLEU 1.0) and
//     completely_unrelated_candidate (BLEU ~0.023) that no existing
//     entry occupies (every other non-perfect entry here is either a
//     single-word edit, near-total mismatch, or a length-only
//     difference). Precisions decrease smoothly with n-gram order as
//     the surviving prefix contributes fewer and fewer intact n-grams
//     the longer the order: p1 = 5/8, p2 = 4/7, p3 = 3/6, p4 = 2/5. BP =
//     1.0 (equal length, 8 == 8). BLEU ~0.517, roughly midway between
//     the identical and unrelated extremes as intended.
//
//   - hallucinated_added_clause_complaint_registered: the exact opposite
//     construction from missing_clause_deletion_within_48_hours above --
//     instead of dropping a trailing clause from the Reference, the
//     Candidate here is the 5-word Reference plus a wholly invented
//     6-word trailing clause ("and will be resolved soon") that was
//     never part of the ground truth, modeling an MT system that
//     hallucinates additional, unverified content rather than omitting
//     real content. Because the Candidate is *longer* than the
//     Reference, BP = 1.0 (no brevity penalty at all, the mirror image
//     of the deletion entry's BP ~0.741 with all-1.0 precisions) --
//     here every precision is instead dragged below 1.0 by the
//     hallucinated words diluting each n-gram order's denominator: p1 =
//     5/10, p2 = 4/9, p3 = 3/8, p4 = 2/7. BLEU ~0.393. Together these two
//     entries show BLEU catching a dropped clause and an added clause
//     through two different mechanisms (brevity penalty vs. precision
//     dilution), not the same one.
//
//   - named_entity_mismatch_relationship_manager_name: a single
//     substituted proper noun (an agent's name, "rahul" -> "suresh") in
//     an otherwise word-for-word-identical 11-word translation -- the
//     realistic "MT gets a name or number wrong" failure mode the PM
//     charter's WER/BLEU tracking exists to catch, and a named-entity
//     counterpart to one_word_substitution_currency_mismatch's numeric
//     mismatch. Unlike that entry, whose single substitution sits at the
//     very last word (costing exactly one n-gram at every order), this
//     entry's mismatch sits mid-sentence (word 4 of 11), so it
//     contaminates every n-gram that overlaps that position: p1 = 10/11,
//     p2 = 8/10, p3 = 6/9, p4 = 4/8. BP = 1.0 (equal length). BLEU
//     ~0.702 -- higher than the end-of-sentence currency mismatch
//     (~0.841) is low relative to only one wrong word, but demonstrates
//     that *where* a single-token error falls changes how many n-grams
//     it breaks, not just whether one exists.
//
//   - case_only_difference_payment_capitalization: the Candidate is
//     character-for-character identical to the 8-word Reference except
//     one word's capitalization ("payment" -> "Payment"), modeling an MT
//     system that preserves source-side title-casing it shouldn't have.
//     BLEUScore performs no case-folding (see bleu.go's doc comment,
//     mirroring WordErrorRate's documented case-sensitivity), so this
//     single-word case difference counts as a full token mismatch at
//     every n-gram order it touches, exactly like a genuine wrong-word
//     substitution would: p1 = 7/8, p2 = 5/7, p3 = 4/6, p4 = 3/5. BP =
//     1.0 (equal length). BLEU ~0.707 for a translation a human reviewer
//     would call perfect -- this entry exists specifically to document
//     that case-sensitivity gap concretely in the fixed corpus, the same
//     way paraphrase_semantically_fine_lexically_different documents
//     BLEU's paraphrase blind spot, rather than only being covered by
//     bleu_test.go's function-level TestBLEUScore_IsCaseSensitive.
//
//   - over_repeated_word_clipping_billing_transfer: the Candidate is a
//     degenerate 8-word repetition of the single token "the" against an
//     8-word Reference that itself contains "the" only twice -- a
//     realistic MT disfluency-loop failure mode (a translation model
//     stuck repeating one high-frequency token) and the corpus-level
//     counterpart to bleu_test.go's unit-level clipping tests
//     (TestClippedNGramMatches_ClipsOverRepeatedWord,
//     TestBLEUScore_OverRepeatedWordDoesNotInflateScore). BLEU's
//     modified-precision clipping caps the credited unigram matches at
//     the Reference's own count of "the" (2), not the Candidate's raw
//     repeat count (8), so p1 = 2/8 despite every single Candidate token
//     technically appearing in the Reference; p2/p3/p4 all fall to the
//     epsilon-smoothed floor since no repeated-"the" bigram/trigram/
//     4-gram exists in the Reference at all. BP = 1.0 (equal length).
//     BLEU ~0.033, about as low as the completely-unrelated entry --
//     demonstrating clipping prevents this degenerate repetition from
//     scoring anywhere near as well as its raw (unclipped) unigram
//     overlap might otherwise suggest.
//
// Sprint 2026-08-10 (QA) adds six further entries covering
// translation-quality failure shapes the original twelve didn't
// exercise:
//
//   - word_order_adjacent_swap_end_delivery_schedule: a localized
//     two-word adjacent swap at the very end of a 6-word sentence
//     ("delivered tomorrow" -> "tomorrow delivered"), the counterpart to
//     word_reordering_full_reversal_delivery_schedule's full-sentence
//     reversal -- here every unigram still matches (p1 = 1.0, the
//     vocabulary is identical) but, unlike the full reversal (which
//     collapses every higher-order precision to the epsilon floor),
//     only the n-grams overlapping the swapped pair are broken, so
//     p2/p3/p4 degrade gracefully (3/5, 1/2, 1/3) instead of collapsing
//     entirely. BP = 1.0 (equal length). Demonstrates that BLEU's
//     sensitivity to reordering scales with how much of the sentence is
//     disturbed, not just whether any reordering occurred at all;
//
//   - two_scattered_substitutions_currency_and_status_refund: two
//     independent single-word substitutions ("rupees"->"dollars" at
//     word 6, "processed"->"completed" at word 9 of a 9-word sentence)
//     placed close enough together that a single 4-gram spans both --
//     distinct from named_entity_mismatch_relationship_manager_name and
//     one_word_substitution_currency_mismatch (each exactly one
//     substitution): p1 = 7/9, p2 = 5/8, p3 = 3/7, p4 = 2/6, each
//     computed by counting exactly which n-grams overlap either error
//     position (with the 4-gram order's two error-touching windows
//     overlapping into one, not two, broken 4-gram, since both errors
//     fall within a single 4-word span). BP = 1.0 (equal length).
//     Verifies BLEUScore's clipping/counting behaves correctly once two
//     independent errors' broken-n-gram windows overlap, not just when
//     they're far enough apart to be independent;
//
//   - short_pair_leading_word_insertion_call_disconnected: a 3-word
//     reference ("call is disconnected") against a 4-word candidate
//     with one hallucinated leading word ("the call is disconnected")
//     -- exercises the full effective order (maxN = min(4, 4) = 4, all
//     four precisions computed) at short-sentence scale, contrasting
//     with very_short_pair_yes_sir (effective order 1, a 1-word
//     candidate) and very_short_pair_substitution_yes_maam below
//     (effective order 2): p1 = 3/4, p2 = 2/3, p3 = 1/2, and p4 is
//     epsilon-smoothed (0.1/1) since the candidate's only 4-gram
//     ("the call is disconnected") has zero matches in a reference too
//     short to contain any 4-gram at all. BP = 1.0 (candidate longer
//     than reference, 4 > 3);
//
//   - stutter_duplicated_trailing_word_confirmed_confirmed: a 5-word
//     reference with its own final word repeated once more by the
//     candidate ("...confirmed" -> "...confirmed confirmed") -- a
//     realistic single-repeat MT/ASR-style disfluency, contrasting with
//     over_repeated_word_clipping_billing_transfer's much more
//     degenerate 8x-repeated-single-token candidate. Clipping still
//     caps the credited match for "confirmed" at the reference's own
//     count (1), not the candidate's count (2): p1 = 5/6, p2 = 4/5,
//     p3 = 3/4, p4 = 2/3. BP = 1.0 (candidate longer, 6 > 5).
//     Demonstrates clipping matters even for the mildest possible
//     over-repetition (one extra copy), not just the extreme
//     all-one-word case;
//
//   - very_short_pair_substitution_yes_maam: a 2-word reference ("yes
//     sir") against a 2-word candidate substituting the address term
//     ("yes sir" -> "yes ma'am") -- unlike very_short_pair_yes_sir
//     (a deletion, effective order 1), this keeps the candidate at the
//     reference's own length so effective order reaches 2 (both p1 and
//     p2 are computed): p1 = 1/2, p2 is epsilon-smoothed (0.1/1) since
//     the candidate's only bigram doesn't match the reference's. BP =
//     1.0 (equal length). Fills the "effective order 2" gap between
//     the existing effective-order-1 and effective-order-4 short
//     entries;
//
//   - combined_substitution_and_trailing_hallucination_complaint_resolved_closed:
//     a 5-word reference with both a substitution ("resolved" ->
//     "closed") and a hallucinated trailing word ("now") appended by
//     the candidate -- distinct from every existing single-mechanism
//     entry (a pure substitution at unchanged length, or a pure
//     length-only change with perfect precision): p1 = 4/6, p2 = 3/5,
//     p3 = 1/2, p4 = 1/3, each precision dragged down by both the
//     substitution and the trailing dilution together rather than by
//     either mechanism alone. BP = 1.0 (candidate longer, 6 > 5).
//
// Sprint 2026-08-11 (QA) adds six further entries covering
// translation-quality failure shapes the existing eighteen didn't
// exercise:
//
//   - word_splitting_helpline_compound_translation: the Candidate splits
//     the Reference's single compound token "helpline" into two words
//     "help line" -- the BLEU counterpart to corpus.go's
//     hinglish_word_splitting_helpline_compound, not previously
//     exercised on the translation-quality side. p1 = 6/8, p2 = 4/7,
//     p3 = 2/6, p4 is epsilon-smoothed (0.1/5) since no 4-gram survives
//     the split intact. BP = 1.0 (candidate longer, 8 > 7);
//
//   - word_merging_signup_process_confirmation: the mirror image --
//     the Candidate merges the Reference's two words "sign up" into
//     one token "signup". p1 = 5/6, p2 = 3/5, p3 = 1/2, p4 = 1/3. BP =
//     exp(1 - 7/6) since the merged Candidate is one word shorter than
//     the Reference;
//
//   - transposition_mid_sentence_will_be_delivered: an adjacent two-word
//     swap ("will be" -> "be will") sitting in the middle of an 8-word
//     sentence, not at the edge like
//     word_order_adjacent_swap_end_delivery_schedule's end-of-sentence
//     swap -- p1 = 1.0 (identical bag of words), p2 = 4/7, p3 = 1/3,
//     p4 = 1/5. BP = 1.0 (equal length, 8 == 8);
//
//   - code_switching_untranslated_source_word_khata: a code-switching
//     residue failure -- the Candidate leaves one Hindi word ("khata")
//     untranslated in the middle of an otherwise-perfect English
//     translation, in place of the Reference's "account". Distinct from
//     named_entity_mismatch_relationship_manager_name (a wrong proper
//     noun, still fully translated) since this token was never
//     translated at all. p1 = 6/7, p2 = 4/6, p3 = 2/5, p4 = 1/4. BP =
//     1.0 (equal length, 7 == 7);
//
//   - homophone_confusion_target_language_their_there: the Candidate
//     substitutes the homophone "their" for the Reference's "there" --
//     the target-language counterpart to corpus.go's
//     hinglish_homophone_to_too_confirmation_query, not previously
//     exercised for BLEU. p1 = 7/8, p2 = 5/7, p3 = 1/2, p4 = 1/5. BP =
//     1.0 (equal length, 8 == 8);
//
//   - number_date_formatting_difference_24hr_vs_12hr: the Candidate
//     collapses the Reference's spoken 12-hour time "5 pm" (two words)
//     into a 24-hour numeral "17:00" (one word) -- a number/date
//     formatting difference, distinct from every substitution/deletion
//     entry above since no wrong value is stated, only a different
//     representation of the same time. p1 = 5/6, p2 = 3/5, p3 = 1/2,
//     p4 = 1/3. BP = exp(1 - 7/6) since the reformatted Candidate is one
//     word shorter than the Reference.
func FixedTranslationCorpus() []TranslationCorpusEntry {
	return []TranslationCorpusEntry{
		{
			Name:           "perfect_identical_translation",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka order confirm ho gaya hai",
			Reference:      "sir your order has been confirmed",
			Candidate:      "sir your order has been confirmed",
		},
		{
			Name:           "one_word_substitution_currency_mismatch",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka balance paanch sau rupaye hai",
			Reference:      "sir your account balance is five hundred rupees",
			Candidate:      "sir your account balance is five hundred dollars",
		},
		{
			Name:           "missing_clause_deletion_within_48_hours",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapki complaint register ho gayi hai aur yeh 48 ghante ke andar resolve ho jayegi",
			Reference:      "sir your complaint has been registered and will be resolved within 48 hours",
			Candidate:      "sir your complaint has been registered and will be resolved",
		},
		{
			Name:           "completely_unrelated_candidate",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka refund safaltapoorvak process ho gaya hai",
			Reference:      "your refund has been processed successfully",
			Candidate:      "the weather today is quite pleasant",
		},
		{
			Name:           "paraphrase_semantically_fine_lexically_different",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir line par rukiye main aapko transfer kar raha hoon",
			Reference:      "please hold the line while I transfer your call",
			Candidate:      "kindly wait a moment as I connect you to another agent",
		},
		{
			Name:           "very_short_pair_yes_sir",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "haan sir",
			Reference:      "yes sir",
			Candidate:      "yes",
		},

		// --- Sprint 2026-07-28 (QA) additions below: six more entries
		// covering translation-quality failure shapes the original six
		// entries didn't exercise. See the doc comment above for each
		// entry's full rationale and hand-computed BLEU.
		{
			Name:           "word_reordering_full_reversal_delivery_schedule",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapki delivery kal ke liye schedule ho chuki hai",
			Reference:      "your delivery has been scheduled for tomorrow",
			Candidate:      "tomorrow for scheduled been has delivery your",
		},
		{
			Name:           "partial_overlap_first_half_matches_delivery_estimate",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka order kal shaam tak deliver ho jayega",
			Reference:      "your order will be delivered by tomorrow evening",
			Candidate:      "your order will be delivered next week sometime",
		},
		{
			Name:           "hallucinated_added_clause_complaint_registered",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapki complaint register ho gayi hai",
			Reference:      "your complaint has been registered",
			Candidate:      "your complaint has been registered and will be resolved soon",
		},
		{
			Name:           "named_entity_mismatch_relationship_manager_name",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapke relationship manager rahul aapko shaam 6 baje call karenge",
			Reference:      "your relationship manager rahul will call you at 6 pm today",
			Candidate:      "your relationship manager suresh will call you at 6 pm today",
		},
		{
			Name:           "case_only_difference_payment_capitalization",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka paanch sau rupaye ka payment confirm ho gaya hai",
			Reference:      "your payment of five hundred rupees is confirmed",
			Candidate:      "your Payment of five hundred rupees is confirmed",
		},
		{
			Name:           "over_repeated_word_clipping_billing_transfer",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "please mujhe billing department mein transfer kar dijiye",
			Reference:      "please transfer the call to the billing department",
			Candidate:      "the the the the the the the the",
		},

		// --- Sprint 2026-08-10 (QA) additions below: six more entries.
		// See the doc comment above for each entry's full rationale and
		// hand-computed BLEU.
		{
			Name:           "word_order_adjacent_swap_end_delivery_schedule",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka order kal shaam tak deliver ho jayega",
			Reference:      "your order will be delivered tomorrow",
			Candidate:      "your order will be tomorrow delivered",
		},
		{
			Name:           "two_scattered_substitutions_currency_and_status_refund",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka refund paanch sau rupaye process ho gaya hai",
			Reference:      "your refund of five hundred rupees has been processed",
			Candidate:      "your refund of five hundred dollars has been completed",
		},
		{
			Name:           "short_pair_leading_word_insertion_call_disconnected",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "call cut gaya hai",
			Reference:      "call is disconnected",
			Candidate:      "the call is disconnected",
		},
		{
			Name:           "stutter_duplicated_trailing_word_confirmed_confirmed",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapka order confirm ho gaya hai",
			Reference:      "your order has been confirmed",
			Candidate:      "your order has been confirmed confirmed",
		},
		{
			Name:           "very_short_pair_substitution_yes_maam",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "haan madam",
			Reference:      "yes sir",
			Candidate:      "yes ma'am",
		},
		{
			Name:           "combined_substitution_and_trailing_hallucination_complaint_resolved_closed",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "sir aapki complaint resolve ho gayi hai",
			Reference:      "your complaint has been resolved",
			Candidate:      "your complaint has been closed now",
		},

		// --- Sprint 2026-08-11 (QA) additions below: six more entries.
		// See the doc comment above for each entry's full rationale and
		// hand-computed BLEU.
		{
			Name:           "word_splitting_helpline_compound_translation",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "kripya sahayata ke liye hamare helpline number par call karein",
			Reference:      "please call our helpline number for support",
			Candidate:      "please call our help line number for support",
		},
		{
			Name:           "word_merging_signup_process_confirmation",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "kripya naye process ke liye sign up karein",
			Reference:      "please sign up for the new process",
			Candidate:      "please signup for the new process",
		},
		{
			Name:           "transposition_mid_sentence_will_be_delivered",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "aapka parcel shaam tak deliver ho jayega",
			Reference:      "your parcel will be delivered by evening today",
			Candidate:      "your parcel be will delivered by evening today",
		},
		{
			Name:           "code_switching_untranslated_source_word_khata",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "kripya apna bank khata number ab share karein",
			Reference:      "please share your bank account number now",
			Candidate:      "please share your bank khata number now",
		},
		{
			Name:           "homophone_confusion_target_language_their_there",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "kripya parcel darwaze par wahan rakh dein",
			Reference:      "please leave the package there at the door",
			Candidate:      "please leave the package their at the door",
		},
		{
			Name:           "number_date_formatting_difference_24hr_vs_12hr",
			SourceLanguage: "hi",
			TargetLanguage: "en",
			Source:         "aapka appointment kal shaam paanch baje hai",
			Reference:      "your appointment is at 5 pm tomorrow",
			Candidate:      "your appointment is at 17:00 tomorrow",
		},
	}
}
