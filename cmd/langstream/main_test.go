package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/observability"
)

func TestResolveBackend(t *testing.T) {
	const envVar = "LANGSTREAM_TEST_BACKEND_ENV"

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envVar, "from-env")
		if got := resolveBackend("from-flag", envVar); got != "from-flag" {
			t.Fatalf("resolveBackend() = %q, want %q", got, "from-flag")
		}
	})

	t.Run("env wins over default when flag unset", func(t *testing.T) {
		t.Setenv(envVar, "from-env")
		if got := resolveBackend("", envVar); got != "from-env" {
			t.Fatalf("resolveBackend() = %q, want %q", got, "from-env")
		}
	})

	t.Run("falls back to mock when neither set", func(t *testing.T) {
		if err := os.Unsetenv(envVar); err != nil {
			t.Fatalf("unsetting env: %v", err)
		}
		if got := resolveBackend("", envVar); got != langstream.BackendMock {
			t.Fatalf("resolveBackend() = %q, want %q", got, langstream.BackendMock)
		}
	})
}

func TestNewSession_MockBackends(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newSession(ctx, langstream.BackendMock, langstream.BackendMock, langstream.BackendMock, "hi", "en")
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	defer func() {
		if err := sess.Close(); err != nil {
			t.Errorf("closing session: %v", err)
		}
	}()

	if sess.Metrics() == nil {
		t.Fatal("expected a non-nil Metrics() recorder")
	}
}

func TestNewSession_UnknownASRBackend(t *testing.T) {
	if _, err := newSession(context.Background(), "does-not-exist", langstream.BackendMock, langstream.BackendMock, "hi", "en"); err == nil {
		t.Fatal("expected an error for an unregistered ASR backend, got nil")
	}
}

func TestNewSession_UnknownTTSBackend(t *testing.T) {
	if _, err := newSession(context.Background(), langstream.BackendMock, langstream.BackendMock, "does-not-exist", "hi", "en"); err == nil {
		t.Fatal("expected an error for an unregistered TTS backend, got nil")
	}
}

// freeAddr asks the OS for a free TCP port on 127.0.0.1, then immediately
// releases it. There's an inherent (tiny) TOCTOU race in reusing the
// address afterwards, but it's the standard, good-enough pattern for
// picking an ephemeral test port.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing reserved port: %v", err)
	}
	return addr
}

func TestServeDashboard_ServesAndShutsDownGracefully(t *testing.T) {
	rec := observability.NewLatencyRecorder()
	rec.RecordEvent("asr", "mock")

	addr := freeAddr(t)
	srv := observability.NewDashboardServer(addr, rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveDashboard(ctx, srv)
	}()

	// Poll until the server is actually accepting connections rather than
	// sleeping a fixed duration, so the test isn't flaky under load.
	var resp *http.Response
	var getErr error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr = http.Get("http://" + addr + "/metrics")
		if getErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if getErr != nil {
		cancel()
		<-done
		t.Fatalf("GET /metrics never succeeded: %v", getErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveDashboard returned an error on graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveDashboard did not return within 3s of context cancellation")
	}
}

func TestServeDashboard_ListenErrorSurfaced(t *testing.T) {
	rec := observability.NewLatencyRecorder()

	// Occupy a port so the dashboard server's own ListenAndServe fails
	// immediately, proving serveDashboard surfaces that error instead of
	// blocking forever waiting on ctx.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	srv := observability.NewDashboardServer(addr, rec)

	err = serveDashboard(context.Background(), srv)
	if err == nil {
		t.Fatal("expected serveDashboard to return an error when the address is already in use")
	}
}
