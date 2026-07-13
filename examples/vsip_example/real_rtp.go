// real_rtp.go adds the piece doc.go originally flagged as missing (see its
// "does NOT implement... an RTP socket" bullet, written 2026-07-08/09
// before pkg/rtp.DuplexSession existed): a real UDP/RTP-socket-backed
// demonstration of a vSIP-style call, built directly on
// rtp.NewDuplexSession/DuplexSession -- the same duplex bridge
// pkg/rtp/duplex_test.go's end-to-end loopback test and `langstream
// duplex` (see cmd/langstream/duplex.go) both use. It complements, rather
// than replaces, main.go's VSIPCallAdapter demo: that one shows the
// Session-method-level integration *contract/shape* with in-process
// channels standing in for RTP; this one proves the same Session
// integrates correctly behind two real ClearStream RTP legs and real
// loopback UDP sockets -- what a real Exotel vSIP integration's RTP layer
// would actually look like plugged in, still using mock ASR/MT/TTS
// backends (ROADMAP.md's Week 2 decision: the RTP/socket wiring being
// real is the point here, not the vendor backends).
//
// This still is not Exotel vSIP trunk signaling: the two "sinks" below
// stand in for wherever a real vSIP trunk would actually terminate each
// leg's RTP (see runRealRTPDemo's doc comment), and there is no SIP
// call-control here at all -- see doc.go for the full, still-accurate
// scope note on what remains out of scope.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"

	"github.com/exotel/langstream/pkg/rtp"
)

// realRTPDemoTimeout bounds the whole real-RTP demo (session construction,
// RTP send, propagation, flush, and shutdown) -- generous for real (if
// loopback) socket I/O and the mock backends' effectively-instant
// processing.
const realRTPDemoTimeout = 10 * time.Second

// buildRawRTPPacket constructs a minimal, valid RTP packet (version 2, no
// padding/extension/CSRCs) carrying payload, for use as this demo's
// simulated inbound "caller speaking" audio. Mirrors the same small helper
// pkg/rtp's own tests use (that one is unexported in package rtp's
// _test.go, unreachable from here) -- this package intentionally doesn't
// reimplement real RTP framing anywhere outside this demo file.
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

// bindLoopbackUDP opens a UDP socket on 127.0.0.1 with an OS-assigned
// port, for use either as a leg's ForwardAddr (to observe what it sends
// out) or as an address this demo's own simulated RTP sender dials.
func bindLoopbackUDP() (*net.UDPConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
}

// learnLoopbackAddr binds a UDP socket on 127.0.0.1 with an OS-assigned
// port, immediately closes it, and returns that address -- used to learn
// a concrete port number to pass to both a DuplexSession leg's ListenAddr
// and this demo's own simulated RTP sender, since ClearStream's Session
// does not expose its actual bound address to callers outside its own
// package. This has the standard, small, pragmatic close-then-rebind race
// (another process could grab the port first) that this pattern always
// has.
func learnLoopbackAddr() (string, error) {
	conn, err := bindLoopbackUDP()
	if err != nil {
		return "", err
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		return "", err
	}
	return addr, nil
}

// runRealRTPDemo builds a real langstream.Session (mock ASR/MT/TTS
// backends, same as newDemoSession) bridged via a real rtp.DuplexSession
// to two real ClearStream RTP legs on loopback UDP, sends real RTP
// packets carrying simulated caller speech into the caller leg's socket,
// closes the Session to flush a final (translated, synthesized) mock
// transcript, and confirms real RTP packets carrying that synthesized bot
// audio arrive on the agent leg's forward socket -- proving the actual
// RTP/socket plumbing (not just the in-process Session-method contract
// main.go's VSIPCallAdapter demo shows) works end to end.
//
// callerSink/agentSink stand in for wherever a real Exotel vSIP trunk
// would terminate each leg's RTP; a real integration would replace them
// with the trunk's actual remote RTP endpoints (learned via SIP/SDP
// negotiation, entirely out of scope here -- see doc.go) instead of a
// loopback socket this same process also owns.
func runRealRTPDemo() error {
	// A fresh, independent timeout -- deliberately not derived from
	// main's own 5-second demo ctx (or any caller-supplied ctx): this
	// demo constructs its own langstream.Session (see newDemoSession),
	// and that Session's entire internal lifecycle is derived from
	// whatever ctx it's given (see session.go's sessCtx) -- reusing a ctx
	// that might already be close to its deadline (or, worse, cancelled
	// for an unrelated reason by whatever called this) could cancel the
	// Session's internal goroutines out from under sess.Close() before
	// its flush finishes, exactly the shutdown-ordering hazard
	// cmd/langstream/duplex.go's buildDuplexSession doc comment describes
	// in more detail for the same underlying reason.
	ctx, cancel := context.WithTimeout(context.Background(), realRTPDemoTimeout)
	defer cancel()

	fmt.Println()
	fmt.Println("--- real RTP/socket demo (rtp.DuplexSession) -----------------------------")
	fmt.Println("Unlike the VSIPCallAdapter demo above (in-process channels standing in for")
	fmt.Println("RTP), this builds a real rtp.DuplexSession over real loopback UDP sockets,")
	fmt.Println("bridged to a langstream.Session with the same mock ASR/MT/TTS backends.")

	sess, err := newDemoSession(ctx)
	if err != nil {
		return fmt.Errorf("real RTP demo: creating session: %w", err)
	}

	callerSink, err := bindLoopbackUDP()
	if err != nil {
		return fmt.Errorf("real RTP demo: binding caller sink: %w", err)
	}
	defer callerSink.Close()

	agentSink, err := bindLoopbackUDP()
	if err != nil {
		return fmt.Errorf("real RTP demo: binding agent sink: %w", err)
	}
	defer agentSink.Close()

	callerListenAddr, err := learnLoopbackAddr()
	if err != nil {
		return fmt.Errorf("real RTP demo: learning caller listen address: %w", err)
	}

	callerSuppressor, err := model.NewSuppressor(model.SuppressorConfig{Backend: "passthrough"})
	if err != nil {
		return fmt.Errorf("real RTP demo: constructing caller leg suppressor: %w", err)
	}
	agentSuppressor, err := model.NewSuppressor(model.SuppressorConfig{Backend: "passthrough"})
	if err != nil {
		return fmt.Errorf("real RTP demo: constructing agent leg suppressor: %w", err)
	}

	duplex, err := rtp.NewDuplexSession(rtp.DuplexConfig{
		CallerLeg: rtp.LegConfig{
			ListenAddr:  callerListenAddr,
			ForwardAddr: callerSink.LocalAddr().String(),
			PayloadType: 0,
			Suppressor:  callerSuppressor,
			Logger:      zap.NewNop(),
		},
		AgentLeg: rtp.LegConfig{
			ListenAddr:  "127.0.0.1:0",
			ForwardAddr: agentSink.LocalAddr().String(),
			PayloadType: 0,
			Suppressor:  agentSuppressor,
			Logger:      zap.NewNop(),
		},
		Session: sess,
		Logger:  zap.NewNop(),
	})
	if err != nil {
		return fmt.Errorf("real RTP demo: constructing DuplexSession: %w", err)
	}

	duplex.Start()
	defer duplex.Stop()

	fmt.Printf("real RTP demo: caller leg listening on %s, forwarding to %s\n", callerListenAddr, callerSink.LocalAddr())
	fmt.Printf("real RTP demo: agent leg forwarding synthesized bot audio to %s\n", agentSink.LocalAddr())

	sender, err := net.Dial("udp", callerListenAddr)
	if err != nil {
		return fmt.Errorf("real RTP demo: dialing caller leg: %w", err)
	}
	defer sender.Close()

	// Simulate the caller speaking: a handful of real RTP packets (PCMU
	// mu-law silence, matching ClearStream's and pkg/rtp's own test
	// fixtures) sent into the caller leg's real UDP socket -- real bytes
	// flow through a real socket here, unlike fakeAudioSource's in-memory
	// channel in the VSIPCallAdapter demo above.
	payload := make([]byte, 160) // 20ms @ 8kHz PCMU
	for i := range payload {
		payload[i] = 0xFF // mu-law silence
	}
	for i := 0; i < 6; i++ {
		pkt := buildRawRTPPacket(uint16(i), uint32(i*160), 0xC0FFEE, payload)
		if _, err := sender.Write(pkt); err != nil {
			return fmt.Errorf("real RTP demo: sending simulated caller RTP packet %d: %w", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("real RTP demo: sent 6 simulated caller RTP packets")

	// Give the packets time to propagate: ClearStream receive -> denoise
	// -> CleanAudio() -> bridgeCleanAudio -> ASR.PushAudio.
	time.Sleep(200 * time.Millisecond)

	// Closing the Session flushes the mock ASR's canned final transcript,
	// translates and synthesizes it, and closes AgentHearsAudio() --
	// which DuplexSession's feedTTSPacer/runTTSPacer relay to the agent
	// leg's real InjectBotAudio (see pkg/rtp/duplex.go).
	if err := sess.Close(); err != nil {
		return fmt.Errorf("real RTP demo: closing session: %w", err)
	}
	fmt.Println("real RTP demo: closed session (flushes + synthesizes the mock transcript)")

	if err := agentSink.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("real RTP demo: setting read deadline: %w", err)
	}
	buf := make([]byte, 2048)
	n, _, err := agentSink.ReadFromUDP(buf)
	if err != nil {
		return fmt.Errorf("real RTP demo: expected synthesized bot audio on the agent leg's forward socket: %w", err)
	}
	fmt.Printf("real RTP demo: received a %d-byte real RTP packet carrying synthesized bot audio on the agent leg\n", n)
	fmt.Println("real RTP demo complete: real UDP sockets and real RTP packets were used end to end")
	fmt.Println("---------------------------------------------------------------------------")

	return nil
}
