// Package rtp will hold LangStream's duplex RTP session handling: two
// synchronized media legs (caller, agent) with jitter buffering, extending
// ClearStream's (github.com/Saurabhsharma209/ClearStream) pkg/rtp.Session
// model - which handles a single leg - rather than reimplementing RTP
// packetization, sequencing, and jitter buffering from scratch.
//
// This package is intentionally a skeleton for Week 1. Per ROADMAP.md,
// "Extend ClearStream's pkg/rtp session for bidirectional media" is
// explicitly called out as the highest-risk item for Week 2, budgeted
// extra time there rather than rushed this week. Week 1's duplex
// orchestrator (pkg/langstream) is deliberately transport-agnostic: it
// consumes and produces asr.AudioFrame / tts.AudioChunk PCM directly via
// Session.PushCallerAudio/PushAgentAudio and
// Session.CallerHearsAudio/AgentHearsAudio, so it can be exercised fully
// against mocks today and wired to a real duplex RTP transport here in
// Week 2 without changing the orchestrator's public API.
//
// Planned Week 2 shape (not yet implemented):
//   - a DuplexSession type composing two ClearStream-style single-leg
//     rtp.Session instances (or a shared jitter-buffered demux) bound to
//     the two RTP streams of a call
//   - PCM framing glue between DuplexSession and langstream.Session's
//     Push*Audio/*HearsAudio methods
//   - SRTP/telephony codec (G.711/G.722) negotiation reused from
//     ClearStream where possible
package rtp
