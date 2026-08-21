package qa

import (
	"math"
	"testing"
)

// emptyHypothesisEntries lists the corpus entries whose Hypothesis is
// deliberately "" (a genuine total-deletion/silence-timeout shape -- see
// FixedCorpus's Sprint 2026-07-21 doc comment on
// hinglish_total_deletion_empty_hypothesis_silence_timeout), so
// TestFixedCorpus_EntriesAreWellFormed's empty-Hypothesis guard (added to
// catch the silent-fixture-bug class this repo has been bitten by before)
// can allow exactly this one intentional exception by name instead of
// disabling the guard for every entry.
var emptyHypothesisEntries = map[string]bool{
	"hinglish_total_deletion_empty_hypothesis_silence_timeout": true,
}

// emptyReferenceEntries lists the corpus entries whose Reference is
// deliberately "" (a genuine hallucination-from-true-silence shape -- see
// FixedCorpus's Sprint 2026-07-23 doc comment on
// hinglish_hallucination_from_true_silence_empty_reference), the mirror
// image of emptyHypothesisEntries above, so
// TestFixedCorpus_EntriesAreWellFormed's empty-Reference guard can allow
// exactly this one intentional exception by name instead of disabling the
// guard for every entry.
var emptyReferenceEntries = map[string]bool{
	"hinglish_hallucination_from_true_silence_empty_reference": true,
}

// TestFixedCorpus_EntriesAreWellFormed guards against the exact class of
// silent-fixture bug this repo has been bitten by before (an empty/
// malformed fixture that "passes" trivially): every entry must have a
// name, language, non-empty reference text, and a non-empty PCM frame, and
// names must be unique so a future accidental duplicate doesn't silently
// shadow another entry in test output. Hypothesis must also be non-empty,
// except for the one entry in emptyHypothesisEntries above, whose empty
// Hypothesis is the deliberate point of that entry (a total-deletion/
// silence-timeout shape), not a malformed fixture.
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
		if e.Reference == "" && !emptyReferenceEntries[e.Name] {
			t.Errorf("entry %q has empty Reference", e.Name)
		}
		if e.Reference != "" && emptyReferenceEntries[e.Name] {
			t.Errorf("entry %q is listed in emptyReferenceEntries but has a non-empty Reference %q -- update emptyReferenceEntries or this entry", e.Name, e.Reference)
		}
		if e.Hypothesis == "" && !emptyHypothesisEntries[e.Name] {
			t.Errorf("entry %q has empty Hypothesis", e.Name)
		}
		if e.Hypothesis != "" && emptyHypothesisEntries[e.Name] {
			t.Errorf("entry %q is listed in emptyHypothesisEntries but has a non-empty Hypothesis %q -- update emptyHypothesisEntries or this entry", e.Name, e.Hypothesis)
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
//
// Sprint 2026-07-12 (QA): includes the three Hindi/English code-switching
// ("Hinglish") entries added to FixedCorpus alongside the original English
// entries — WordErrorRate tokenizes purely on whitespace (see wer.go's doc
// comment), so a code-switched sentence mixing Devanagari and English
// words within one string is expected to align/diff correctly exactly like
// a single-script sentence; these cases confirm that in practice, not just
// by argument.
//
// Sprint 2026-07-13 (QA): includes nine more, harder Hinglish entries
// (mid-sentence switches, embedded loanwords, numbers/dates, digit
// sequences, filler words, an insertion case, and a two-substitution
// case) — see FixedCorpus's doc comment for each entry's reasoning and
// hand-computed WER.
//
// Sprint 2026-07-14 (QA): includes ten further entries (a multi-word
// deletion, proper-noun brand/person-name substitutions, a number-word-
// vs-digit substitution, two long utterances, a content-word deletion, a
// hallucinated-insertion case, a reverse-direction English-dominant
// entry, and a digit-sequence deletion) — see FixedCorpus's doc comment
// for each entry's reasoning and hand-computed WER.
//
// Sprint 2026-07-15 (QA): includes five further entries (a negation-word
// deletion, two acronym/homophone substitutions, a two-insertion case,
// and a second long-utterance entry with a multi-word deletion) — see
// FixedCorpus's doc comment for each entry's reasoning and hand-computed
// WER.
//
// Sprint 2026-07-16 (QA): includes five further entries (a third
// acronym/homophone substitution, a digit-duplication insertion, a
// trailing-position insertion, a long utterance mixing a substitution and
// a deletion together, and a long utterance with two insertions) — see
// FixedCorpus's doc comment for each entry's reasoning and hand-computed
// WER.
//
// Sprint 2026-07-17 (QA): includes six further entries (an isolated
// leading-position insertion, a word-splitting case, a word-merging case,
// an adjacent-word transposition, a case-sensitivity mismatch, and a
// severe-hallucination case demonstrating WER > 1.0) — see FixedCorpus's
// doc comment for each entry's reasoning and hand-computed WER.
//
// Sprint 2026-07-20 (QA): includes six further entries (a punctuation-only
// mismatch, a three-error-type mix in one sentence, a currency-symbol-vs-
// spelled-out-words mismatch, a total-substitution-failure case with
// WER == 1.0 via pure substitution, a short common-word homophone
// substitution, and three non-adjacent deletions in one sentence) — see
// FixedCorpus's doc comment for each entry's reasoning and hand-computed
// WER.
//
// Sprint 2026-07-21 (QA): includes five further entries (a total-deletion
// entry via a genuinely empty hypothesis, a combined deletion+insertion
// entry with no substitution, a contiguous three-word phrase-repeat
// insertion, a systematic repeated-word substitution, and a trailing
// contiguous three-word deletion modeling a truncated/cut-off call) —
// see FixedCorpus's doc comment for each entry's reasoning and
// hand-computed WER.
//
// Sprint 2026-07-23 (QA): includes five further entries (a leading
// contiguous two-word deletion block, a reference-side repeated word
// collapsed by the fake ASR, three non-adjacent substitutions in a long
// utterance, a contiguous three-word substitution block, and a
// hallucination-from-true-silence case with an empty Reference) — see
// FixedCorpus's doc comment for each entry's reasoning and hand-computed
// WER.
//
// Sprint 2026-07-27 (QA): includes five further entries (three
// non-adjacent insertions, a digit-value misrecognition within an
// unchanged numeral, a trailing hallucinated multi-word clause, a
// "bookend" leading-deletion-plus-trailing-insertion mix, and a
// pure-insertion entry reaching WER == 1.0) — see FixedCorpus's doc
// comment for each entry's reasoning and hand-computed WER.
//
// Sprint 2026-07-28 (QA): includes five further entries (a combined
// two-deletions-plus-two-insertions entry, a long utterance isolating
// exactly one substitution, a WER > 1.0 entry driven by a mix of one
// substitution and four insertions, a reference-side repeated word
// misheard as a substitution rather than collapsed, and a long-distance
// non-adjacent two-word swap) — see FixedCorpus's doc comment for each
// entry's reasoning and hand-computed WER.
//
// Sprint 2026-08-10 (QA): includes eight further entries (a long
// utterance isolating exactly one insertion, a contiguous two-word
// insertion block of brand-new words, a fully-deleted repeated
// reference word, a three-word-phrase-to-one-word collapse, a
// cross-language acoustic homophone, a contiguous two-word substitution
// block, a magnitude-word substitution, and a transliteration
// spelling-variant mismatch) — see FixedCorpus's doc comment for each
// entry's reasoning and hand-computed WER.
func TestFixedCorpus_PrecomputedWERMatches(t *testing.T) {
	want := map[string]float64{
		"identical_greeting":              0.0,
		"one_word_substitution":           1.0 / 5.0,
		"one_word_deletion":               1.0 / 7.0,
		"hinglish_identical_order_status": 0.0,
		"hinglish_one_word_substitution":  1.0 / 6.0,
		"hinglish_one_word_deletion":      1.0 / 7.0,

		// Sprint 2026-07-13 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_midsentence_switch_payment_status":     1.0 / 12.0,
		"hinglish_loanword_recharge_request":             1.0 / 7.0,
		"hinglish_numbers_bill_amount_and_date":          1.0 / 12.0,
		"hinglish_order_number_spoken_in_english_digits": 1.0 / 9.0,
		"hinglish_filler_words_address_update":           1.0 / 9.0,
		"hinglish_otp_request_insertion":                 1.0 / 5.0,
		"hinglish_call_disconnect_network_issue":         0.0,
		"hinglish_account_block_query_two_substitutions": 2.0 / 13.0,
		"hinglish_callback_request_deletion_and_filler":  1.0 / 10.0,

		// Sprint 2026-07-14 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_two_word_deletion_travel_booking_confirmation":  2.0 / 13.0,
		"hinglish_proper_noun_brand_substitution_recharge":        1.0 / 8.0,
		"hinglish_proper_noun_person_name_substitution_order":     1.0 / 11.0,
		"hinglish_number_word_vs_digit_substitution":              1.0 / 9.0,
		"hinglish_long_utterance_single_deletion_callback":        1.0 / 25.0,
		"hinglish_content_word_deletion_parcel_delivery_date":     1.0 / 12.0,
		"hinglish_insertion_hallucinated_filler_word":             1.0 / 7.0,
		"english_dominant_embedded_hindi_courtesy_agent_transfer": 1.0 / 12.0,
		"hinglish_digit_sequence_deletion_account_number":         1.0 / 10.0,
		"hinglish_long_utterance_two_substitutions_refund_status": 2.0 / 18.0,

		// Sprint 2026-07-15 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_negation_deletion_service_unavailable":                1.0 / 7.0,
		"hinglish_acronym_kyc_homophone_substitution":                   1.0 / 7.0,
		"hinglish_two_insertions_confirmation_repeat":                   2.0 / 5.0,
		"hinglish_acronym_ivr_homophone_substitution":                   1.0 / 7.0,
		"hinglish_long_utterance_two_deletions_kyc_document_submission": 2.0 / 20.0,

		// Sprint 2026-07-16 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_acronym_emi_homophone_substitution":                                  1.0 / 8.0,
		"hinglish_digit_duplication_insertion_registered_mobile_number":                1.0 / 10.0,
		"hinglish_insertion_trailing_word_repeat_call_end":                             1.0 / 5.0,
		"hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation": 2.0 / 24.0,
		"hinglish_long_utterance_two_insertions_delivery_confirmation":                 2.0 / 21.0,

		// Sprint 2026-07-17 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_insertion_leading_word_repeat_call_open":             1.0 / 6.0,
		"hinglish_word_splitting_helpline_compound":                    2.0 / 6.0,
		"hinglish_word_merging_update_profile_request":                 2.0 / 7.0,
		"hinglish_adjacent_word_transposition_balance_check":           2.0 / 6.0,
		"hinglish_case_sensitivity_capitalized_sir_mismatch":           1.0 / 6.0,
		"hinglish_severe_hallucination_wer_exceeds_one_listen_request": 5.0 / 3.0,

		// Sprint 2026-07-20 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_punctuation_only_mismatch_confirm_query":          1.0 / 5.0,
		"hinglish_three_error_types_mixed_appointment_reschedule":   3.0 / 11.0,
		"hinglish_currency_symbol_vs_words_bill_amount":             2.0 / 5.0,
		"hinglish_total_substitution_failure_balance_request":       5.0 / 5.0,
		"hinglish_homophone_to_too_confirmation_query":              1.0 / 6.0,
		"hinglish_three_nonadjacent_deletions_complaint_resolution": 3.0 / 16.0,

		// Sprint 2026-07-21 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_total_deletion_empty_hypothesis_silence_timeout":               7.0 / 7.0,
		"hinglish_deletion_and_insertion_no_substitution_order_confirmation":     2.0 / 8.0,
		"hinglish_three_word_phrase_repeat_insertion_order_confirmation":         3.0 / 7.0,
		"hinglish_systematic_repeated_word_substitution_hai_hain_verb_agreement": 2.0 / 11.0,
		"hinglish_trailing_three_word_deletion_call_cutoff_complaint_update":     3.0 / 14.0,

		// Sprint 2026-07-23 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_leading_two_word_deletion_call_greeting":                2.0 / 8.0,
		"hinglish_reference_repeated_word_collapsed_dhanyavaad":           1.0 / 8.0,
		"hinglish_three_nonadjacent_substitutions_payment_confirmation":   3.0 / 18.0,
		"hinglish_contiguous_three_word_substitution_block_refund_status": 3.0 / 8.0,
		"hinglish_hallucination_from_true_silence_empty_reference":        1.0,

		// Sprint 2026-07-27 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_three_nonadjacent_insertions_appointment_confirmation":        3.0 / 9.0,
		"hinglish_digit_value_misrecognition_account_number":                    1.0 / 6.0,
		"hinglish_trailing_hallucinated_clause_insertion_call_closing":          4.0 / 7.0,
		"hinglish_bookend_leading_deletion_trailing_insertion_greeting_closing": 2.0 / 8.0,
		"hinglish_insertion_only_wer_equals_one_order_confirmation":             4.0 / 4.0,

		// Sprint 2026-07-28 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_two_deletions_two_insertions_no_substitution_address_update":     4.0 / 9.0,
		"hinglish_long_utterance_single_substitution_call_timing_confirmation":     1.0 / 21.0,
		"hinglish_wer_exceeds_one_mixed_substitution_and_insertions_balance_query": 5.0 / 4.0,
		"hinglish_reference_repeated_word_substituted_haan_haan":                   1.0 / 9.0,
		"hinglish_long_distance_word_swap_account_status_request":                  2.0 / 6.0,

		// Sprint 2026-08-10 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_long_utterance_single_insertion_isolated_delivery_status":        1.0 / 21.0,
		"hinglish_contiguous_two_word_insertion_block_hold_music_apology":          2.0 / 8.0,
		"hinglish_reference_repeated_word_fully_deleted_bilkul_bilkul":             2.0 / 9.0,
		"hinglish_three_word_phrase_collapsed_to_one_word_customer_care_executive": 3.0 / 10.0,
		"hinglish_cross_language_homophone_call_kal_confusion":                     1.0 / 6.0,
		"hinglish_two_word_contiguous_substitution_block_status_query":             2.0 / 8.0,
		"hinglish_magnitude_word_substitution_lakh_hazar_confusion":                1.0 / 10.0,
		"hinglish_transliteration_spelling_variant_dhanyavad_mismatch":             1.0 / 7.0,

		// Sprint 2026-08-11 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_mid_sentence_hallucinated_insertion_crosstalk":            1.0 / 7.0,
		"hinglish_date_format_digit_vs_spoken_words_mismatch":               2.0 / 6.0,
		"hinglish_full_word_order_reversal_no_substitution_parcel_delivery": 6.0 / 7.0,
		"hinglish_gender_agreement_homophone_unka_unke_substitution":        1.0 / 5.0,
		"hinglish_negation_flip_substitution_nahi_ho_refund_status":         1.0 / 6.0,
		"hinglish_acronym_expansion_substitution_emi_installment":           1.0 / 7.0,

		// Sprint 2026-08-12 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_honorific_marker_ji_deletion":                       1.0 / 7.0,
		"hinglish_unit_of_measurement_kilometer_miles_substitution":   1.0 / 7.0,
		"hinglish_numeral_comma_grouping_formatting_substitution":     1.0 / 6.0,
		"hinglish_alphanumeric_bank_code_ifsc_substitution":           1.0 / 6.0,
		"hinglish_politeness_register_aap_tum_downgrade_substitution": 1.0 / 7.0,
		"hinglish_currency_subunit_paise_rupee_substitution":          1.0 / 8.0,
		"hinglish_devanagari_numeral_script_substitution":             1.0 / 6.0,

		// Sprint 2026-08-13 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_contraction_expansion_wont_will_not_substitution":           2.0 / 8.0,
		"hinglish_ordinal_number_word_vs_digit_substitution_teesri_complaint": 1.0 / 8.0,
		"hinglish_phone_number_spaced_digit_grouping_collapse":                5.0 / 9.0,
		"hinglish_truncated_word_cutoff_mid_utterance_call_drop":              1.0 / 7.0,
		"hinglish_full_utterance_echo_duplication_order_confirmed":            7.0 / 7.0,
		"hinglish_sentence_final_particle_na_deletion_confirmation_query":     1.0 / 5.0,

		// Sprint 2026-08-17 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_place_name_substitution_city_mumbai_pune_query":        1.0 / 8.0,
		"hinglish_addressee_honorific_gender_substitution_sir_madam":     1.0 / 6.0,
		"hinglish_nonlexical_filler_hallucination_umm_insertion":         1.0 / 7.0,
		"hinglish_hedge_word_deletion_lagbhag_approximate_bill_amount":   1.0 / 8.0,
		"hinglish_spelled_out_letter_substitution_pin_code_confirmation": 1.0 / 10.0,
		"hinglish_tense_marker_substitution_future_past_delivery_status": 1.0 / 6.0,

		// Sprint 2026-08-21 (QA) additions, see FixedCorpus's doc comment
		// for the reasoning behind each entry's error shape.
		"hinglish_two_word_phrase_repeat_insertion_hold_please":        2.0 / 7.0,
		"hinglish_decimal_point_marker_deletion_parcel_weight":         1.0 / 10.0,
		"hinglish_postposition_case_marker_substitution_ko_ki":         1.0 / 7.0,
		"hinglish_day_of_week_substitution_monday_tuesday_appointment": 1.0 / 8.0,
		"hinglish_conjunction_word_deletion_aur_bill_and_receipt":      1.0 / 7.0,
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
