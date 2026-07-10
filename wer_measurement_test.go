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
	if len(entries) < 3 {
		t.Fatalf("qa.FixedCorpus() returned %d entries, want at least 3", len(entries))
	}

	// Precomputed expected WER for the first 3 entries, matching
	// pkg/qa/corpus.go's FixedCorpus doc comment. Measured against this
	// test's own fake-ASR-backed transcript below (not directly against
	// the corpus's Hypothesis string) so a regression in the Sarvam
	// client's transcript parsing (e.g. truncating or mangling the text)
	// would show up as a WER mismatch here even though the corpus data
	// itself is untouched.
	wantWER := map[string]float64{
		"identical_greeting":    0.0,
		"one_word_substitution": 1.0 / 5.0,
		"one_word_deletion":     1.0 / 7.0,
	}

	tested := 0
	for _, entry := range entries {
		want, ok := wantWER[entry.Name]
		if !ok {
			continue // only wiring the first 3 known entries, per this test's doc comment
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

	if tested != 3 {
		t.Fatalf("wired up %d corpus entries against the fake-ASR pipeline, want exactly 3 (identical_greeting, one_word_substitution, one_word_deletion) - update wantWER alongside pkg/qa.FixedCorpus if entries changed", tested)
	}
}
