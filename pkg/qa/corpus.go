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
	}
}
