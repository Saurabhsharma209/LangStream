// cost_tracking_integration_test.go is QA's cross-package integration test
// for the 2026-07-15 PE+SRE cost-tracking wiring: PE added
// asr.WithMetrics/asr.WithSarvamMetrics (pkg/asr/deepgram.go,
// pkg/asr/sarvam.go) so a real vendor client attributes a per-audio-frame
// cost estimate to observability.LatencyRecorder.RecordCost, and SRE is
// making sure that data actually surfaces through
// observability.BuildDashboardData/NewDashboardHandler. This file drives
// the real asr.DeepgramRecognizer and asr.SarvamRecognizer clients against
// small fake local WebSocket servers (same convention as
// integration_vendor_test.go's newFakeSarvamASRServer -- no network call
// here ever leaves the test process) and checks the seam PE and SRE each
// only partially see from inside their own package: that cost ends up
// attributed to the *right* vendor string, isn't double-counted, scales
// with audio duration rather than being a fixed per-call constant, and is
// visible end to end through observability.BuildDashboardData.
//
// GROUNDWORK, NOT A LIVE MEASUREMENT - same caveat as
// integration_vendor_test.go's package doc comment: these are fake
// servers, and deepgramCostPerMinuteUSD/sarvamCostPerMinuteUSD (private
// to pkg/asr) are themselves documented ASSUMPTIONS about vendor pricing,
// not verified billing-grade rates. This test never asserts an exact
// dollar figure against those private constants (which would make the
// test as fragile as the pricing assumption itself, without adding any
// safety) - instead it asserts the properties that matter regardless of
// what the per-minute rate is: correct attribution, no double-counting,
// and duration-proportional scaling.
package langstream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/observability"
)

// newFakeDeepgramCostServer starts a local WebSocket server standing in
// for Deepgram's streaming endpoint that this test only needs to accept a
// connection and silently drain whatever the client sends -- unlike
// integration_vendor_test.go's ASR fake server, this test never needs a
// transcript back: pkg/asr/deepgram.go's recordAudioCost fires
// synchronously inside PushAudio itself, before any response is read.
func newFakeDeepgramCostServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"
	return srv, wsURL
}

// newFakeSarvamCostServer is newFakeDeepgramCostServer's Sarvam
// equivalent, at the path asr.WithSarvamBaseURL/sarvam.go's client
// expects (mirroring integration_vendor_test.go's
// newFakeSarvamASRServer, minus ever needing to script a transcript
// reply).
func newFakeSarvamCostServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/speech-to-text/ws"
	return srv, wsURL
}

// frame20ms and frame40ms are fixed-shape 16-bit mono PCM frames at
// 8kHz: 320 bytes = 160 samples = 20ms, and double that for 40ms. Their
// exact contents are irrelevant (see this repo's placeholderPCM
// convention, e.g. pkg/qa/corpus.go) -- only their length (and therefore
// implied duration, given SampleRate) matters for the cost-per-duration
// checks below.
func frame20ms() asr.AudioFrame { return asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000} }
func frame40ms() asr.AudioFrame { return asr.AudioFrame{PCM: make([]byte, 640), SampleRate: 8000} }

// TestCostTracking_DeepgramAndSarvam_AttributedCorrectly is this test
// file's main check: drive real asr.DeepgramRecognizer and
// asr.SarvamRecognizer clients, sharing one *observability.LatencyRecorder,
// a different (and easy to tell apart) number of times each, and verify:
//
//   - each vendor's RecordCost event count exactly matches the number of
//     PushAudio calls made against it (no double-counting: e.g. a bug
//     that recorded cost both inside PushAudio and again from some other
//     hook on the same audio would double this count without changing
//     which vendor it's attributed to);
//   - each vendor's total cost is strictly positive after real audio was
//     pushed (cost tracking is actually wired, not a silent no-op);
//   - "deepgram" and "sarvam" never leak into each other's totals despite
//     sharing the same recorder (wrong-vendor-string bugs, e.g.
//     copy-pasting Deepgram's recordAudioCost into sarvam.go without
//     updating the literal vendor string, would show up here as cost
//     appearing under the wrong key or a mismatched event count); and
//   - the same data is visible through
//     observability.BuildDashboardData/CostSnapshot, the seam SRE's
//     dashboard work depends on, not just LatencyRecorder's own
//     CostTotal/CostEventCount accessors.
func TestCostTracking_DeepgramAndSarvam_AttributedCorrectly(t *testing.T) {
	dgSrv, dgURL := newFakeDeepgramCostServer(t)
	defer dgSrv.Close()
	svSrv, svURL := newFakeSarvamCostServer(t)
	defer svSrv.Close()

	t.Setenv("DEEPGRAM_API_KEY", "fake-test-key")
	t.Setenv("SARVAM_API_KEY", "fake-test-key")

	rec := observability.NewLatencyRecorder()

	dg, err := asr.NewDeepgramRecognizer(asr.WithBaseURL(dgURL), asr.WithMetrics(rec))
	if err != nil {
		t.Fatalf("NewDeepgramRecognizer: %v", err)
	}
	sv, err := asr.NewSarvamRecognizer(asr.WithSarvamBaseURL(svURL), asr.WithSarvamMetrics(rec))
	if err != nil {
		t.Fatalf("NewSarvamRecognizer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dgStream, err := dg.StartStream(ctx, asr.Language("en"))
	if err != nil {
		t.Fatalf("Deepgram StartStream: %v", err)
	}
	defer dgStream.Close()
	svStream, err := sv.StartStream(ctx, asr.Language("hi"))
	if err != nil {
		t.Fatalf("Sarvam StartStream: %v", err)
	}
	defer svStream.Close()

	const dgPushes = 4
	const svPushes = 7 // deliberately different from dgPushes

	for i := 0; i < dgPushes; i++ {
		if err := dgStream.PushAudio(ctx, frame20ms()); err != nil {
			t.Fatalf("Deepgram PushAudio[%d]: %v", i, err)
		}
	}
	for i := 0; i < svPushes; i++ {
		if err := svStream.PushAudio(ctx, frame20ms()); err != nil {
			t.Fatalf("Sarvam PushAudio[%d]: %v", i, err)
		}
	}

	if got := rec.CostEventCount("deepgram"); got != dgPushes {
		t.Errorf("CostEventCount(\"deepgram\") = %d, want %d (one RecordCost per PushAudio call -- a mismatch indicates missing or double cost recording)", got, dgPushes)
	}
	if got := rec.CostEventCount("sarvam"); got != svPushes {
		t.Errorf("CostEventCount(\"sarvam\") = %d, want %d (one RecordCost per PushAudio call -- a mismatch indicates missing or double cost recording)", got, svPushes)
	}

	dgTotal := rec.CostTotal("deepgram")
	svTotal := rec.CostTotal("sarvam")
	if dgTotal <= 0 {
		t.Errorf("CostTotal(\"deepgram\") = %v, want > 0 after %d real audio frames pushed through the real Deepgram client", dgTotal, dgPushes)
	}
	if svTotal <= 0 {
		t.Errorf("CostTotal(\"sarvam\") = %v, want > 0 after %d real audio frames pushed through the real Sarvam client", svTotal, svPushes)
	}

	// Cross-attribution check: nothing recorded for either vendor should
	// have leaked in under an unrelated/misspelled vendor string.
	snapshot := rec.CostSnapshot()
	seenVendors := make(map[string]observability.CostStats, len(snapshot))
	for _, cs := range snapshot {
		seenVendors[cs.Vendor] = cs
	}
	if len(snapshot) != 2 {
		t.Fatalf("CostSnapshot() returned %d vendor entries (%+v), want exactly 2 (deepgram, sarvam) -- an extra/misspelled vendor key would indicate a wrong-vendor-string bug", len(snapshot), snapshot)
	}
	dgStats, ok := seenVendors["deepgram"]
	if !ok {
		t.Fatalf("CostSnapshot() has no \"deepgram\" entry: %+v", snapshot)
	}
	svStats, ok := seenVendors["sarvam"]
	if !ok {
		t.Fatalf("CostSnapshot() has no \"sarvam\" entry: %+v", snapshot)
	}
	if dgStats.Events != dgPushes {
		t.Errorf("CostSnapshot()'s deepgram entry Events = %d, want %d", dgStats.Events, dgPushes)
	}
	if svStats.Events != svPushes {
		t.Errorf("CostSnapshot()'s sarvam entry Events = %d, want %d", svStats.Events, svPushes)
	}
	if dgStats.TotalUSD != dgTotal {
		t.Errorf("CostSnapshot()'s deepgram TotalUSD = %v, want %v (matching CostTotal(\"deepgram\"))", dgStats.TotalUSD, dgTotal)
	}
	if svStats.TotalUSD != svTotal {
		t.Errorf("CostSnapshot()'s sarvam TotalUSD = %v, want %v (matching CostTotal(\"sarvam\"))", svStats.TotalUSD, svTotal)
	}

	// The same numbers must be visible through the actual seam SRE's
	// dashboard work depends on (observability.BuildDashboardData), not
	// just LatencyRecorder's own accessors -- if BuildDashboardData ever
	// filtered/renamed/dropped cost entries, this would catch it even
	// though rec.CostTotal above wouldn't.
	dash := observability.BuildDashboardData(rec)
	if len(dash.Costs) != 2 {
		t.Fatalf("BuildDashboardData(rec).Costs has %d entries, want 2: %+v", len(dash.Costs), dash.Costs)
	}
	var dashDG, dashSV *observability.CostStats
	for i := range dash.Costs {
		switch dash.Costs[i].Vendor {
		case "deepgram":
			dashDG = &dash.Costs[i]
		case "sarvam":
			dashSV = &dash.Costs[i]
		}
	}
	if dashDG == nil {
		t.Fatalf("BuildDashboardData(rec).Costs is missing a \"deepgram\" entry: %+v", dash.Costs)
	}
	if dashSV == nil {
		t.Fatalf("BuildDashboardData(rec).Costs is missing a \"sarvam\" entry: %+v", dash.Costs)
	}
	if dashDG.TotalUSD != dgTotal || dashDG.Events != dgPushes {
		t.Errorf("BuildDashboardData(rec)'s deepgram entry = %+v, want TotalUSD=%v Events=%d", *dashDG, dgTotal, dgPushes)
	}
	if dashSV.TotalUSD != svTotal || dashSV.Events != svPushes {
		t.Errorf("BuildDashboardData(rec)'s sarvam entry = %+v, want TotalUSD=%v Events=%d", *dashSV, svTotal, svPushes)
	}
}

// TestCostTracking_ScalesWithAudioDurationNotFlatPerCall guards against a
// different, easy-to-get-wrong shape of bug than
// TestCostTracking_DeepgramAndSarvam_AttributedCorrectly above: a cost
// formula that is secretly a flat per-PushAudio-call constant (e.g.
// hardcoding "one frame costs $X" instead of deriving cost from the
// frame's actual PCM length/SampleRate), or one that uses the wrong unit
// entirely (e.g. per-byte instead of per-second of audio, which would
// scale by 1x not 2x when only the audio's *duration convention* doubles
// while its underlying sample count also legitimately doubles -- this
// specific test doubles PCM length at a fixed SampleRate, so both
// "per-byte" and "per-duration" formulas would agree here, but a flat
// per-call formula would not). Pushing one double-length (40ms) frame is
// asserted to cost approximately double what one 20ms frame costs, for
// each vendor independently.
func TestCostTracking_ScalesWithAudioDurationNotFlatPerCall(t *testing.T) {
	for _, vendor := range []string{"deepgram", "sarvam"} {
		vendor := vendor
		t.Run(vendor, func(t *testing.T) {
			rec := observability.NewLatencyRecorder()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var stream asr.StreamSession
			switch vendor {
			case "deepgram":
				srv, url := newFakeDeepgramCostServer(t)
				defer srv.Close()
				t.Setenv("DEEPGRAM_API_KEY", "fake-test-key")
				dg, err := asr.NewDeepgramRecognizer(asr.WithBaseURL(url), asr.WithMetrics(rec))
				if err != nil {
					t.Fatalf("NewDeepgramRecognizer: %v", err)
				}
				stream, err = dg.StartStream(ctx, asr.Language("en"))
				if err != nil {
					t.Fatalf("StartStream: %v", err)
				}
			case "sarvam":
				srv, url := newFakeSarvamCostServer(t)
				defer srv.Close()
				t.Setenv("SARVAM_API_KEY", "fake-test-key")
				sv, err := asr.NewSarvamRecognizer(asr.WithSarvamBaseURL(url), asr.WithSarvamMetrics(rec))
				if err != nil {
					t.Fatalf("NewSarvamRecognizer: %v", err)
				}
				stream, err = sv.StartStream(ctx, asr.Language("hi"))
				if err != nil {
					t.Fatalf("StartStream: %v", err)
				}
			}
			defer stream.Close()

			if err := stream.PushAudio(ctx, frame20ms()); err != nil {
				t.Fatalf("PushAudio(20ms): %v", err)
			}
			shortCost := rec.CostTotal(vendor)
			if shortCost <= 0 {
				t.Fatalf("cost after one 20ms frame = %v, want > 0", shortCost)
			}

			if err := stream.PushAudio(ctx, frame40ms()); err != nil {
				t.Fatalf("PushAudio(40ms): %v", err)
			}
			totalAfterLong := rec.CostTotal(vendor)
			longCost := totalAfterLong - shortCost

			ratio := longCost / shortCost
			const wantRatio = 2.0
			const relTolerance = 0.05 // 5% -- generous but still catches a flat-per-call (ratio ~1.0) or wildly-wrong-unit formula.
			if ratio < wantRatio*(1-relTolerance) || ratio > wantRatio*(1+relTolerance) {
				t.Errorf("%s: a 40ms frame cost %v after a 20ms frame cost %v (ratio %.4f), want ratio ~%.1f -- cost does not appear to scale with audio duration (possible flat-per-call or wrong-unit cost formula)", vendor, longCost, shortCost, ratio, wantRatio)
			}
		})
	}
}
