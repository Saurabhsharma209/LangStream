// idle_room_qa_test.go is QA's independent, adversarial verification of
// this package's 2026-07-15 idle-room-timeout hardening (see room.go's
// DefaultRoomIdleTimeout/WithIdleTimeout and room_test.go, both added by
// the same day's Tech workstream). QA's charter is not to just trust
// Tech's own tests: room_test.go already asserts on RoomCount(), the lone
// peer's PeerConnection reaching Closed, and the Session's
// AgentHearsAudio channel closing -- all good signals, but none of them
// directly measure whether the goroutines that idle timeout is supposed
// to reclaim (peer.go's startInboundBridge/startOutboundBridge for the
// lone peer, plus whatever the underlying langstream.Session and pion
// PeerConnection spin up) have actually exited, as opposed to merely
// being dereferenced from the Manager's bookkeeping while still running
// in the background. This file adds that independent signal directly via
// runtime.NumGoroutine().
package webrtcgw

import (
	"runtime"
	"testing"
	"time"
)

// TestQA_IdleRoomExpiry_DoesNotLeakGoroutines churns through several
// single-peer rooms that are each deliberately abandoned (the second
// participant never joins, exactly the "dead link/browser crash/
// abandoned tab" scenario DefaultRoomIdleTimeout's doc comment
// describes), lets the configured idle timeout expire every one of them,
// and asserts the process's total goroutine count returns to
// (approximately) its pre-churn baseline afterward -- not just that
// Manager.RoomCount() drops to zero, which only proves the map entry was
// deleted, not that every goroutine that room's peer/session spun up
// actually returned.
func TestQA_IdleRoomExpiry_DoesNotLeakGoroutines(t *testing.T) {
	const idleTimeout = 50 * time.Millisecond
	const rooms = 8

	// Let any goroutines left over from earlier tests in this package's
	// shared test binary settle before taking a baseline, so this test
	// measures its own churn's contribution, not ambient noise from
	// tests that happened to run immediately before it.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	mgr := NewManager(mockSessionFactory, WithIdleTimeout(idleTimeout))
	for i := 0; i < rooms; i++ {
		room := uniqueRoom("qa-goroutine-leak")
		if _, _, err := mgr.Join(room, roleCaller, nil, nil); err != nil {
			t.Fatalf("Join (room %d): %v", i, err)
		}
	}

	if got := mgr.RoomCount(); got != rooms {
		t.Fatalf("RoomCount() = %d immediately after joining %d single-peer rooms, want %d", got, rooms, rooms)
	}

	// All rooms above are single-peer and share idleTimeout, so all
	// should expire and be removed within the same generous window
	// room_test.go's own tests budget for.
	waitForRoomCount(t, mgr, 0, 5*time.Second)

	// Give the now-cancelled contexts and force-closed PeerConnections a
	// settle window past the RoomCount()==0 signal: a goroutine blocked
	// in track.ReadRTP() or a channel send doesn't necessarily return the
	// exact instant its PeerConnection's Close() call returns.
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// Allow slack rather than exact equality: background goroutines
	// (GC workers, finalizers, the Go runtime's own timers) can
	// legitimately vary run to run. What this guards against is *growth
	// proportional to `rooms`* -- a real per-room leak -- not scheduler
	// noise.
	const slack = 5
	if after > before+slack {
		t.Errorf("goroutine count grew from %d to %d after %d abandoned single-peer rooms were created and their idle timeouts expired (allowed slack %d) -- possible goroutine leak in idle-room cleanup (peer.go's startInboundBridge/startOutboundBridge, or the underlying langstream.Session/PeerConnection, not fully exiting on expiry)", before, after, rooms, slack)
	}
}

// TestQA_IdleRoomExpiry_RepeatedChurnStaysBounded is a stronger variant of
// the above: instead of one batch of `rooms` abandoned rooms, it runs
// several sequential batches and asserts the goroutine count after each
// batch stays within the same bounded envelope rather than trending
// upward -- a slow, cumulative leak (e.g. one goroutine leaked per
// expired room) could hide within a single batch's slack budget but would
// show up as steady growth across repeated batches.
func TestQA_IdleRoomExpiry_RepeatedChurnStaysBounded(t *testing.T) {
	const idleTimeout = 40 * time.Millisecond
	const roomsPerBatch = 5
	const batches = 4

	mgr := NewManager(mockSessionFactory, WithIdleTimeout(idleTimeout))

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var samples []int
	for b := 0; b < batches; b++ {
		for i := 0; i < roomsPerBatch; i++ {
			room := uniqueRoom("qa-goroutine-churn")
			if _, _, err := mgr.Join(room, roleCaller, nil, nil); err != nil {
				t.Fatalf("batch %d: Join (room %d): %v", b, i, err)
			}
		}
		waitForRoomCount(t, mgr, 0, 5*time.Second)

		runtime.GC()
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		samples = append(samples, runtime.NumGoroutine())
	}

	const slack = 8
	for b, count := range samples {
		if count > baseline+slack {
			t.Errorf("batch %d: goroutine count = %d, baseline = %d (allowed slack %d) -- samples across all batches: %v -- trending growth would indicate a cumulative per-room goroutine leak", b, count, baseline, slack, samples)
		}
	}
}
