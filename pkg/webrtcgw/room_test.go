// room_test.go covers Manager/Room lifecycle hardening added 2026-07-15:
// an idle-room timeout (a room that never reaches two peers is
// automatically closed instead of leaking its Session/goroutines
// forever) and an optional cap on concurrent rooms. These tests exercise
// Manager.Join directly rather than going through the full
// SignalingHandler/testBrowser machinery gateway_test.go uses -- room
// lifecycle is a Manager-level concern that doesn't need a real WebRTC
// handshake to observe, and driving it directly keeps these tests fast
// and deterministic even with a very short idle timeout.
package webrtcgw

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

// waitForRoomCount polls mgr.RoomCount() until it equals want or
// deadline elapses, failing the test in the latter case. Mirrors the
// polling pattern gateway_test.go's TestManager_RoomCleanupOnBothPeersLeaving
// already uses for the same reason: room cleanup happens asynchronously
// (from a timer or from OnConnectionStateChange), not synchronously
// within any call the test itself makes.
func waitForRoomCount(t *testing.T, mgr *Manager, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if mgr.RoomCount() == want {
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("RoomCount() never reached %d within %s (still %d)", want, timeout, mgr.RoomCount())
		}
	}
}

// TestManager_IdleSinglePeerRoomIsExpired verifies the core hardening: a
// room that only ever gets one peer (the second participant never shows
// up -- a dead link, a browser crash before joining, an abandoned tab)
// is automatically closed once the configured idle timeout elapses, not
// left running forever. It also verifies the lone peer's own
// PeerConnection is force-closed (so its inbound/outbound bridge
// goroutines actually exit, not just the room bookkeeping) and that the
// room's Session was really torn down (its outbound audio channels
// close), not merely forgotten about while its ASR/MT/TTS session keeps
// running in the background.
func TestManager_IdleSinglePeerRoomIsExpired(t *testing.T) {
	const idleTimeout = 80 * time.Millisecond
	mgr := NewManager(mockSessionFactory, WithIdleTimeout(idleTimeout))
	room := uniqueRoom("idle-single")

	p, r, err := mgr.Join(room, roleCaller, nil, nil)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if got := mgr.RoomCount(); got != 1 {
		t.Fatalf("RoomCount() = %d, want 1 immediately after the first peer joins", got)
	}

	// The room should disappear from the Manager well after idleTimeout
	// has elapsed (generous upper bound: 25x the timeout, to absorb
	// scheduler slop under `-race`, which this package's other tests
	// also budget for with multi-second deadlines against sub-second
	// operations).
	waitForRoomCount(t, mgr, 0, 2*time.Second)

	// The lone peer's PeerConnection must have been force-closed too --
	// otherwise its startInboundBridge/startOutboundBridge goroutines
	// (peer.go) would leak: startOutboundBridge exits on ctx.Done(a
	// room.cancel() covers that), but startInboundBridge's blocking
	// track.ReadRTP() call only unblocks (with an error) once the
	// PeerConnection itself is closed.
	deadline := time.Now().Add(2 * time.Second)
	for p.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		if time.Now().After(deadline) {
			t.Fatalf("peer's PeerConnection never reached Closed state (last state: %s)", p.pc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The room's Session must have actually been closed (not just
	// dropped from the map): AgentHearsAudio's channel is documented to
	// close once Close has fully shut the session down.
	select {
	case _, ok := <-r.session.AgentHearsAudio():
		if ok {
			t.Error("AgentHearsAudio() delivered a value instead of being closed after room expiry")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AgentHearsAudio() channel was never closed after the idle room expired")
	}
}

// TestManager_FullRoomSurvivesIdleTimeout is TestManager_IdleSinglePeerRoomIsExpired's
// counterpart: a room that reaches both a caller and an agent before the
// idle timeout elapses must NOT be closed by that timeout just because
// it happened to still be running when the timer fired -- Join is
// expected to have stopped the timer already. This guards specifically
// against a regression where the idle-timeout hardening added here
// becomes overzealous and starts tearing down perfectly normal, active
// two-party calls.
func TestManager_FullRoomSurvivesIdleTimeout(t *testing.T) {
	const idleTimeout = 80 * time.Millisecond
	mgr := NewManager(mockSessionFactory, WithIdleTimeout(idleTimeout))
	room := uniqueRoom("idle-full")

	callerPeer, _, err := mgr.Join(room, roleCaller, nil, nil)
	if err != nil {
		t.Fatalf("Join(caller): %v", err)
	}
	agentPeer, _, err := mgr.Join(room, roleAgent, nil, nil)
	if err != nil {
		t.Fatalf("Join(agent): %v", err)
	}

	// Wait well past the idle timeout and confirm the room is still
	// alive throughout, not just at one sampled instant.
	deadline := time.Now().Add(5 * idleTimeout)
	for time.Now().Before(deadline) {
		if got := mgr.RoomCount(); got != 1 {
			t.Fatalf("RoomCount() = %d, want 1 -- a full two-peer room must not be closed by the idle timeout", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if callerPeer.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.Error("caller's PeerConnection was force-closed even though the room reached both peers before the idle timeout")
	}
	if agentPeer.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.Error("agent's PeerConnection was force-closed even though the room reached both peers before the idle timeout")
	}

	// Clean up explicitly rather than relying on OnConnectionStateChange
	// firing from a bare (never offer/answer'd) PeerConnection's Close.
	mgr.leave(room, roleCaller)
	mgr.leave(room, roleAgent)
	_ = callerPeer.close()
	_ = agentPeer.close()
	waitForRoomCount(t, mgr, 0, 2*time.Second)
}

// TestManager_MaxRoomsRejectsNewRoomsPastCap verifies WithMaxRooms only
// blocks *new* room creation once the cap is hit -- an existing room's
// second peer must always be allowed to join, since that never
// increases the room count.
func TestManager_MaxRoomsRejectsNewRoomsPastCap(t *testing.T) {
	mgr := NewManager(mockSessionFactory, WithMaxRooms(1))

	roomA := uniqueRoom("cap-a")
	callerA, _, err := mgr.Join(roomA, roleCaller, nil, nil)
	if err != nil {
		t.Fatalf("Join(roomA, caller): %v", err)
	}

	roomB := uniqueRoom("cap-b")
	if _, _, err := mgr.Join(roomB, roleCaller, nil, nil); err == nil {
		t.Fatal("Join for a second, brand-new room succeeded despite WithMaxRooms(1) already being at capacity")
	}

	// The second peer of the *existing* room must still be accepted --
	// the cap is on room count, not participant count.
	agentA, _, err := mgr.Join(roomA, roleAgent, nil, nil)
	if err != nil {
		t.Fatalf("Join(roomA, agent) was rejected even though it doesn't create a new room: %v", err)
	}

	if got := mgr.RoomCount(); got != 1 {
		t.Fatalf("RoomCount() = %d, want 1", got)
	}

	mgr.leave(roomA, roleCaller)
	mgr.leave(roomA, roleAgent)
	_ = callerA.close()
	_ = agentA.close()
	waitForRoomCount(t, mgr, 0, 2*time.Second)
}
