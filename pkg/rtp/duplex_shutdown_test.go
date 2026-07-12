package rtp

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// newTestDuplexSession builds a DuplexSession wired to a fresh
// langstream.Session with this repo's mock backends, on loopback UDP with
// OS-assigned ports, for the Stop()-edge-case tests below (none of them
// care about actual audio flow, only about Stop()'s own behavior under
// unusual call patterns). The returned cleanup func closes the
// langstream.Session; it does not call duplex.Stop() (callers of this
// helper are specifically testing Stop() themselves).
func newTestDuplexSession(t *testing.T) (duplex *DuplexSession, cleanup func()) {
	t.Helper()
	logger := zap.NewNop()

	ctx, cancelSession := context.WithCancel(context.Background())

	sess, err := langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            asr.NewMockRecognizer("hi", "en"),
		Translator:     translate.NewMockTranslator([2]translate.Language{"hi", "en"}, [2]translate.Language{"en", "hi"}),
		TTS:            tts.NewMockSynthesizer("hi", "en"),
	})
	if err != nil {
		t.Fatalf("langstream.NewSession: %v", err)
	}

	d, err := NewDuplexSession(DuplexConfig{
		CallerLeg: LegConfig{
			ListenAddr:  "127.0.0.1:0",
			ForwardAddr: "127.0.0.1:0",
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		AgentLeg: LegConfig{
			ListenAddr:  "127.0.0.1:0",
			ForwardAddr: "127.0.0.1:0",
			PayloadType: 0,
			JitterDepth: 1,
			Suppressor:  model.NewMockSuppressor(),
			Logger:      logger,
		},
		Session: sess,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewDuplexSession: %v", err)
	}

	return d, func() {
		cancelSession()
		_ = sess.Close()
	}
}

// mustReturnWithin runs fn in a goroutine and fails t (with the given
// label) if it doesn't return within timeout -- the "hunting for hangs
// -race won't catch" pattern the task calls for, since -race only finds
// data races, not deadlocks.
func mustReturnWithin(t *testing.T, label string, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s: did not return within %s -- suspected hang", label, timeout)
	}
}

// TestDuplexSession_StopBeforeStart calls Stop() on a DuplexSession that
// was constructed (NewDuplexSession binds both legs' UDP sockets) but
// never Start()ed. Per Stop()'s own doc comment, d.started is false in
// this case, so neither ClearStream session's Stop() is called (calling
// ClearStream's Session.Stop() on a never-Start()ed Session would itself
// hang forever on its internal rtcpReady channel -- see NewDuplexSession's
// doc comment on the same gap) and the four bridging goroutines were
// never launched, so there is nothing for the internal wg.Wait() to wait
// on. This test confirms that path actually returns promptly, with a nil
// error, rather than hanging on either the (never-signaled) ClearStream
// internals or the (never-populated) WaitGroup.
func TestDuplexSession_StopBeforeStart(t *testing.T) {
	duplex, cleanup := newTestDuplexSession(t)
	defer cleanup()

	var stopErr error
	mustReturnWithin(t, "Stop() before Start()", 5*time.Second, func() {
		stopErr = duplex.Stop()
	})
	if stopErr != nil {
		t.Errorf("Stop() before Start(): got error %v, want nil", stopErr)
	}
}

// TestDuplexSession_StopImmediatelyAfterStart calls Start() then Stop()
// back-to-back with no audio ever sent on either leg, exercising the
// documented ordering (ClearStream sessions stopped -- and their
// CleanAudio() channels closed -- before bridgeCleanAudio ever observes a
// single frame) in the degenerate case where the four bridging goroutines
// may not have even reached their first select iteration yet when Stop
// runs.
func TestDuplexSession_StopImmediatelyAfterStart(t *testing.T) {
	duplex, cleanup := newTestDuplexSession(t)
	defer cleanup()

	duplex.Start()

	var stopErr error
	mustReturnWithin(t, "Stop() immediately after Start()", 5*time.Second, func() {
		stopErr = duplex.Stop()
	})
	if stopErr != nil {
		t.Errorf("Stop() immediately after Start(): got error %v, want nil", stopErr)
	}
}

// TestDuplexSession_StopConcurrentFromMultipleGoroutines calls Stop()
// from many goroutines at once. Stop() is documented idempotent and safe
// for concurrent callers via stopOnce, all of whom "observe the same
// result" -- this test drives that concurrently (not just sequentially,
// which sync.Once would trivially get right even with a subtly broken
// implementation) and confirms every single call returns the identical
// error value promptly, with no panic (-race is also expected to run
// clean over this, but a hang is the specific failure mode -race can't
// catch on its own, hence the bounded deadline).
func TestDuplexSession_StopConcurrentFromMultipleGoroutines(t *testing.T) {
	duplex, cleanup := newTestDuplexSession(t)
	defer cleanup()

	duplex.Start()

	const n = 20
	errs := make([]error, n)

	mustReturnWithin(t, "concurrent Stop() calls", 5*time.Second, func() {
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				errs[i] = duplex.Stop()
			}(i)
		}
		wg.Wait()
	})

	for i, err := range errs {
		if err != nil {
			t.Errorf("Stop() call %d: got error %v, want nil", i, err)
		}
	}

	// A final, sequential call after the storm above should also still
	// return the same (nil) result instantly.
	if err := duplex.Stop(); err != nil {
		t.Errorf("Stop() after concurrent storm: got error %v, want nil", err)
	}
}

// TestDuplexSession_StopConcurrentWithStart calls Start() and Stop()
// concurrently against each other (not just Stop() against Stop()),
// simulating a caller that races a shutdown signal (e.g. SIP BYE)
// against its own startup. This specifically checks the *pair* racing
// doesn't hang, panic, or race.
//
// FIXED (2026-07-12, EM): this test originally caught a real data race in
// DuplexSession.Start()/Stop() -- Start()'s `d.wg.Add(4)` and Stop()'s
// internal `d.wg.Wait()` goroutine ran under two *independent* sync.Once
// guards (startOnce, stopOnce) with no happens-before edge between them,
// so a Stop() that reached wg.Wait() before a concurrent Start() reached
// wg.Add(4) was exactly the "Add with a positive delta concurrent with
// Wait while the counter may still be zero" pattern sync.WaitGroup's own
// doc comment calls out as a data race (observed ~2/5 runs failing under
// go test -race before the fix). Reported by QA, fixed by the EM same
// day: Start/Stop now coordinate through a single lifecycleMu mutex (see
// duplex.go's DuplexSession struct doc comment) that guarantees Start's
// wg.Add(4) either fully happens-before any concurrent Stop's wg.Wait(),
// or never happens at all (Stop already won the race, Start bails out
// before touching wg). This test now passes reliably (verified at
// -race -count=30 with zero failures) and stays in the suite as a
// permanent regression guard for that class of bug.
func TestDuplexSession_StopConcurrentWithStart(t *testing.T) {
	duplex, cleanup := newTestDuplexSession(t)
	defer cleanup()

	mustReturnWithin(t, "concurrent Start()/Stop()", 5*time.Second, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			duplex.Start()
		}()
		var stopErr error
		go func() {
			defer wg.Done()
			stopErr = duplex.Stop()
			_ = stopErr
		}()
		wg.Wait()
	})

	// Whichever interleaving happened, the session must now be fully
	// stopped: a second, purely sequential Stop() call afterwards must
	// still return promptly and with a nil error.
	mustReturnWithin(t, "Stop() after concurrent Start()/Stop() race", 5*time.Second, func() {
		if err := duplex.Stop(); err != nil {
			t.Errorf("Stop() after race: got error %v, want nil", err)
		}
	})
}
