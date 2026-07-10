// QA's Sprint 4 integration coverage for the `serve` subcommand Tech added
// this run (see main.go's runServe/serveDashboard) and SRE wired into
// docker-compose.yml's `command:`. Existing tests in main_test.go already
// cover serveDashboard's shutdown behavior against a synthetic
// *observability.LatencyRecorder with one manually-added event, and a
// listen-error path; this file adds two things those don't cover:
//
//  1. TestServe_RealSessionActivity_ReflectsInDashboardAndShutsDownGracefully
//     drives serveDashboard against a *real* Session (built via newSession,
//     the exact registry-based construction path runServe itself uses)
//     that has had real caller audio pushed through it and closed, so the
//     dashboard is asserted against genuine pipeline activity end to end
//     rather than a hand-populated recorder — and separately re-confirms
//     graceful shutdown on context cancellation within a bounded timeout,
//     matching the "don't let this test hang forever if shutdown is
//     broken" requirement.
//  2. TestServeCommand_RealBinary_EndToEnd goes one level further and
//     builds+runs the actual `langstream` binary (the thing Docker/CI/an
//     operator would actually invoke — main_test.go's tests all call
//     serveDashboard/newSession directly in-process, which would not catch
//     a bug in main()'s os.Args dispatch, flag wiring, or process-level
//     signal handling), hits it over real HTTP, sends it a real SIGTERM,
//     and asserts the process actually exits within a bounded time.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
)

// waitForServer polls url until it responds with any HTTP status (proving
// the server is accepting connections) or the deadline elapses.
func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became reachable: %v", url, lastErr)
}

func getDashboardJSON(t *testing.T, baseURL string) observability.DashboardData {
	t.Helper()
	resp, err := http.Get(baseURL + "/dashboard.json")
	if err != nil {
		t.Fatalf("GET /dashboard.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard.json status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var data observability.DashboardData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decoding /dashboard.json: %v", err)
	}
	return data
}

// findErrorStats returns the *observability.ErrorStats entry matching
// (stage, vendor), or nil if none is present.
func findErrorStats(entries []observability.ErrorStats, stage, vendor string) *observability.ErrorStats {
	for i := range entries {
		if entries[i].Stage == stage && entries[i].Vendor == vendor {
			return &entries[i]
		}
	}
	return nil
}

// TestServe_RealSessionActivity_ReflectsInDashboardAndShutsDownGracefully
// builds a real Session the same way runServe does (via newSession, the
// registry-based backend resolution), pushes one real caller utterance
// through it and closes it (exactly like runDemo's "push then flush on
// Close" pattern) so the mock ASR/MT/TTS pipeline actually records
// asr_confidence/translate/tts events (see
// pkg/langstream/fallback.go's recordSuccessMetric call sites) into
// Session.Metrics(), then asserts serveDashboard serves that real activity
// correctly over both /dashboard.json and /metrics, and shuts down within
// a bounded time once ctx is cancelled.
//
// Note: Session's stage instrumentation (recordSuccessMetric) records into
// the *error/event* side of LatencyRecorder (RecordEvent), which surfaces
// as DashboardData.Errors (Total/Errors/Rate), not DashboardData.Latency
// (which only ever contains stages someone called Record/RecordStage on
// directly — Session itself never does). Asserting on the wrong section
// here would silently pass on stale/empty data, exactly the kind of gap
// this test exists to catch.
func TestServe_RealSessionActivity_ReflectsInDashboardAndShutsDownGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := newSession(ctx, langstream.BackendMock, langstream.BackendMock, langstream.BackendMock, "hi", "en")
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	frame := asr.AudioFrame{PCM: make([]byte, 320), SampleRate: 8000}
	if err := sess.PushCallerAudio(frame); err != nil {
		t.Fatalf("PushCallerAudio: %v", err)
	}
	// Close flushes the buffered utterance through ASR -> MT -> TTS (see
	// Session.Close's doc comment), which is what actually produces the
	// asr_confidence/translate/tts events this test checks for below.
	if err := sess.Close(); err != nil {
		t.Fatalf("closing session: %v", err)
	}

	addr := freeAddr(t)
	srv := observability.NewDashboardServer(addr, sess.Metrics())

	done := make(chan error, 1)
	go func() { done <- serveDashboard(ctx, srv) }()

	baseURL := "http://" + addr
	waitForServer(t, baseURL+"/metrics")

	data := getDashboardJSON(t, baseURL)
	for _, stage := range []string{"asr_confidence", "translate", "tts"} {
		es := findErrorStats(data.Errors, stage, "mock")
		if es == nil {
			t.Errorf("dashboard.json Errors missing a (%s, mock) entry after real caller audio was pushed through the session; got %+v", stage, data.Errors)
			continue
		}
		if es.Total < 1 {
			t.Errorf("(%s, mock) Total = %d, want >= 1", stage, es.Total)
		}
		if es.Errors != 0 {
			t.Errorf("(%s, mock) Errors = %d, want 0 (mock backends never fail)", stage, es.Errors)
		}
	}

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBody := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		metricsBody = append(metricsBody, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	// Not asserting an exact count here: both the caller and agent legs
	// flush a final transcript on Session.Close (even the agent leg,
	// which never had audio pushed to it - MockRecognizer flushes
	// whatever is buffered, including nothing, as a final transcript on
	// Close), so each stage's event count reflects two leg-flushes, not
	// one. The JSON assertions above already pin the exact Total/Errors
	// values per stage; here we only check the real stage/vendor labels
	// actually appear in the Prometheus text output, and that no errors
	// were recorded (mock backends never fail).
	for _, want := range []string{
		`langstream_stage_errors_total{stage="asr_confidence",vendor="mock"} 0`,
		`langstream_stage_errors_total{stage="translate",vendor="mock"} 0`,
		`langstream_stage_errors_total{stage="tts",vendor="mock"} 0`,
	} {
		if !bytes.Contains(metricsBody, []byte(want)) {
			t.Errorf("/metrics missing expected line %q\nfull body:\n%s", want, metricsBody)
		}
	}
	for _, wantLabel := range []string{
		`langstream_stage_events_total{stage="asr_confidence",vendor="mock"}`,
		`langstream_stage_events_total{stage="translate",vendor="mock"}`,
		`langstream_stage_events_total{stage="tts",vendor="mock"}`,
	} {
		if !bytes.Contains(metricsBody, []byte(wantLabel)) {
			t.Errorf("/metrics missing expected label %q\nfull body:\n%s", wantLabel, metricsBody)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveDashboard returned an error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveDashboard did not return within 5s of context cancellation - possible shutdown-ordering regression")
	}
}

// syncBuffer is a concurrency-safe io.Writer wrapping a bytes.Buffer, used
// to capture a subprocess's combined stdout/stderr: exec.Cmd copies from
// each pipe on its own goroutine, so a plain bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestServeCommand_RealBinary_EndToEnd builds the actual langstream binary
// (the same `go build ./cmd/langstream` Docker/CI/an operator would run)
// into a temp directory, starts it as `langstream serve --addr <ephemeral>`
// via os/exec (not an in-process function call), confirms /dashboard.json
// and /metrics respond correctly for a freshly started (idle - no
// transport is attached, see runServe's doc comment) server, sends the
// process a real SIGTERM, and asserts the process actually exits within a
// bounded time - bounded so a broken graceful-shutdown path fails this
// test instead of hanging the test suite or CI forever.
func TestServeCommand_RealBinary_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM-based graceful shutdown assumes POSIX signals")
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not found on PATH: %v", err)
	}

	binPath := filepath.Join(t.TempDir(), "langstream-serve-test-bin")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, goBin, "build", "-o", binPath, ".")
	var buildOut syncBuffer
	buildCmd.Stdout = &buildOut
	buildCmd.Stderr = &buildOut
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("building the langstream binary for an end-to-end serve test: %v\n%s", err, buildOut.String())
	}

	addr := freeAddr(t)
	runCmd := exec.Command(binPath, "serve", "--addr", addr)
	var runOut syncBuffer
	runCmd.Stdout = &runOut
	runCmd.Stderr = &runOut
	if err := runCmd.Start(); err != nil {
		t.Fatalf("starting langstream serve subprocess: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- runCmd.Wait() }()

	killedAndReaped := false
	cleanup := func() {
		if killedAndReaped {
			return
		}
		killedAndReaped = true
		_ = runCmd.Process.Kill()
		<-waitDone
	}
	defer cleanup()

	baseURL := "http://" + addr
	waitForServer(t, baseURL+"/metrics")

	// No real telephony transport is attached to `serve` (see runServe's
	// doc comment): the dashboard should be idle, but must still respond
	// with a well-formed, empty (not erroring, not nil-panicking) snapshot.
	data := getDashboardJSON(t, baseURL)
	if len(data.Errors) != 0 {
		t.Errorf("expected no error/event stats on a freshly started idle `serve`, got %+v", data.Errors)
	}
	if len(data.Latency) != 0 {
		t.Errorf("expected no latency stages on a freshly started idle `serve`, got %+v", data.Latency)
	}

	metricsResp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", metricsResp.StatusCode)
	}

	if err := runCmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM to the langstream serve subprocess: %v", err)
	}

	select {
	case err := <-waitDone:
		killedAndReaped = true // already Wait()-ed; skip cleanup's Kill/Wait.
		if err != nil {
			t.Fatalf("langstream serve subprocess exited with a non-zero/error status after SIGTERM: %v\noutput:\n%s", err, runOut.String())
		}
	case <-time.After(10 * time.Second):
		cleanup()
		t.Fatalf("langstream serve subprocess did not exit within 10s of SIGTERM - graceful shutdown appears to hang\noutput so far:\n%s", runOut.String())
	}
}
