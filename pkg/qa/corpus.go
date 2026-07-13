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
	}
}
