// webrtc.go implements the `langstream webrtc` subcommand: a real,
// two-user, browser-facing test harness for LangStream's translation
// pipeline (see pkg/webrtcgw's package doc comment for the architecture --
// G.711/PCMA over ordinary WebRTC, no Opus/cgo dependency). Unlike
// `duplex` (ClearStream RTP legs, telephony-facing) or `serve` (a Session
// with no transport attached at all), `webrtc` is meant to be opened
// directly in a browser: two people each open the served page, join the
// same room ID with opposite roles, grant mic access, and talk to each
// other live through real ASR->MT->TTS, entirely from their own machines
// with no telephony infrastructure involved.
//
// This reuses `demo`/`serve`/`duplex`'s existing backend-selection path
// (resolveBackend/newSession) unchanged -- which ASR/MT/TTS backend
// powers a room is orthogonal to how audio gets in and out of it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/webrtcgw"
)

// defaultWebRTCAddr is the default HTTP listen address for `webrtc`'s
// combined signaling-server-and-static-client. Distinct from `serve`'s
// defaultDashboardAddr so the two subcommands can run side by side (e.g.
// `serve` for metrics, `webrtc` for a live browser test) without a port
// clash.
const defaultWebRTCAddr = ":8081"

// defaultSTUNServer is used for ICE candidate gathering unless --stun
// overrides it. Google's public STUN server is a widely used, free,
// no-registration-required default suitable for local/developer testing
// (both participants are typically on the same machine or the same LAN
// during a test, so a TURN relay is not needed); a real deployment behind
// restrictive NATs might need to add a TURN server via --stun.
const defaultSTUNServer = "stun:stun.l.google.com:19302"

func runWebRTC(args []string) error {
	fs := flag.NewFlagSet("webrtc", flag.ContinueOnError)
	backend := fs.String("backend", "", `backend name for ASR, MT, and TTS alike (default "mock")`)
	addr := fs.String("addr", defaultWebRTCAddr, "address for the signaling/static-client HTTP server to listen on")
	callerLang := fs.String("caller-lang", "hi", "language the \"caller\" role speaks and hears")
	agentLang := fs.String("agent-lang", "en", "language the \"agent\" role speaks and hears")
	stunServers := fs.String("stun", defaultSTUNServer, "comma-separated STUN/TURN server URLs for ICE (empty to disable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	asrName := resolveBackend(*backend, envASRBackend)
	mtName := resolveBackend(*backend, envMTBackend)
	ttsName := resolveBackend(*backend, envTTSBackend)

	var iceServers []webrtc.ICEServer
	for _, u := range strings.Split(*stunServers, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{u}})
	}

	mgr := webrtcgw.NewManager(func(ctx context.Context) (*langstream.Session, error) {
		return newSession(ctx, asrName, mtName, ttsName, langstream.Language(*callerLang), langstream.Language(*agentLang))
	})

	mux := http.NewServeMux()
	mux.Handle("/", webrtcgw.StaticHandler())
	mux.Handle("/ws", webrtcgw.SignalingHandler(mgr, iceServers))

	srv := &http.Server{Addr: *addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("langstream webrtc: backends asr=%s mt=%s tts=%s (caller=%s, agent=%s)\n", asrName, mtName, ttsName, *callerLang, *agentLang)
	fmt.Printf("langstream webrtc: open http://localhost%s in two browser tabs (or two machines) to test a live translated call\n", *addr)
	fmt.Println("langstream webrtc: press Ctrl+C to stop")

	return serveWebRTC(ctx, srv)
}

// serveWebRTC runs srv until ctx is cancelled, then shuts it down
// gracefully (bounded, mirroring serveDashboard's own contract in
// main.go) and returns. Split out from runWebRTC so it can be exercised
// directly in tests without flag parsing or OS signal delivery, exactly
// like serveDashboard is for runServe.
func serveWebRTC(ctx context.Context, srv *http.Server) error {
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
