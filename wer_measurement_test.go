// Package langstream_test - QA's first "Real WER... measurement" datapoint
// (ROADMAP.md Week 4: "Real WER, latency, and CSAT measurement"), wiring
// pkg/qa's WordErrorRate calculator and fixed corpus (pkg/qa/corpus.go)
// up against the same fake-vendor-server infrastructure
// integration_vendor_test.go already uses (newFakeSarvamASRServer), rather
// than a live vendor endpoint.
//
// GROUNDWORK, NOT A LIVE MEASUREMENT - same caveat as pkg/qa's package doc
// comment: every "transcript" measured here comes from a fake Sarvam
// server scripted, per pkg/qa.CorpusEntry, to reply with a
// pre-authored Hypothesis string, not from real speech recognition. This
// proves the WER-measurement plumbing (real asr.Recognizer client code +
// WordErrorRate + a fixed corpus) works end to end and gives repeatable,
// hand-verifiable numbers; it is not evidence about any real vendor's
// accuracy. That requires live vendor traffic, which is explicitly out of
// scope until API keys exist (see ROADMAP.md's Week 2 decision, restated
// in integration_vendor_test.go's package doc comment).
//
// Sprint 2026-07-12 (QA) wires in pkg/qa's three new Hindi/English
// code-switching ("Hinglish") corpus entries alongside the original three
// English ones, per DEVLOG.md's 2026-07-10 entry flagging corpus expansion
// + a Hindi/code-switching case as the next-sprint QA priority "now that
// the harness exists". The fake Sarvam server (newFakeSarvamASRServer) is
// a dumb echo: it replies with whatever transcriptText string it was
// started with regardless of audio content or script, so it already
// "supports" code-switched text the same way it supports any other
// string - the point of these new cases is exercising the
// asr.SarvamRecognizer client's transcript parsing and WordErrorRate's
// whitespace tokenization against real Devanagari+English mixed text, not
// exercising anything special in the fake server itself.
//
// Sprint 2026-07-13 (QA) wires in nine further, harder Hinglish corpus
// entries (mid-sentence switches, embedded English loanwords in Hindi
// grammar, numbers/dates spoken across a language boundary, digit
// sequences, filler words, an ASR-style repeat/insertion, and a
// two-substitution case) the same way, exercising the same
// client-parsing + WER path against a wider variety of realistic
// contact-center Hinglish shapes, most of them Romanized rather than
// Devanagari-mixed (the 2026-07-12 entries already cover the Devanagari
// shape).
//
// Sprint 2026-07-14 (QA) wires in ten further entries (a multi-word
// deletion, proper-noun brand/person-name substitutions, a number-word-
// vs-digit substitution, two long utterances, a content-word deletion, a
// hallucinated-insertion case, a reverse-direction English-dominant
// entry, and a digit-sequence deletion) the same way — see
// pkg/qa/corpus.go's FixedCorpus doc comment for each entry's reasoning.
//
// Sprint 2026-07-15 (QA) wires in five further entries (a negation-word
// deletion, two acronym/homophone substitutions, a two-insertion case,
// and a second long-utterance entry with a multi-word deletion) the same
// way — see pkg/qa/corpus.go's FixedCorpus doc comment for each entry's
// reasoning.
//
// Sprint 2026-07-16 (QA) wires in five further entries (a third acronym/
// homophone substitution, a digit-duplication insertion, a
// trailing-position insertion, a long utterance mixing a substitution and
// a deletion together, and a long utterance with two insertions) the
// same way — see pkg/qa/corpus.go's FixedCorpus doc comment for each
// entry's reasoning.
//
// Sprint 2026-07-17 (QA) wires in six further entries (an isolated
// leading-position insertion, a word-splitting case, a word-merging case,
// an adjacent-word transposition, a case-sensitivity mismatch, and a
// severe-hallucination case demonstrating WER > 1.0) the same way — see
// pkg/qa/corpus.go's FixedCorpus doc comment for each entry's reasoning.
package langstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/qa"
)

// TestWERMeasurement_FixedCorpusAgainstFakeASRBackedPipeline drives the
// real asr.SarvamRecognizer client against a fake Sarvam server (see
// integration_vendor_test.go's newFakeSarvamASRServer) scripted to return
// each pkg/qa.CorpusEntry's Hypothesis transcript, then measures
// qa.WordErrorRate(entry.Reference, actualTranscript) against the fake
// pipeline's actual output - not the corpus's own precomputed expectation
// directly, so this test genuinely exercises the ASR client -> transcript
// -> WER path rather than just re-checking pkg/qa/corpus_test.go's
// arithmetic against itself.
func TestWERMeasurement_FixedCorpusAgainstFakeASRBackedPipeline(t *testing.T) {
	entries := qa.FixedCorpus()
	if len(entries) < 41 {
		t.Fatalf("qa.FixedCorpus() returned %d entries, want at least 41", len(entries))
	}

	// Precomputed expected WER for the entries this test wires up,
	// matching pkg/qa/corpus.go's FixedCorpus doc comment. Measured
	// against this test's own fake-ASR-backed transcript below (not
	// directly against the corpus's Hypothesis string) so a regression in
	// the Sarvam client's transcript parsing (e.g. truncating or mangling
	// the text, or mishandling non-ASCII/Devanagari bytes) would show up
	// as a WER mismatch here even though the corpus data itself is
	// untouched.
	wantWER := map[string]float64{
		"identical_greeting":              0.0,
		"one_word_substitution":           1.0 / 5.0,
		"one_word_deletion":               1.0 / 7.0,
		"hinglish_identical_order_status": 0.0,
		"hinglish_one_word_substitution":  1.0 / 6.0,
		"hinglish_one_word_deletion":      1.0 / 7.0,

		// Sprint 2026-07-13 (QA) additions - see pkg/qa/corpus.go's
		// FixedCorpus doc comment for each entry's reasoning and
		// hand-computed WER.
		"hinglish_midsentence_switch_payment_status":     1.0 / 12.0,
		"hinglish_loanword_recharge_request":             1.0 / 7.0,
		"hinglish_numbers_bill_amount_and_date":          1.0 / 12.0,
		"hinglish_order_number_spoken_in_english_digits": 1.0 / 9.0,
		"hinglish_filler_words_address_update":           1.0 / 9.0,
		"hinglish_otp_request_insertion":                 1.0 / 5.0,
		"hinglish_call_disconnect_network_issue":         0.0,
		"hinglish_account_block_query_two_substitutions": 2.0 / 13.0,
		"hinglish_callback_request_deletion_and_filler":  1.0 / 10.0,

		// Sprint 2026-07-14 (QA) additions - see pkg/qa/corpus.go's
		// FixedCorpus doc comment for each entry's reasoning and
		// hand-computed WER.
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

		// Sprint 2026-07-15 (QA) additions - see pkg/qa/corpus.go's
		// FixedCorpus doc comment for each entry's reasoning and
		// hand-computed WER.
		"hinglish_negation_deletion_service_unavailable":                1.0 / 7.0,
		"hinglish_acronym_kyc_homophone_substitution":                   1.0 / 7.0,
		"hinglish_two_insertions_confirmation_repeat":                   2.0 / 5.0,
		"hinglish_acronym_ivr_homophone_substitution":                   1.0 / 7.0,
		"hinglish_long_utterance_two_deletions_kyc_document_submission": 2.0 / 20.0,

		// Sprint 2026-07-16 (QA) additions - see pkg/qa/corpus.go's
		// FixedCorpus doc comment for each entry's reasoning and
		// hand-computed WER.
		"hinglish_acronym_emi_homophone_substitution":                                  1.0 / 8.0,
		"hinglish_digit_duplication_insertion_registered_mobile_number":                1.0 / 10.0,
		"hinglish_insertion_trailing_word_repeat_call_end":                             1.0 / 5.0,
		"hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation": 2.0 / 24.0,
		"hinglish_long_utterance_two_insertions_delivery_confirmation":                 2.0 / 21.0,

		// Sprint 2026-07-17 (QA) additions - see pkg/qa/corpus.go's
		// FixedCorpus doc comment for each entry's reasoning and
		// hand-computed WER.
		"hinglish_insertion_leading_word_repeat_call_open":             1.0 / 6.0,
		"hinglish_word_splitting_helpline_compound":                    2.0 / 6.0,
		"hinglish_word_merging_update_profile_request":                 2.0 / 7.0,
		"hinglish_adjacent_word_transposition_balance_check":           2.0 / 6.0,
		"hinglish_case_sensitivity_capitalized_sir_mismatch":           1.0 / 6.0,
		"hinglish_severe_hallucination_wer_exceeds_one_listen_request": 5.0 / 3.0,
	}

	tested := 0
	for _, entry := range entries {
		want, ok := wantWER[entry.Name]
		if !ok {
			continue // only wiring the known entries above, per this test's doc comment
		}
		tested++

		t.Run(entry.Name, func(t *testing.T) {
			sarvamSrv, sarvamURL := newFakeSarvamASRServer(t, entry.Hypothesis)
			defer sarvamSrv.Close()

			t.Setenv("SARVAM_API_KEY", "fake-test-key")
			rec, err := asr.NewSarvamRecognizer(asr.WithSarvamBaseURL(sarvamURL))
			if err != nil {
				t.Fatalf("NewSarvamRecognizer: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream, err := rec.StartStream(ctx, asr.Language(entry.Language))
			if err != nil {
				t.Fatalf("StartStream: %v", err)
			}
			defer stream.Close()

			frame := asr.AudioFrame{PCM: entry.PCM, SampleRate: entry.SampleRate}
			if err := stream.PushAudio(ctx, frame); err != nil {
				t.Fatalf("PushAudio: %v", err)
			}

			var transcript asr.Transcript
			select {
			case tr, ok := <-stream.Transcripts():
				if !ok {
					t.Fatal("Transcripts() channel closed before delivering a transcript")
				}
				transcript = tr
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for a transcript from the fake-Sarvam-backed recognizer")
			}

			if transcript.Text != entry.Hypothesis {
				t.Fatalf("fake ASR server returned transcript %q, want the scripted hypothesis %q (fake server or Sarvam client parsing regression)", transcript.Text, entry.Hypothesis)
			}

			got := qa.WordErrorRate(entry.Reference, transcript.Text)
			t.Logf("entry %q: measured WER = %.4f (reference=%q hypothesis=%q) [against a fake ASR server, not live vendor traffic]", entry.Name, got, entry.Reference, transcript.Text)

			const epsilon = 1e-9
			if diff := got - want; diff > epsilon || diff < -epsilon {
				t.Errorf("entry %q: measured WER = %v, want %v", entry.Name, got, want)
			}
		})
	}

	if tested != 41 {
		t.Fatalf("wired up %d corpus entries against the fake-ASR pipeline, want exactly 41 (identical_greeting, one_word_substitution, one_word_deletion, hinglish_identical_order_status, hinglish_one_word_substitution, hinglish_one_word_deletion, hinglish_midsentence_switch_payment_status, hinglish_loanword_recharge_request, hinglish_numbers_bill_amount_and_date, hinglish_order_number_spoken_in_english_digits, hinglish_filler_words_address_update, hinglish_otp_request_insertion, hinglish_call_disconnect_network_issue, hinglish_account_block_query_two_substitutions, hinglish_callback_request_deletion_and_filler, hinglish_two_word_deletion_travel_booking_confirmation, hinglish_proper_noun_brand_substitution_recharge, hinglish_proper_noun_person_name_substitution_order, hinglish_number_word_vs_digit_substitution, hinglish_long_utterance_single_deletion_callback, hinglish_content_word_deletion_parcel_delivery_date, hinglish_insertion_hallucinated_filler_word, english_dominant_embedded_hindi_courtesy_agent_transfer, hinglish_digit_sequence_deletion_account_number, hinglish_long_utterance_two_substitutions_refund_status, hinglish_negation_deletion_service_unavailable, hinglish_acronym_kyc_homophone_substitution, hinglish_two_insertions_confirmation_repeat, hinglish_acronym_ivr_homophone_substitution, hinglish_long_utterance_two_deletions_kyc_document_submission, hinglish_acronym_emi_homophone_substitution, hinglish_digit_duplication_insertion_registered_mobile_number, hinglish_insertion_trailing_word_repeat_call_end, hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation, hinglish_long_utterance_two_insertions_delivery_confirmation, hinglish_insertion_leading_word_repeat_call_open, hinglish_word_splitting_helpline_compound, hinglish_word_merging_update_profile_request, hinglish_adjacent_word_transposition_balance_check, hinglish_case_sensitivity_capitalized_sir_mismatch, hinglish_severe_hallucination_wer_exceeds_one_listen_request) - update wantWER alongside pkg/qa.FixedCorpus if entries changed", tested)
	}
}
