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
// The six entries:
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
	}
}
