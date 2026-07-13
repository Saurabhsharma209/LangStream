package main

import "testing"

// TestRunRealRTPDemo_EndToEnd is Tech's own basic coverage for the real
// rtp.DuplexSession-backed demo added in real_rtp.go: it just runs the
// whole demo (real loopback UDP sockets, real RTP packets in, real RTP
// packets carrying synthesized bot audio out) and confirms it completes
// without error, so a future change to this file or to
// pkg/rtp.DuplexSession that broke the wiring would fail CI instead of
// only being caught by a human running `go run ./examples/vsip_example`
// and eyeballing the output.
func TestRunRealRTPDemo_EndToEnd(t *testing.T) {
	if err := runRealRTPDemo(); err != nil {
		t.Fatalf("runRealRTPDemo: %v", err)
	}
}

// TestRunRealRTPDemo_RunsTwiceWithoutPortConflicts runs the demo twice in
// a row, confirming each run's loopback address selection (learnLoopbackAddr)
// and DuplexSession construction/teardown don't leak sockets or otherwise
// interfere with a subsequent run -- a basic sanity check for anyone
// invoking this repeatedly (e.g. from a shell loop while manually
// exercising the example).
func TestRunRealRTPDemo_RunsTwiceWithoutPortConflicts(t *testing.T) {
	for i := 0; i < 2; i++ {
		if err := runRealRTPDemo(); err != nil {
			t.Fatalf("runRealRTPDemo run %d: %v", i, err)
		}
	}
}
