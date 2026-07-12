package rtp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// TestNewDuplexSession_AgentLegFailureReleasesCallerSocket exercises the
// path NewDuplexSession's doc comment calls out explicitly: if
// constructing the agent leg's ClearStream Session fails after the caller
// leg's has already succeeded (and, per clearstream.NewSession, already
// bound a real UDP socket via net.ListenUDP), NewDuplexSession Starts then
// immediately Stops the caller leg solely to release that socket, since
// ClearStream's own Session.Stop() cannot be safely called on a
// never-Start()ed Session (it would hang forever on an internal readiness
// channel -- see that doc comment for the full explanation) and
// ClearStream exposes no other way to release a bound-but-unstarted
// socket.
//
// This test actually triggers that path (an empty AgentLeg.ListenAddr,
// which clearstream.NewSession rejects immediately with "ListenAddr
// required", before it ever touches the network -- see
// clearstream/pkg/rtp/session.go's NewSession) and confirms:
//
//  1. NewDuplexSession returns an error (not a panic, not a hang -- bounded
//     via mustReturnWithin, reusing the same helper duplex_shutdown_test.go
//     uses for the same class of concern).
//  2. The caller leg's socket really is released afterward: a *new*
//     listener can be bound on the exact same address immediately after
//     NewDuplexSession returns, which would fail with "address already in
//     use" if the doc comment's claimed Start-then-Stop release didn't
//     actually work.
func TestNewDuplexSession_AgentLegFailureReleasesCallerSocket(t *testing.T) {
	logger := zap.NewNop()

	// Learn a concrete, currently-free loopback address to hand to the
	// caller leg, the same newLoopbackPort/close-then-rebind pattern
	// TestDuplexSession_EndToEndLoopback already uses.
	callerListenAddr := newLoopbackPort(t)

	callerFwdSink := bindLoopbackSink(t)
	defer callerFwdSink.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	defer sess.Close()

	var (
		duplex *DuplexSession
		newErr error
	)
	mustReturnWithin(t, "NewDuplexSession with a failing agent leg", 5*time.Second, func() {
		duplex, newErr = NewDuplexSession(DuplexConfig{
			CallerLeg: LegConfig{
				ListenAddr:  callerListenAddr,
				ForwardAddr: callerFwdSink.LocalAddr().String(),
				PayloadType: 0,
				JitterDepth: 1,
				Suppressor:  model.NewMockSuppressor(),
				Logger:      logger,
			},
			AgentLeg: LegConfig{
				// Empty ListenAddr: clearstream.NewSession rejects this
				// immediately ("rtp: ListenAddr required"), before
				// attempting any net.ListenUDP call, so this failure is
				// deterministic and isolates exactly the
				// caller-leg-already-succeeded/agent-leg-fails path this
				// test targets.
				ListenAddr:  "",
				ForwardAddr: "127.0.0.1:0",
				PayloadType: 0,
				JitterDepth: 1,
				Suppressor:  model.NewMockSuppressor(),
				Logger:      logger,
			},
			Session: sess,
			Logger:  logger,
		})
	})

	if newErr == nil {
		t.Fatalf("NewDuplexSession: expected an error from the failing agent leg, got nil (duplex=%v)", duplex)
	}
	if duplex != nil {
		t.Errorf("NewDuplexSession: expected nil *DuplexSession alongside a non-nil error, got %v", duplex)
	}
	if !strings.Contains(newErr.Error(), "agent leg") {
		t.Errorf("NewDuplexSession error = %q, want it to mention the agent leg construction failure", newErr.Error())
	}

	// The real assertion: the caller leg's UDP socket must have been
	// released. If NewDuplexSession's Start-then-Stop workaround didn't
	// actually release it, this bind will fail with "address already in
	// use".
	addr, err := net.ResolveUDPAddr("udp", callerListenAddr)
	if err != nil {
		t.Fatalf("resolve %q: %v", callerListenAddr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("caller leg socket %s was not released after NewDuplexSession's agent-leg failure path: %v", callerListenAddr, err)
	}
	conn.Close()
}
