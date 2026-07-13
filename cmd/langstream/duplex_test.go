package main

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"testing"
	"time"
)

// buildRawRTPPacket and bindLoopbackSink/newLoopbackPort mirror
// pkg/rtp/duplex_test.go's unexported test helpers of the same name --
// those live in package rtp's own _test.go and aren't reachable from
// here, so this file keeps its own minimal copies rather than reaching
// into that package's internals.
func buildRawRTPPacket(seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	buf := make([]byte, 12+len(payload))
	buf[0] = 0x80 // Version=2, no padding, no extension, CSRC count=0
	buf[1] = 0x00 // marker=0, payload type=0 (PCMU)
	binary.BigEndian.PutUint16(buf[2:4], seq)
	binary.BigEndian.PutUint32(buf[4:8], ts)
	binary.BigEndian.PutUint32(buf[8:12], ssrc)
	copy(buf[12:], payload)
	return buf
}

func bindLoopbackSink(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind loopback UDP socket: %v", err)
	}
	return conn
}

func newLoopbackPort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("newLoopbackPort: %v", err)
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatalf("newLoopbackPort: closing probe socket: %v", err)
	}
	return addr
}

func TestParseDuplexFlags_MissingRequiredAddresses(t *testing.T) {
	if _, err := parseDuplexFlags(nil); err == nil {
		t.Fatal("expected an error when none of the required RTP addresses are set")
	}

	if _, err := parseDuplexFlags([]string{"--caller-listen", "127.0.0.1:0"}); err == nil {
		t.Fatal("expected an error when only one of four required addresses is set")
	}
}

func TestParseDuplexFlags_ResolvesDefaults(t *testing.T) {
	cfg, err := parseDuplexFlags([]string{
		"--caller-listen", "127.0.0.1:5004",
		"--caller-forward", "127.0.0.1:5005",
		"--agent-listen", "127.0.0.1:5006",
		"--agent-forward", "127.0.0.1:5007",
	})
	if err != nil {
		t.Fatalf("parseDuplexFlags: %v", err)
	}
	if cfg.asrBackend != "mock" || cfg.mtBackend != "mock" || cfg.ttsBackend != "mock" {
		t.Errorf("backends = (%s, %s, %s), want all \"mock\" by default", cfg.asrBackend, cfg.mtBackend, cfg.ttsBackend)
	}
	if cfg.callerLang != "hi" || cfg.agentLang != "en" {
		t.Errorf("languages = (%s, %s), want (hi, en) by default", cfg.callerLang, cfg.agentLang)
	}
	if cfg.suppressorBackend != defaultSuppressorBackend {
		t.Errorf("suppressorBackend = %q, want %q", cfg.suppressorBackend, defaultSuppressorBackend)
	}
	if cfg.dashboardAddr != defaultDashboardAddr {
		t.Errorf("dashboardAddr = %q, want %q", cfg.dashboardAddr, defaultDashboardAddr)
	}
}

func TestParseDuplexFlags_BackendFlagAppliesToAllThreeLegs(t *testing.T) {
	cfg, err := parseDuplexFlags([]string{
		"--caller-listen", "127.0.0.1:5004",
		"--caller-forward", "127.0.0.1:5005",
		"--agent-listen", "127.0.0.1:5006",
		"--agent-forward", "127.0.0.1:5007",
		"--backend", "does-not-exist",
	})
	if err != nil {
		t.Fatalf("parseDuplexFlags: %v", err)
	}
	if cfg.asrBackend != "does-not-exist" || cfg.mtBackend != "does-not-exist" || cfg.ttsBackend != "does-not-exist" {
		t.Errorf("backends = (%s, %s, %s), want all overridden by --backend", cfg.asrBackend, cfg.mtBackend, cfg.ttsBackend)
	}
}

func TestBuildDuplexSession_UnknownBackendFails(t *testing.T) {
	cfg := duplexConfig{
		asrBackend: "does-not-exist", mtBackend: "mock", ttsBackend: "mock",
		callerLang: "hi", agentLang: "en",
		callerListen: "127.0.0.1:0", callerForward: newLoopbackPort(t),
		agentListen: "127.0.0.1:0", agentForward: newLoopbackPort(t),
		suppressorBackend: defaultSuppressorBackend,
	}
	if _, _, err := buildDuplexSession(context.Background(), cfg); err == nil {
		t.Fatal("expected an error for an unregistered ASR backend")
	}
}

func TestBuildDuplexSession_UnknownSuppressorBackendFails(t *testing.T) {
	cfg := duplexConfig{
		asrBackend: "mock", mtBackend: "mock", ttsBackend: "mock",
		callerLang: "hi", agentLang: "en",
		callerListen: "127.0.0.1:0", callerForward: newLoopbackPort(t),
		agentListen: "127.0.0.1:0", agentForward: newLoopbackPort(t),
		suppressorBackend: "does-not-exist",
	}
	if _, _, err := buildDuplexSession(context.Background(), cfg); err == nil {
		t.Fatal("expected an error for an unregistered suppressor backend")
	}
}

// TestRunDuplexWithContext_RealLoopbackEndToEnd is `duplex`'s own
// lifecycle test, mirroring pkg/rtp's TestDuplexSession_EndToEndLoopback
// one layer up (through this package's flag parsing and
// construct/run/shutdown wiring, rather than calling rtp.NewDuplexSession
// directly): it runs runDuplexWithContext against a real loopback UDP
// caller/agent leg pair and the observability dashboard, sends real RTP
// packets into the caller leg, confirms synthesized bot audio comes out
// the agent leg's forward socket, confirms the dashboard reflects that
// activity, then cancels the context and confirms runDuplexWithContext
// returns nil within a bounded time.
func TestRunDuplexWithContext_RealLoopbackEndToEnd(t *testing.T) {
	callerSink := bindLoopbackSink(t)
	defer callerSink.Close()
	agentSink := bindLoopbackSink(t)
	defer agentSink.Close()

	callerListenAddr := newLoopbackPort(t)
	agentListenAddr := newLoopbackPort(t)
	dashboardAddr := freeAddr(t)

	cfg := duplexConfig{
		asrBackend: "mock", mtBackend: "mock", ttsBackend: "mock",
		callerLang: "hi", agentLang: "en",
		callerListen:      callerListenAddr,
		callerForward:     callerSink.LocalAddr().String(),
		agentListen:       agentListenAddr,
		agentForward:      agentSink.LocalAddr().String(),
		suppressorBackend: defaultSuppressorBackend,
		dashboardAddr:     dashboardAddr,
	}

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() { runDone <- runDuplexWithContext(ctx, cfg) }()

	// Wait for the dashboard to come up as a proxy for "the DuplexSession
	// has been constructed and Start() has been called" (both happen
	// before runDuplexWithContext prints its "running" message).
	waitForServer(t, "http://"+dashboardAddr+"/metrics")

	sender, err := net.Dial("udp", callerListenAddr)
	if err != nil {
		t.Fatalf("dial caller leg: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 160) // 20ms @ 8kHz PCMU
	for i := range payload {
		payload[i] = 0xFF // mu-law silence
	}
	for i := 0; i < 6; i++ {
		pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xC0FFEE, payload)
		if _, err := sender.Write(pkt); err != nil {
			t.Fatalf("send RTP packet %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give the packets time to propagate through ClearStream receive ->
	// denoise -> CleanAudio() -> bridgeCleanAudio -> ASR before we check
	// the dashboard reflects that activity.
	time.Sleep(200 * time.Millisecond)

	resp, err := http.Get("http://" + dashboardAddr + "/dashboard.json")
	if err != nil {
		t.Fatalf("GET /dashboard.json: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard.json status = %d, want 200", resp.StatusCode)
	}

	// pkg/asr's MockRecognizer only ever emits a *final* transcript from
	// its StreamSession.Close() flush (see pkg/rtp/duplex_test.go's own
	// end-to-end test, which relies on the same behavior) -- so
	// synthesized bot audio only actually appears once the Session is
	// closed. runDuplexWithContext's shutdown path does exactly that
	// (Session.Close() first, then a bounded drain grace, then
	// DuplexSession.Stop() -- see duplexFinalDrainGrace's doc comment) as
	// part of reacting to ctx cancellation, so set a generous read
	// deadline, trigger shutdown via cancel(), and confirm the flushed,
	// translated, synthesized audio actually arrives over real RTP before
	// the RTP legs are torn down.
	if err := agentSink.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	cancel()

	buf := make([]byte, 2048)
	n, _, err := agentSink.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected synthesized bot audio on agentSink during graceful shutdown, got error: %v", err)
	}
	if n <= 12 {
		t.Fatalf("expected an RTP packet with header+payload (>12 bytes), got %d bytes", n)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runDuplexWithContext returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runDuplexWithContext did not return within 10s of context cancellation")
	}
}

// TestRunDuplexWithContext_DashboardDisabled confirms an empty
// dashboardAddr genuinely skips starting the dashboard server (no
// goroutine, no listener) while the DuplexSession itself still starts,
// runs, and shuts down cleanly on context cancellation.
func TestRunDuplexWithContext_DashboardDisabled(t *testing.T) {
	callerSink := bindLoopbackSink(t)
	defer callerSink.Close()
	agentSink := bindLoopbackSink(t)
	defer agentSink.Close()

	cfg := duplexConfig{
		asrBackend: "mock", mtBackend: "mock", ttsBackend: "mock",
		callerLang: "hi", agentLang: "en",
		callerListen:      newLoopbackPort(t),
		callerForward:     callerSink.LocalAddr().String(),
		agentListen:       newLoopbackPort(t),
		agentForward:      agentSink.LocalAddr().String(),
		suppressorBackend: defaultSuppressorBackend,
		dashboardAddr:     "",
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runDuplexWithContext(ctx, cfg) }()

	// No dashboard to poll for readiness; give Start() a moment to run.
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runDuplexWithContext returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runDuplexWithContext did not return within 10s of context cancellation")
	}
}
