// Command vsip_example shows the shape of an Exotel vSIP trunk integration
// with a langstream.Session: what a real integration would call on the
// Session, and in what order, once a live call's audio is available.
//
// Scope — read this before assuming more than is here:
//
//   - This example does NOT implement Exotel vSIP trunk signaling, an RTP
//     socket, or PCM extraction from RTP packets. Inbound "caller" and
//     "agent" audio here is produced by a simulated fake source (see
//     fakeAudioSource in main.go) standing in for "audio already decoded
//     to 16-bit PCM by whatever sits between the vSIP trunk and this
//     process".
//   - It does NOT implement ClearStream's duplex-RTP integration. As of
//     2026-07-08/09 (see DEVLOG.md), ClearStream's pkg/rtp.Session exports
//     InjectBotAudio for the TTS-out direction but has no exported hook for
//     the caller->ASR direction — that needs an (unauthorized, not
//     attempted) ClearStream code change. This example is deliberately
//     agnostic to that: VSIPCallAdapter (see adapter.go) just needs
//     *some* source of decoded PCM frames and *some* sink for playback
//     chunks, whatever eventually supplies/consumes them.
//   - This is a contract/shape example, not an end-to-end wired
//     integration: it demonstrates which langstream.Session methods
//     (PushCallerAudio, PushAgentAudio, CallerHearsAudio, AgentHearsAudio)
//     a real vSIP integration would call and how their inputs/outputs
//     compose via VSIPCallAdapter, so Exotel's SIP team has a concrete
//     Go-level starting point once the duplex-RTP decision above is
//     resolved.
//
// Run it with:
//
//	go run ./examples/vsip_example
package main
