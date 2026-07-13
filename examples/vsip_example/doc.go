// Command vsip_example shows two complementary things about integrating a
// langstream.Session with an Exotel vSIP trunk call:
//
//  1. main.go's original VSIPCallAdapter demo: the Session-method-level
//     integration *contract/shape* -- which langstream.Session methods
//     (PushCallerAudio, PushAgentAudio, CallerHearsAudio, AgentHearsAudio)
//     a real integration calls and in what order -- using in-process
//     channels (see fakeAudioSource) standing in for RTP, so the contract
//     itself is easy to read without any socket/RTP noise around it.
//  2. real_rtp.go's runRealRTPDemo: the same kind of call wired to a real
//     rtp.DuplexSession (see pkg/rtp/duplex.go) over real loopback UDP
//     sockets and real RTP packets -- proving the actual RTP/ClearStream
//     integration this file originally (2026-07-08/09) flagged as not
//     yet implemented now works end to end, not just as a Go-level
//     method contract.
//
// Scope — read this before assuming more than is here:
//
//   - Neither demo implements Exotel vSIP trunk *signaling* (no SIP/SDP).
//     runRealRTPDemo's "caller sink"/"agent sink" loopback sockets stand
//     in for wherever a real vSIP trunk would actually terminate each
//     leg's RTP (an address a real integration would learn via SIP/SDP
//     negotiation instead); main.go's VSIPCallAdapter demo doesn't use
//     RTP or sockets at all, real or simulated.
//   - Both demos use mock ASR/MT/TTS backends (see pkg/langstream's
//     backend registry) rather than real vendors -- ROADMAP.md's Week 2
//     decision, and orthogonal to what either demo is actually
//     demonstrating (Session-method contract vs. real RTP/socket
//     plumbing, respectively). A real integration would select real
//     vendor backends the same way `langstream demo --backend NAME` does
//     (see cmd/langstream/main.go), and would replace runRealRTPDemo's
//     loopback sinks with the trunk's real remote RTP endpoints and its
//     simulated RTP sender with the trunk's actual inbound RTP stream.
//
// Run it with:
//
//	go run ./examples/vsip_example
package main
