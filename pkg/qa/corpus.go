package qa

// CorpusEntry is one fixed test-call fixture: a ground-truth Reference
// transcript, the Hypothesis transcript a fake ASR backend is scripted to
// return in its place (standing in for what a real vendor might have
// imperfectly transcribed — see the package doc comment in wer.go for why
// this is groundwork against fakes, not a live measurement), and the PCM
// audio frame that triggers that fake response.
//
// The PCM frame itself is not meaningfully "the audio for" Reference or
// Hypothesis — the fake ASR servers this corpus is designed to be wired
// against (see integration_vendor_test.go's newFakeSarvamASRServer and the
// repo-root wer_measurement_test.go) don't perform real speech recognition;
// they reply with whatever transcript text they were configured with
// regardless of the audio bytes received. PCM here is a placeholder frame
// of the right shape (16-bit mono PCM) to drive that real client code down
// its normal PushAudio path, exactly like the fixed synthetic frames used
// throughout integration_vendor_test.go and cmd/langstream's demo/serve
// paths.
type CorpusEntry struct {
	// Name uniquely identifies this entry (used in test names/output).
	Name string

	// Language is the language hint passed to Recognizer.StartStream,
	// e.g. "en".
	Language string

	// Reference is the ground-truth transcript.
	Reference string

	// Hypothesis is what the fake ASR backend is scripted to return —
	// identical to Reference for a "perfect" entry, or a deliberately
	// perturbed variant (one substitution/deletion/insertion) to give a
	// non-trivial, precomputed expected WER.
	Hypothesis string

	// PCM is the placeholder audio frame pushed to trigger the fake ASR
	// response. See the type doc comment: its contents are not analyzed.
	PCM []byte

	// SampleRate is the sample rate stamped onto the PCM frame above.
	SampleRate int
}

// placeholderPCM returns a fixed placeholder audio frame: 20ms of 16-bit
// mono silence-shaped PCM at 8kHz (320 bytes), matching the convention
// used elsewhere in this repo (see cmd/langstream/main.go's runDemo and
// examples/vsip_example's fakeAudioSource) for a single telephony-sized
// frame.
func placeholderPCM() []byte {
	return make([]byte, 320)
}

// FixedCorpus returns a small, fixed set of reference/hypothesis
// transcript pairs for wiring WordErrorRate up against a fake-ASR-backed
// pipeline (see wer_measurement_test.go at the repo root). The set
// deliberately includes one identical (WER 0.0) entry and two entries with
// a single, precisely known word-level error each, so the expected WER for
// every entry can be (and is, in corpus_test.go) computed by hand and
// asserted exactly:
//
//   - identical_greeting:      WER 0.0    (0 errors / 6 words)
//   - one_word_substitution:   WER 0.2    (1 substitution / 5 words)
//   - one_word_deletion:       WER 1/7    (1 deletion / 7 words)
//
// Sprint 2026-07-12 (QA) adds three Hindi/English code-switching
// ("Hinglish") entries, per DEVLOG.md's 2026-07-10 entry flagging this as
// the next-sprint QA priority now that the WER harness exists. These use
// Language "hi" (the same code-switching-capable hint
// integration_vendor_test.go uses with the real Sarvam client/fake
// server — Sarvam's whole purpose, per that file's package doc comment,
// is Hindi-English code-switching), and mix Devanagari-script Hindi words
// with untransliterated English words within the same sentence, which is
// the realistic shape of Hinglish speech (not a fully Romanized
// transliteration of Hindi, and not pure English or pure Hindi):
//
//   - hinglish_identical_order_status: WER 0.0  (0 errors / 6 words)
//   - hinglish_one_word_substitution:  WER 1/6  (1 substitution / 6 words)
//   - hinglish_one_word_deletion:      WER 1/7  (1 deletion / 7 words)
//
// Sprint 2026-07-13 (QA) substantially widens the Hinglish coverage beyond
// those three single-pattern cases, adding nine more entries modeled on
// realistic Indian contact-center call patterns that the original three
// didn't exercise: a mid-sentence Hindi->English->Hindi switch, an English
// loanword embedded in Hindi grammar, numbers and a date spoken in one
// language inside an otherwise different-language sentence, digits spoken
// as English words, conversational filler words ("matlab", "actually",
// "na"), an ASR-style stutter/repeat insertion (the corpus previously had
// no insertion case at all), a fully clean multi-switch technical sentence
// (network/call-drop jargon), a two-substitution case, and a trailing
// filler-word deletion. Each still carries exactly one hand-verifiable
// error shape (or zero), so its expected WER remains something
// corpus_test.go can assert exactly:
//
//   - hinglish_midsentence_switch_payment_status:        WER 1/12  (1 substitution / 12 words)
//   - hinglish_loanword_recharge_request:                WER 1/7   (1 deletion / 7 words)
//   - hinglish_numbers_bill_amount_and_date:              WER 1/12  (1 substitution / 12 words)
//   - hinglish_order_number_spoken_in_english_digits:     WER 1/9   (1 substitution / 9 words)
//   - hinglish_filler_words_address_update:               WER 1/9   (1 deletion / 9 words)
//   - hinglish_otp_request_insertion:                     WER 1/5   (1 insertion / 5 words)
//   - hinglish_call_disconnect_network_issue:             WER 0.0   (0 errors / 10 words)
//   - hinglish_account_block_query_two_substitutions:     WER 2/13  (2 substitutions / 13 words)
//   - hinglish_callback_request_deletion_and_filler:      WER 1/10  (1 deletion / 10 words)
//
// Sprint 2026-07-14 (QA) adds ten further entries per DEVLOG.md's
// 2026-07-13 follow-up flagging categories the corpus still didn't
// exercise well: a multi-word (not just single-word) deletion, proper
// noun/brand-name and person-name substitutions, a numbers-spoken-as-
// words-vs-digit mismatch (both a single-word substitution shape and a
// digit-sequence deletion shape), two long (18-25 word) utterances (the
// corpus previously skewed short, topping out around 13 words), a
// content-word (not filler-word) deletion, a hallucinated-word insertion
// (distinct from the existing stutter/repeat insertion case), and an
// English-dominant sentence with an embedded Hindi courtesy phrase (the
// reverse code-switch direction from every existing entry, which are all
// Hindi-dominant with embedded English):
//
//   - hinglish_two_word_deletion_travel_booking_confirmation:  WER 2/13  (2 deletions / 13 words)
//   - hinglish_proper_noun_brand_substitution_recharge:        WER 1/8   (1 substitution / 8 words)
//   - hinglish_proper_noun_person_name_substitution_order:     WER 1/11  (1 substitution / 11 words)
//   - hinglish_number_word_vs_digit_substitution:              WER 1/9   (1 substitution / 9 words)
//   - hinglish_long_utterance_single_deletion_callback:        WER 1/25  (1 deletion / 25 words)
//   - hinglish_content_word_deletion_parcel_delivery_date:     WER 1/12  (1 deletion / 12 words)
//   - hinglish_insertion_hallucinated_filler_word:             WER 1/7   (1 insertion / 7 words)
//   - english_dominant_embedded_hindi_courtesy_agent_transfer: WER 1/12  (1 deletion / 12 words)
//   - hinglish_digit_sequence_deletion_account_number:         WER 1/10  (1 deletion / 10 words)
//   - hinglish_long_utterance_two_substitutions_refund_status: WER 2/18  (2 substitutions / 18 words)
//
// Sprint 2026-07-15 (QA) adds five further entries covering categories the
// corpus still didn't exercise: a negation-word deletion (dropping "nahi"
// flips the entire sentence's meaning -- a small edit distance but a
// disproportionately high-impact error class not yet represented, since
// every prior deletion drops a content or filler word rather than a
// negation), two acronym/homophone substitutions ("KYC" misheard as the
// phonetically similar "casey", and "IVR" misheard as "aivar" -- acronyms
// are routine in Indian contact-center speech and are exactly the kind of
// short, low-context token an ASR model is prone to mishear as a normal
// word), a two-insertion case (this corpus's existing insertion entries
// each add exactly one spurious word; this one has two independent
// duplicated-word insertions in the same short sentence, checking WER
// alignment doesn't miscount when insertions occur at multiple points),
// and a second long-utterance entry exercising a multi-word *deletion*
// specifically at long-utterance length (the corpus's other 18-25 word
// entries only exercise substitutions or a single-word deletion, not a
// multi-word deletion at that length):
//
//   - hinglish_negation_deletion_service_unavailable:                WER 1/7   (1 deletion / 7 words)
//   - hinglish_acronym_kyc_homophone_substitution:                   WER 1/7   (1 substitution / 7 words)
//   - hinglish_two_insertions_confirmation_repeat:                   WER 2/5   (2 insertions / 5 words)
//   - hinglish_acronym_ivr_homophone_substitution:                   WER 1/7   (1 substitution / 7 words)
//   - hinglish_long_utterance_two_deletions_kyc_document_submission: WER 2/20  (2 deletions / 20 words)
//
// Sprint 2026-07-16 (QA) adds five further entries covering categories the
// corpus still didn't exercise: a third acronym/homophone substitution
// ("EMI" misheard as "emmy", alongside the existing KYC/IVR cases), a
// digit-duplication insertion (the insertion-side counterpart to the
// existing digit-sequence deletion entry -- this corpus had no digit
// insertion case), an insertion positioned at the very end of the
// utterance rather than the start/middle (every prior insertion entry
// duplicates an internal word), a long (24-word) utterance mixing two
// different error types in one entry (a substitution and a deletion
// together -- every existing long entry is homogeneous, only
// substitutions or only deletions), and a long (21-word) utterance with
// two insertions (the long-utterance counterpart to the existing short
// two-insertion entry, the same way the corpus already has long
// counterparts for two-substitution and two-deletion entries):
//
//   - hinglish_acronym_emi_homophone_substitution:                                 WER 1/8   (1 substitution / 8 words)
//   - hinglish_digit_duplication_insertion_registered_mobile_number:               WER 1/10  (1 insertion / 10 words)
//   - hinglish_insertion_trailing_word_repeat_call_end:                            WER 1/5   (1 insertion / 5 words)
//   - hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation: WER 2/24  (1 substitution + 1 deletion / 24 words)
//   - hinglish_long_utterance_two_insertions_delivery_confirmation:                WER 2/21  (2 insertions / 21 words)
//
// Sprint 2026-07-17 (QA) adds six further entries covering error shapes
// the corpus still didn't exercise, found while auditing existing
// coverage against wer.go's own documented behaviors and edge cases
// (see wer.go's package/function doc comments):
//
//   - an isolated leading-position insertion (every existing insertion
//     entry duplicates an internal or trailing word;
//     hinglish_insertion_trailing_word_repeat_call_end covers the
//     trailing edge, but nothing isolates a duplicate at the very start
//     of the utterance on its own -- hinglish_two_insertions_confirmation_repeat
//     has a leading duplicate too, but paired with a second, unrelated
//     mid-sentence insertion, not isolated);
//
//   - a word-splitting case, where the fake ASR splits a single
//     reference word into two hypothesis words ("helpline" ->
//     "help line") -- the corpus had substitutions, deletions, and
//     stutter/hallucination insertions, but no compound-word-splitting
//     shape, which costs one substitution plus one insertion under
//     WordErrorRate's edit-distance alignment;
//
//   - a word-merging case, the reverse of the above: two adjacent
//     reference words merged into one hypothesis word ("up date" ->
//     "update"), costing one deletion plus one substitution;
//
//   - an adjacent-word transposition/swap, where the fake ASR reports
//     two adjacent words in reverse order -- WordErrorRate has no "swap"
//     edit operation, so this costs two substitutions under the
//     standard Levenshtein alignment, a shape distinct from this
//     corpus's existing (non-adjacent) two-substitution entries;
//
//   - a case-sensitivity mismatch, directly exercising the caveat
//     documented on WordErrorRate itself ("no punctuation stripping or
//     case-folding is performed, so 'Hello,' and 'hello' are different
//     tokens"): a sentence-initial capitalized word transcribed in
//     lowercase counts as a full substitution even though it is the same
//     word semantically -- no existing entry demonstrates this
//     documented limitation concretely;
//
//   - a severe-hallucination case demonstrating WordErrorRate's
//     documented WER > 1.0 behavior ("WordErrorRate can exceed 1.0 when
//     hypothesis has many more words than reference") -- every existing
//     corpus entry has WER <= 1.0 (the highest so far is 2/5 = 0.4); this
//     entry's fake ASR hallucinates enough extra, distinct words around
//     a short real utterance that the edit distance (5) exceeds the
//     reference word count (3), giving WER = 5/3.
//
//   - hinglish_insertion_leading_word_repeat_call_open:              WER 1/6  (1 insertion / 6 words)
//
//   - hinglish_word_splitting_helpline_compound:                     WER 2/6  (1 substitution + 1 insertion / 6 words)
//
//   - hinglish_word_merging_update_profile_request:                  WER 2/7  (1 deletion + 1 substitution / 7 words)
//
//   - hinglish_adjacent_word_transposition_balance_check:            WER 2/6  (2 substitutions / 6 words)
//
//   - hinglish_case_sensitivity_capitalized_sir_mismatch:            WER 1/6  (1 substitution / 6 words)
//
//   - hinglish_severe_hallucination_wer_exceeds_one_listen_request:  WER 5/3  (5 insertions / 3 words, WER > 1.0)
func FixedCorpus() []CorpusEntry {
	return []CorpusEntry{
		{
			Name:       "identical_greeting",
			Language:   "en",
			Reference:  "hello this is a test call",
			Hypothesis: "hello this is a test call",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			Name:       "one_word_substitution",
			Language:   "en",
			Reference:  "please confirm your account number",
			Hypothesis: "please confirm your account limit",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			Name:       "one_word_deletion",
			Language:   "en",
			Reference:  "i would like to cancel my subscription",
			Hypothesis: "i would like cancel my subscription",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "I want to track my order" — a typical Hinglish
			// construction where the English noun phrase "order track"
			// is embedded directly in an otherwise-Hindi sentence,
			// rather than either fully Romanized Hindi or pure English.
			Name:       "hinglish_identical_order_status",
			Language:   "hi",
			Reference:  "मुझे अपना order track करना है",
			Hypothesis: "मुझे अपना order track करना है",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Same sentence as above, but the fake ASR mishears the
			// English verb "track" as "cancel" — a single substitution
			// in the middle of a code-switched sentence, exercising WER
			// alignment across a script boundary (the surrounding
			// Devanagari words must still align correctly on either
			// side of the substituted English word).
			Name:       "hinglish_one_word_substitution",
			Language:   "hi",
			Reference:  "मुझे अपना order track करना है",
			Hypothesis: "मुझे अपना order cancel करना है",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "Please check my account balance" with the English word
			// "check" embedded mid-sentence; the fake ASR drops it
			// entirely — a single deletion, again straddling a
			// Devanagari/English script boundary.
			Name:       "hinglish_one_word_deletion",
			Language:   "hi",
			Reference:  "please मेरा account balance check कर दीजिए",
			Hypothesis: "please मेरा account balance कर दीजिए",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-13 (QA) additions below: harder, more varied
		// Hinglish patterns than the three above, per DEVLOG's follow-up
		// asking for mid-sentence switches, embedded loanwords,
		// numbers/dates, filler words, and other realistic
		// contact-center call shapes. All use romanized ("Hinglish
		// proper") transliteration for the Hindi portion rather than
		// Devanagari, which is the other common real-world shape ASR
		// vendors and agents actually see/type (the three entries above
		// already cover the Devanagari-mixed shape) — between the two
		// groups this corpus now covers both common script conventions.

		{
			// A mid-sentence switch that itself switches back
			// (Hindi -> English -> Hindi -> English), the shape of a
			// typical agent-to-customer status update, not just a
			// single embedded English phrase. The fake ASR mishears
			// the second English verb "pending" as "processing" — a
			// substitution in the second, not first, code-switched
			// span, checking alignment doesn't just get lucky on the
			// first switch point.
			Name:       "hinglish_midsentence_switch_payment_status",
			Language:   "hi",
			Reference:  "sir aapka payment successful ho gaya hai lekin refund abhi pending hai",
			Hypothesis: "sir aapka payment successful ho gaya hai lekin refund abhi processing hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "internet connection" is an English loanword pair used
			// directly inside Hindi sentence grammar (not translated to
			// Hindi equivalents) — extremely common in telecom/ISP
			// support calls. The fake ASR drops "connection" entirely,
			// a deletion of one of the two loanwords while leaving the
			// other and the surrounding Hindi grammar intact.
			Name:       "hinglish_loanword_recharge_request",
			Language:   "hi",
			Reference:  "mujhe apna internet connection recharge karwana hai",
			Hypothesis: "mujhe apna internet recharge karwana hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Numbers ("five hundred rupees") spoken in English and a
			// date reference ("pandrah tareekh" = "the 15th") spoken in
			// Hindi, within the same billing sentence — a very common
			// real contact-center pattern where amounts are read out in
			// English but calendar dates stay in Hindi. The fake ASR
			// mishears the Hindi number word "pandrah" (fifteen) as
			// "solah" (sixteen), a single substitution inside the
			// Hindi-language date span, not the English amount span.
			Name:       "hinglish_numbers_bill_amount_and_date",
			Language:   "hi",
			Reference:  "aapka bill five hundred rupees hai jo pandrah tareekh ko due hai",
			Hypothesis: "aapka bill five hundred rupees hai jo solah tareekh ko due hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// An order number read out digit-by-digit in English words
			// embedded in an otherwise Hindi sentence — routine for
			// reading back reference/order/OTP numbers on Indian
			// support calls. The fake ASR mishears one digit word
			// ("four") as its homophone-ish "for", a single
			// substitution among the digit span.
			Name:       "hinglish_order_number_spoken_in_english_digits",
			Language:   "hi",
			Reference:  "mera order number one two three four five hai",
			Hypothesis: "mera order number one two three for five hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Conversational filler words ("matlab", "actually", "na")
			// are extremely common in real Hinglish call audio and are
			// exactly the kind of low-information word an ASR model is
			// most likely to drop or mis-transcribe. The fake ASR here
			// drops the leading Hindi filler "matlab" ("I mean") in its
			// entirety — a deletion of a filler word specifically, not
			// of a content word like the other deletion cases in this
			// corpus.
			Name:       "hinglish_filler_words_address_update",
			Language:   "hi",
			Reference:  "matlab actually mujhe apna address update karna hai na",
			Hypothesis: "actually mujhe apna address update karna hai na",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// This corpus previously had no *insertion* case at all
			// (only substitutions and deletions) — a real ASR failure
			// mode is a stutter/repeat artifact that inserts a spurious
			// duplicate word, which this entry models: the fake ASR
			// repeats "OTP" back to back where the reference has it
			// once. OTP requests are one of the single most common
			// Indian contact-center/IVR utterances, and code-switch
			// trivially (the acronym itself is never translated).
			Name:       "hinglish_otp_request_insertion",
			Language:   "hi",
			Reference:  "sir please OTP bhej dijiye",
			Hypothesis: "sir please OTP OTP bhej dijiye",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A fully clean (WER 0.0) multi-switch technical-jargon
			// sentence — "network issue" and "call cut" are English
			// telecom terms used with Hindi grammar around them, with
			// no Devanagari script at all (pure Romanized Hinglish).
			// Included specifically as a should-transcribe-cleanly
			// baseline for this harder, more technical vocabulary
			// register, the same role identical_greeting and
			// hinglish_identical_order_status play for their own
			// registers.
			Name:       "hinglish_call_disconnect_network_issue",
			Language:   "hi",
			Reference:  "network issue ki wajah se call cut ho gaya tha",
			Hypothesis: "network issue ki wajah se call cut ho gaya tha",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Harder than this corpus's other cases: *two*
			// substitutions in one sentence, both confusions between
			// near-synonym English loanwords ("block"/"lock" and
			// "unblock"/"unlock") that are easy for an ASR model (or a
			// human) to mishear, exercising WER's alignment when
			// multiple, non-adjacent errors occur in the same
			// code-switched sentence rather than just one.
			Name:       "hinglish_account_block_query_two_substitutions",
			Language:   "hi",
			Reference:  "sir mera account block ho gaya hai kya aap unblock kar sakte hain",
			Hypothesis: "sir mera account lock ho gaya hai kya aap unlock kar sakte hain",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A callback request ending in a trailing Hindi/English
			// mixed politeness marker ("dijiyega please" — a Hindi verb
			// form immediately followed by an English courtesy word),
			// which the fake ASR drops as a trailing deletion — a
			// realistic failure mode since trailing words at the very
			// end of an utterance (after the speaker trails off) are
			// disproportionately likely to get clipped by real ASR
			// systems.
			Name:       "hinglish_callback_request_deletion_and_filler",
			Language:   "hi",
			Reference:  "aap mujhe thodi der mein call back kar dijiyega please",
			Hypothesis: "aap mujhe thodi der mein call back kar dijiyega",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-14 (QA) additions below: ten more entries per
		// DEVLOG.md's 2026-07-13 follow-up, covering categories the corpus
		// still didn't exercise (see FixedCorpus's doc comment above for
		// the full rationale for each).

		{
			// A travel/booking-confirmation sentence where the fake ASR
			// drops the contiguous two-word phrase "email par" ("via
			// email") entirely — a multi-word deletion, distinct from
			// every existing deletion case in this corpus, which each
			// drop exactly one word.
			Name:       "hinglish_two_word_deletion_travel_booking_confirmation",
			Language:   "hi",
			Reference:  "sir aapki flight booking confirm ho gayi hai ticket email par bhej diya",
			Hypothesis: "sir aapki flight booking confirm ho gayi hai ticket bhej diya",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A recharge request naming a telecom brand; the fake ASR
			// mishears the brand name "Jio" as the competing brand
			// "Airtel" — a proper-noun/brand-name substitution, a
			// realistic ASR confusion this corpus hadn't covered (prior
			// entries only substitute common nouns/verbs).
			Name:       "hinglish_proper_noun_brand_substitution_recharge",
			Language:   "hi",
			Reference:  "sir mera Jio number recharge nahi ho raha",
			Hypothesis: "sir mera Airtel number recharge nahi ho raha",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A customer stating their name before an order-status query;
			// the fake ASR mishears the person's given name "Rakesh" as
			// the phonetically similar "Rajesh" — a person-name
			// substitution, the other proper-noun category this corpus
			// hadn't covered.
			Name:       "hinglish_proper_noun_person_name_substitution_order",
			Language:   "hi",
			Reference:  "sir mera naam Rakesh Kumar hai order abhi tak nahi aaya",
			Hypothesis: "sir mera naam Rajesh Kumar hai order abhi tak nahi aaya",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A delivery-timeline sentence where the Hindi number word
			// "teen" (three) is spoken but the fake ASR transcribes it as
			// the digit "3" — a number-word-vs-digit mismatch, the same
			// underlying number but a different literal token, which
			// WordErrorRate (whitespace tokenization only, no number
			// normalization per wer.go's doc comment) correctly counts as
			// a substitution.
			Name:       "hinglish_number_word_vs_digit_substitution",
			Language:   "hi",
			Reference:  "mera order teen din mein deliver ho jayega sir",
			Hypothesis: "mera order 3 din mein deliver ho jayega sir",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (25-word) callback-request sentence — every entry in
			// this corpus so far topped out around 13 words. The fake ASR
			// drops one word from the doubled idiom "jaldi se jaldi" ("as
			// soon as possible", literally "soon from soon"), leaving
			// "se jaldi" — a single deletion in a long utterance, checking
			// WER alignment still isolates exactly one error rather than
			// drifting across the rest of the long sentence.
			Name:       "hinglish_long_utterance_single_deletion_callback",
			Language:   "hi",
			Reference:  "sir aap ki problem hum samajh gaye hain aur hum apni team ko inform kar denge taki wo aapko jaldi se jaldi callback kar sake",
			Hypothesis: "sir aap ki problem hum samajh gaye hain aur hum apni team ko inform kar denge taki wo aapko se jaldi callback kar sake",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A parcel-delivery-date sentence where the fake ASR drops
			// "shaam" ("evening") — a content word specifying time of day,
			// not a filler word like hinglish_filler_words_address_update's
			// deletion, so this exercises WER against a content-word loss
			// specifically.
			Name:       "hinglish_content_word_deletion_parcel_delivery_date",
			Language:   "hi",
			Reference:  "sir aapka parcel kal shaam tak aapke delivery address par pahunch jayega",
			Hypothesis: "sir aapka parcel kal tak aapke delivery address par pahunch jayega",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A balance-check request where the fake ASR hallucinates an
			// extra word, "ke", that was never spoken at all — distinct
			// from hinglish_otp_request_insertion's stutter/repeat
			// insertion (which duplicates an adjacent real word), this
			// models the other common ASR insertion failure mode: an
			// invented token with no corresponding audio.
			Name:       "hinglish_insertion_hallucinated_filler_word",
			Language:   "hi",
			Reference:  "sir mera balance check kar dijiye please",
			Hypothesis: "sir mera balance check kar ke dijiye please",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// An agent-transfer sentence that is English-dominant with a
			// single embedded Hindi courtesy word, "shukriya" ("thank
			// you") — the reverse code-switch direction from every other
			// entry in this corpus, which are all Hindi-dominant with
			// embedded English. The fake ASR drops "another", a single
			// deletion.
			Name:       "english_dominant_embedded_hindi_courtesy_agent_transfer",
			Language:   "hi",
			Reference:  "let me transfer your call to another agent one moment please shukriya",
			Hypothesis: "let me transfer your call to agent one moment please shukriya",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// An account-number readout, digit-by-digit in English words
			// inside a Hindi sentence, like
			// hinglish_order_number_spoken_in_english_digits, but here the
			// fake ASR drops one digit ("three") from the middle of the
			// sequence entirely rather than mishearing it — a digit-
			// sequence deletion, not a digit substitution.
			Name:       "hinglish_digit_sequence_deletion_account_number",
			Language:   "hi",
			Reference:  "sir mera account number one two three four five hai",
			Hypothesis: "sir mera account number one two four five hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (18-word) refund-status update with *two*
			// substitutions, like
			// hinglish_account_block_query_two_substitutions but longer:
			// the fake ASR mishears "process" as "complete" and
			// "transfer" as "credit" — two independent, non-adjacent
			// near-synonym confusions in one long sentence.
			Name:       "hinglish_long_utterance_two_substitutions_refund_status",
			Language:   "hi",
			Reference:  "sir aapka refund process ho chuka hai lekin bank ki taraf se paise abhi tak transfer nahi hue",
			Hypothesis: "sir aapka refund complete ho chuka hai lekin bank ki taraf se paise abhi tak credit nahi hue",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-15 (QA) additions below: five more entries
		// per the doc comment above (a negation deletion, two acronym/
		// homophone substitutions, a two-insertion case, and a second
		// long-utterance entry with a multi-word deletion).

		{
			// Dropping the negation word "nahi" ("not") flips this
			// sentence's meaning entirely (from "this service is not
			// available" to "this service is available") even though
			// it's a single-word deletion like several other entries --
			// included specifically because negation words are a
			// disproportionately high-impact ASR failure mode that this
			// corpus otherwise never isolates on its own.
			Name:       "hinglish_negation_deletion_service_unavailable",
			Language:   "hi",
			Reference:  "sir yeh service abhi available nahi hai",
			Hypothesis: "sir yeh service abhi available hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "KYC" (Know Your Customer, ubiquitous in Indian
			// banking/telecom support calls) misheard as the
			// phonetically similar "casey" -- an acronym/homophone
			// substitution, a realistic ASR confusion category this
			// corpus hadn't covered (prior substitutions mishear common
			// nouns/verbs, proper nouns, or number words, not
			// acronyms).
			Name:       "hinglish_acronym_kyc_homophone_substitution",
			Language:   "hi",
			Reference:  "sir aapka KYC update ho gaya hai",
			Hypothesis: "sir aapka casey update ho gaya hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Every existing insertion entry in this corpus
			// (hinglish_otp_request_insertion,
			// hinglish_insertion_hallucinated_filler_word) adds exactly
			// one spurious word. This entry has two independent
			// duplicated-word insertions in the same short sentence
			// ("sir" repeated at the start, "ho" repeated mid-sentence)
			// -- checking WER alignment correctly isolates two separate
			// insertion points rather than miscounting when errors
			// aren't adjacent to each other.
			Name:       "hinglish_two_insertions_confirmation_repeat",
			Language:   "hi",
			Reference:  "sir aapka refund ho jayega",
			Hypothesis: "sir sir aapka refund ho ho jayega",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "IVR" (Interactive Voice Response, another acronym
			// routine in Indian telecom/contact-center speech)
			// misheard as "aivar" -- a second acronym/homophone
			// substitution alongside
			// hinglish_acronym_kyc_homophone_substitution, the same way
			// this corpus already carries two two-substitution entries
			// at different lengths (hinglish_account_block_query_two_substitutions,
			// hinglish_long_utterance_two_substitutions_refund_status)
			// for the same error shape.
			Name:       "hinglish_acronym_ivr_homophone_substitution",
			Language:   "hi",
			Reference:  "call IVR ke through connect hua tha",
			Hypothesis: "call aivar ke through connect hua tha",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (20-word) KYC-document sentence where the fake ASR
			// drops the contiguous two-word phrase "ja kar" ("by
			// going") entirely -- this corpus's other long (18-25 word)
			// entries (hinglish_long_utterance_single_deletion_callback,
			// hinglish_long_utterance_two_substitutions_refund_status)
			// exercise a single-word deletion or two substitutions at
			// that length, but not a multi-word deletion at long-
			// utterance length specifically, which this entry fills in.
			Name:       "hinglish_long_utterance_two_deletions_kyc_document_submission",
			Language:   "hi",
			Reference:  "sir aapko apna KYC document branch mein ja kar submit karna hoga varna aapka account temporarily block ho sakta hai",
			Hypothesis: "sir aapko apna KYC document branch mein submit karna hoga varna aapka account temporarily block ho sakta hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "EMI" (Equated Monthly Installment, routine in Indian
			// banking/lending support calls) misheard as the
			// phonetically similar "emmy" -- a third acronym/homophone
			// substitution alongside
			// hinglish_acronym_kyc_homophone_substitution and
			// hinglish_acronym_ivr_homophone_substitution, the same
			// realistic ASR confusion category (short, low-context
			// acronyms) applied to a different acronym.
			Name:       "hinglish_acronym_emi_homophone_substitution",
			Language:   "hi",
			Reference:  "sir aapka EMI is month process ho jayega",
			Hypothesis: "sir aapka emmy is month process ho jayega",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A registered-mobile-number readout, digit-by-digit in
			// English words, where the fake ASR duplicates one digit
			// ("two") rather than dropping or mishearing it -- the
			// insertion-side counterpart to
			// hinglish_digit_sequence_deletion_account_number (which
			// drops a digit from a similar sequence); this corpus had no
			// digit-duplication insertion case before this entry.
			Name:       "hinglish_digit_duplication_insertion_registered_mobile_number",
			Language:   "hi",
			Reference:  "sir aapka registered mobile number one two three four hai",
			Hypothesis: "sir aapka registered mobile number one two two three four hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A short call-closing courtesy line where the fake ASR
			// duplicates the very last word, "dhanyavaad" ("thank you") --
			// every existing insertion entry in this corpus duplicates a
			// word in the middle or at the start of the sentence; this is
			// the first insertion positioned at the trailing edge of the
			// utterance, checking WER alignment handles a duplicated
			// final token as cleanly as an internal one.
			Name:       "hinglish_insertion_trailing_word_repeat_call_end",
			Language:   "hi",
			Reference:  "sir call ke liye dhanyavaad",
			Hypothesis: "sir call ke liye dhanyavaad dhanyavaad",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (24-word) complaint-escalation sentence combining
			// *two different* error types in one entry: the fake ASR
			// drops "abhi" entirely (a deletion) and separately mishears
			// "resolution" as "solution" (a substitution). Every existing
			// long (18-25 word) entry in this corpus is homogeneous --
			// only deletions, or only substitutions -- this is the first
			// long utterance mixing error types, checking WER alignment
			// correctly isolates two independent, non-adjacent edits of
			// different kinds rather than miscounting when they don't
			// share a shape.
			Name:       "hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation",
			Language:   "hi",
			Reference:  "sir maine already do baar complaint register karwaya hai lekin abhi tak koi resolution nahi mila hai isliye main ise escalate karna chahta hoon",
			Hypothesis: "sir maine already do baar complaint register karwaya hai lekin tak koi solution nahi mila hai isliye main ise escalate karna chahta hoon",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (21-word) delivery/payment confirmation with *two*
			// stutter/repeat insertions ("ho" duplicated early, "hai"
			// duplicated later) -- this corpus's existing two-insertion
			// entry (hinglish_two_insertions_confirmation_repeat) is only
			// five words; this is the long-utterance counterpart, the
			// same way hinglish_long_utterance_two_substitutions_refund_status
			// and hinglish_long_utterance_two_deletions_kyc_document_submission
			// are the long-utterance counterparts to their short
			// two-substitution/two-deletion siblings.
			Name:       "hinglish_long_utterance_two_insertions_delivery_confirmation",
			Language:   "hi",
			Reference:  "sir aapka order successfully deliver ho gaya hai aur payment bhi successfully complete ho chuka hai dhanyavaad aapka time ke liye",
			Hypothesis: "sir aapka order successfully deliver ho ho gaya hai aur payment bhi successfully complete ho chuka hai hai dhanyavaad aapka time ke liye",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-17 (QA) additions below: six more entries per
		// the doc comment above (an isolated leading-position insertion, a
		// word-splitting case, a word-merging case, an adjacent-word
		// transposition, a case-sensitivity mismatch, and a
		// severe-hallucination WER>1.0 case).

		{
			// A call-connection confirmation where the fake ASR
			// duplicates the very first word, "sir" -- the leading-edge
			// counterpart to hinglish_insertion_trailing_word_repeat_call_end
			// (which duplicates the last word). This corpus's other
			// leading-duplicate shape
			// (hinglish_two_insertions_confirmation_repeat) always pairs
			// it with a second, unrelated mid-sentence insertion; this
			// entry isolates a duplicated leading word as the sentence's
			// only error.
			Name:       "hinglish_insertion_leading_word_repeat_call_open",
			Language:   "hi",
			Reference:  "sir aapka call connect ho gaya",
			Hypothesis: "sir sir aapka call connect ho gaya",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A customer-care sentence where the fake ASR splits the
			// single compound word "helpline" into two separate words,
			// "help line" -- a word-splitting failure mode this corpus
			// hadn't covered (distinct from every existing substitution/
			// deletion/insertion entry, none of which change the total
			// word-to-token mapping this way). Under WordErrorRate's
			// edit-distance alignment this costs one substitution
			// ("helpline" -> "help") plus one insertion ("line").
			Name:       "hinglish_word_splitting_helpline_compound",
			Language:   "hi",
			Reference:  "customer care helpline number yeh hai",
			Hypothesis: "customer care help line number yeh hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// The reverse of the word-splitting case above: a profile-
			// update request where the fake ASR merges the two reference
			// words "up date" into the single hypothesis word "update" --
			// a word-merging failure mode, also not previously covered.
			// This costs one deletion (of one of the two source words)
			// plus one substitution (turning the other into "update").
			Name:       "hinglish_word_merging_update_profile_request",
			Language:   "hi",
			Reference:  "sir apna profile up date kar lijiye",
			Hypothesis: "sir apna profile update kar lijiye",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A balance-check request where the fake ASR reports two
			// adjacent words, "balance" and "check", in swapped order.
			// WordErrorRate's standard Levenshtein alignment has no
			// "transposition" operation, so a pure adjacent-word swap
			// costs two substitutions -- a distinct shape from this
			// corpus's existing two-substitution entries
			// (hinglish_account_block_query_two_substitutions,
			// hinglish_long_utterance_two_substitutions_refund_status),
			// which each mishear two different, non-adjacent word pairs
			// rather than reordering one adjacent pair.
			Name:       "hinglish_adjacent_word_transposition_balance_check",
			Language:   "hi",
			Reference:  "sir pehle mera balance check kijiye",
			Hypothesis: "sir pehle mera check balance kijiye",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Directly exercises the case-sensitivity caveat documented
			// on WordErrorRate itself: no case-folding is performed, so
			// the same word in different casing counts as a full
			// substitution. Here, the fake ASR transcribes the
			// sentence-initial "Sir" (capitalized, as a real transcript
			// might render the start of a sentence) as lowercase "sir" --
			// semantically identical, but WordErrorRate correctly (per
			// its own documented behavior) counts it as one substitution.
			// No existing corpus entry demonstrates this documented
			// limitation concretely.
			Name:       "hinglish_case_sensitivity_capitalized_sir_mismatch",
			Language:   "hi",
			Reference:  "Sir aapka order confirm ho gaya",
			Hypothesis: "sir aapka order confirm ho gaya",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Demonstrates WordErrorRate's documented WER > 1.0 behavior
			// ("WordErrorRate can exceed 1.0 when hypothesis has many
			// more words than reference") -- every existing corpus entry
			// has WER <= 1.0 (the highest so far is 2/5 = 0.4). Here a
			// short, real three-word request ("sir suniye please" -- "sir,
			// please listen") is surrounded by enough fake-ASR
			// hallucinated, distinct extra words (modeling a severely
			// noisy/cross-talking line) that the minimal edit distance
			// (5, all insertions since every reference word still
			// appears, in order, as a subsequence of the hypothesis)
			// exceeds the 3-word reference length, giving WER = 5/3.
			Name:       "hinglish_severe_hallucination_wer_exceeds_one_listen_request",
			Language:   "hi",
			Reference:  "sir suniye please",
			Hypothesis: "haan sir thoda suniye na please theek hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-20 (QA) additions below: six more entries
		// covering error shapes the corpus still didn't exercise, found
		// while auditing wer.go's documented caveats/behaviors
		// (punctuation stripping, not just case-folding) and this
		// corpus's own existing entries for untested combinations
		// (single-type-only edits, two-token-max error counts, and
		// single-shape-only errors):
		//
		//   - a punctuation-only mismatch, directly exercising wer.go's
		//     "no punctuation stripping" caveat (the corpus already
		//     exercises the neighboring "no case-folding" caveat via
		//     hinglish_case_sensitivity_capitalized_sir_mismatch, but
		//     nothing isolates punctuation on its own);
		//
		//   - a single entry mixing all *three* error types at once
		//     (substitution + deletion + insertion together) --
		//     hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation
		//     mixes two types (substitution + deletion), but no entry
		//     combines all three;
		//
		//   - a currency-symbol-vs-spelled-out-words mismatch ("₹500"
		//     as one token vs "500 rupees" as two), a token-count-
		//     changing shape distinct from
		//     hinglish_number_word_vs_digit_substitution (a 1:1 token
		//     substitution with no token-count change);
		//
		//   - a total-substitution-failure entry where every single
		//     reference word is replaced (WER = 1.0 via pure
		//     substitution, S=N) -- distinct from
		//     hinglish_severe_hallucination_wer_exceeds_one_listen_request's
		//     WER > 1.0 (driven by insertions around a still-present
		//     reference subsequence): here none of the reference words
		//     survive at all, only every existing entry's WER <= 1.0
		//     with S < N;
		//
		//   - a short, common-word homophone substitution ("to"
		//     misheard as "too") -- distinct from this corpus's existing
		//     acronym/homophone entries (KYC/IVR/EMI), which are all
		//     multi-letter acronyms, not ordinary short function words;
		//
		//   - three non-adjacent single-word deletions in one sentence
		//     -- every existing multi-deletion entry
		//     (hinglish_two_word_deletion_travel_booking_confirmation,
		//     hinglish_long_utterance_two_deletions_kyc_document_submission)
		//     tops out at two deletions.
		//
		//   - hinglish_punctuation_only_mismatch_confirm_query:            WER 1/5   (1 substitution / 5 words)
		//   - hinglish_three_error_types_mixed_appointment_reschedule:     WER 3/11  (1 substitution + 1 deletion + 1 insertion / 11 words)
		//   - hinglish_currency_symbol_vs_words_bill_amount:               WER 2/5   (1 substitution + 1 insertion / 5 words)
		//   - hinglish_total_substitution_failure_balance_request:         WER 5/5   (5 substitutions / 5 words, WER == 1.0)
		//   - hinglish_homophone_to_too_confirmation_query:                WER 1/6   (1 substitution / 6 words)
		//   - hinglish_three_nonadjacent_deletions_complaint_resolution:   WER 3/16  (3 deletions / 16 words)
		{
			// The fake ASR attaches a trailing "?" to the final word
			// ("hai" -> "hai?") that was never actually spoken as
			// punctuation (real speech carries no literal question
			// mark) -- directly exercising wer.go's documented "no
			// punctuation stripping" caveat in isolation, the sibling
			// caveat to the case-folding one
			// hinglish_case_sensitivity_capitalized_sir_mismatch already
			// covers.
			Name:       "hinglish_punctuation_only_mismatch_confirm_query",
			Language:   "hi",
			Reference:  "sir kya yeh sahi hai",
			Hypothesis: "sir kya yeh sahi hai?",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// An appointment-reschedule sentence combining all three
			// edit types in one entry: the fake ASR drops "kal"
			// ("tomorrow") entirely (a deletion), mishears the Hindi
			// number word "nau" (nine) as "das" (ten) (a substitution),
			// and duplicates "ho" (an insertion) -- every existing
			// multi-error entry in this corpus mixes at most two of the
			// three edit types; this is the first to combine all three.
			Name:       "hinglish_three_error_types_mixed_appointment_reschedule",
			Language:   "hi",
			Reference:  "sir aapka appointment kal subah nau baje reschedule ho gaya hai",
			Hypothesis: "sir aapka appointment subah das baje reschedule ho ho gaya hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A billing sentence where the reference uses the rupee
			// symbol directly against the digits ("₹500", one token)
			// and the fake ASR instead transcribes it as the spelled-out
			// two-token form "500 rupees" -- a token-count-changing
			// currency mismatch, distinct from
			// hinglish_number_word_vs_digit_substitution's 1:1
			// word-to-digit substitution (no token-count change there).
			// Costs one substitution ("₹500" -> "500") plus one
			// insertion ("rupees").
			Name:       "hinglish_currency_symbol_vs_words_bill_amount",
			Language:   "hi",
			Reference:  "sir aapka bill ₹500 hai",
			Hypothesis: "sir aapka bill 500 rupees hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// Every word in this short balance-check request is
			// mistranscribed as a completely unrelated word -- a total
			// substitution failure (S == N, WER == 1.0 exactly) modeling
			// a severely garbled or wrong-audio-routed line. Distinct
			// from
			// hinglish_severe_hallucination_wer_exceeds_one_listen_request's
			// WER > 1.0 (driven by extra hallucinated insertions around
			// an otherwise-intact reference subsequence): here none of
			// the original words survive at all, and the word count
			// matches exactly (S=5, D=0, I=0), a shape no existing entry
			// demonstrates.
			Name:       "hinglish_total_substitution_failure_balance_request",
			Language:   "hi",
			Reference:  "sir mera balance batao please",
			Hypothesis: "yeh voh network dikhao dhanyavaad",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// The English function word "to" misheard as its common
			// homophone "too" -- a short, ordinary-word homophone
			// confusion, distinct from this corpus's existing acronym/
			// homophone entries
			// (hinglish_acronym_kyc_homophone_substitution,
			// hinglish_acronym_ivr_homophone_substitution,
			// hinglish_acronym_emi_homophone_substitution), all of which
			// are multi-letter acronyms rather than everyday short
			// function words. "to"/"too" is one of the single most
			// common homophone pairs in English ASR output.
			Name:       "hinglish_homophone_to_too_confirmation_query",
			Language:   "hi",
			Reference:  "sir yeh sahi hai to bataiye",
			Hypothesis: "sir yeh sahi hai too bataiye",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// A long (16-word) complaint-resolution sentence where the
			// fake ASR drops three separate, non-adjacent words
			// ("aapka", "aur", "hi") entirely -- every existing
			// multi-deletion entry in this corpus
			// (hinglish_two_word_deletion_travel_booking_confirmation,
			// hinglish_long_utterance_two_deletions_kyc_document_submission)
			// tops out at two deletions; this is the first with three,
			// checking WER alignment still isolates exactly three
			// independent single-word deletions scattered across a long
			// sentence rather than miscounting or conflating them.
			Name:       "hinglish_three_nonadjacent_deletions_complaint_resolution",
			Language:   "hi",
			Reference:  "sir aapka complaint register ho gaya hai aur hum jald hi ise resolve kar denge dhanyavaad",
			Hypothesis: "sir complaint register ho gaya hai hum jald ise resolve kar denge dhanyavaad",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},

		// --- Sprint 2026-07-21 (QA) additions below: five more entries
		// covering error shapes still not represented anywhere in this
		// corpus, found while auditing every existing entry's
		// hand-verified error shape (per DEVLOG.md's sprint-by-sprint
		// history in this file's own doc comment) for gaps rather than
		// re-treading an already-covered mechanic under a new name:
		//
		//   - a total-deletion entry via a genuinely empty hypothesis
		//     (silence/ASR-timeout: the backend produced no transcript
		//     text at all) -- distinct from
		//     hinglish_total_substitution_failure_balance_request's
		//     WER == 1.0 (S == N, same word count, every word wrong) in
		//     that this is D == N with zero hypothesis words whatsoever,
		//     the shape wer.go's own doc comment calls out by name
		//     ("WordErrorRate(reference, \"\") for a non-empty reference
		//     returns 1.0"), which no existing entry demonstrates
		//     concretely. NOTE: unlike this corpus's other entries, this
		//     one is deliberately NOT wired into the repo-root
		//     wer_measurement_test.go's fake-ASR-backed pipeline test --
		//     see that file's own comment next to this entry's absence
		//     from its wantWER map for why (the real
		//     asr.SarvamRecognizer client silently drops empty-transcript
		//     messages, so it can never be observed end-to-end through
		//     that specific wiring); it is still exercised directly by
		//     corpus_test.go's precomputed-WER check, which calls
		//     WordErrorRate itself rather than going through an ASR
		//     client;
		//
		//   - a combined deletion-plus-insertion entry with NO
		//     substitution at all (one reference word dropped, one
		//     unrelated word inserted elsewhere, with enough matching
		//     words in between that the two edits can't collapse into a
		//     single substitution under minimum-edit alignment) --
		//     distinct from every existing multi-type entry
		//     (hinglish_long_utterance_substitution_and_deletion_mixed_complaint_escalation
		//     mixes substitution+deletion,
		//     hinglish_three_error_types_mixed_appointment_reschedule
		//     mixes all three), none of which isolates deletion+insertion
		//     on their own;
		//
		//   - a contiguous multi-word phrase-repeat insertion (the fake
		//     ASR duplicates a whole three-word leading phrase, not just
		//     one word) -- every existing insertion entry
		//     (hinglish_otp_request_insertion,
		//     hinglish_insertion_hallucinated_filler_word,
		//     hinglish_digit_duplication_insertion_registered_mobile_number,
		//     hinglish_insertion_trailing_word_repeat_call_end,
		//     hinglish_insertion_leading_word_repeat_call_open) inserts
		//     exactly one word, and
		//     hinglish_two_insertions_confirmation_repeat/
		//     hinglish_long_utterance_two_insertions_delivery_confirmation
		//     insert two words at independent, non-contiguous points --
		//     none inserts a contiguous multi-word block;
		//
		//   - a systematic repeated-word substitution, where the *same*
		//     reference word ("hai") is mis-heard the *same* way
		//     ("hain") at two separate, non-adjacent occurrences in one
		//     sentence -- modeling a consistent dialectal/verb-agreement
		//     ASR bias rather than two unrelated word errors. Distinct
		//     from hinglish_account_block_query_two_substitutions (two
		//     *different* word pairs, each substituted once): every
		//     existing multi-substitution entry in this corpus involves
		//     distinct words, none repeats the identical substitution
		//     pattern at multiple points;
		//
		//   - a trailing contiguous multi-word (three-word) deletion
		//     modeling a truncated/cut-off call (the connection or ASR's
		//     silence-detection timeout drops the last few words of an
		//     utterance entirely) -- distinct from
		//     hinglish_two_word_deletion_travel_booking_confirmation's
		//     contiguous two-word deletion (mid-sentence, not at the
		//     trailing edge, and only two words) and from
		//     hinglish_three_nonadjacent_deletions_complaint_resolution's
		//     three *non-adjacent* single-word deletions (scattered, not
		//     one contiguous block): this is the first entry with a
		//     three-word contiguous deletion specifically anchored at the
		//     very end of the sentence.
		//
		//   - hinglish_total_deletion_empty_hypothesis_silence_timeout:               WER 7/7   (7 deletions / 7 words, WER == 1.0, empty hypothesis)
		//   - hinglish_deletion_and_insertion_no_substitution_order_confirmation:     WER 2/8   (1 deletion + 1 insertion / 8 words)
		//   - hinglish_three_word_phrase_repeat_insertion_order_confirmation:         WER 3/7   (3 insertions / 7 words)
		//   - hinglish_systematic_repeated_word_substitution_hai_hain_verb_agreement: WER 2/11  (2 substitutions / 11 words)
		//   - hinglish_trailing_three_word_deletion_call_cutoff_complaint_update:     WER 3/14  (3 deletions / 14 words)
		{
			// The fake ASR backend produced no transcript text
			// whatsoever for this utterance (modeling a total silence/
			// ASR-timeout failure) -- wer.go's own doc comment documents
			// WordErrorRate(reference, "") returning 1.0 for a non-empty
			// reference, but no existing entry demonstrates that
			// concretely with a real (if trivial) reference sentence.
			// See this block's leading comment for why this entry is
			// intentionally excluded from wer_measurement_test.go's
			// fake-ASR-backed pipeline wiring.
			Name:       "hinglish_total_deletion_empty_hypothesis_silence_timeout",
			Language:   "hi",
			Reference:  "sir aapka refund process ho gaya hai",
			Hypothesis: "",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// The fake ASR drops "aapka" entirely (a deletion) and
			// separately inserts an unrelated word "turant"
			// ("immediately") after "confirm" (an insertion) -- with two
			// unrelated matching words ("order", "confirm") in between
			// the two edit sites, so minimum-edit alignment can't
			// collapse them into a single substitution: this costs
			// exactly one deletion plus one insertion, with zero
			// substitutions, a combination no existing multi-error entry
			// in this corpus isolates on its own.
			Name:       "hinglish_deletion_and_insertion_no_substitution_order_confirmation",
			Language:   "hi",
			Reference:  "sir aapka order confirm ho gaya hai dhanyavaad",
			Hypothesis: "sir order confirm turant ho gaya hai dhanyavaad",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// The fake ASR duplicates the entire leading three-word
			// phrase ("sir aapka order") at the very start of the
			// utterance, rather than repeating a single word the way
			// every existing insertion entry in this corpus does --
			// costing three contiguous insertions in one block, a shape
			// distinct from this corpus's existing single-word and
			// independent-two-word insertion entries alike.
			Name:       "hinglish_three_word_phrase_repeat_insertion_order_confirmation",
			Language:   "hi",
			Reference:  "sir aapka order confirm ho gaya hai",
			Hypothesis: "sir aapka order sir aapka order confirm ho gaya hai",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// "hai" is mis-heard as "hain" at both of its two separate,
			// non-adjacent occurrences in this sentence -- the identical
			// substitution pattern repeated, not two unrelated word
			// errors the way
			// hinglish_account_block_query_two_substitutions's two
			// substitutions are. Models a systematic ASR bias (e.g. a
			// singular/plural verb-agreement confusion) rather than two
			// independent mistranscriptions.
			Name:       "hinglish_systematic_repeated_word_substitution_hai_hain_verb_agreement",
			Language:   "hi",
			Reference:  "sir aapka order ready hai aur payment bhi ho gaya hai",
			Hypothesis: "sir aapka order ready hain aur payment bhi ho gaya hain",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
		{
			// The fake ASR drops the trailing three words ("update
			// denge dhanyavaad") entirely, modeling a call that cuts off
			// (or an ASR silence-detection timeout that truncates the
			// transcript) before the speaker finishes the sentence --
			// distinct from
			// hinglish_two_word_deletion_travel_booking_confirmation's
			// contiguous two-word deletion (mid-sentence, only two
			// words) and from
			// hinglish_three_nonadjacent_deletions_complaint_resolution's
			// three scattered, non-adjacent single-word deletions: this
			// is the first entry with a three-word contiguous deletion
			// block anchored specifically at the very end of the
			// utterance.
			Name:       "hinglish_trailing_three_word_deletion_call_cutoff_complaint_update",
			Language:   "hi",
			Reference:  "sir aapki complaint register ho gayi hai aur hum jald hi update denge dhanyavaad",
			Hypothesis: "sir aapki complaint register ho gayi hai aur hum jald hi",
			PCM:        placeholderPCM(),
			SampleRate: 8000,
		},
	}
}
