// See doc.go for the package-level explanation of what this example does
// and, importantly, does not do.
package main

import (
	"context"
	"fmt"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/tts"
)

// VSIPCallAdapter demonstrates the *shape* of the integration between one
// leg of an Exotel vSIP trunk call and a langstream.Session. It is not a
// SIP/RTP client: it has no socket, no SDP/SIP signaling, and no RTP
// packet parsing. Its only job is to show, precisely, which
// langstream.Session methods a real integration would call and in which
// direction data flows:
//
//   - inbound audio (caller speaking, or agent speaking) -> PushCallerAudio
//     / PushAgentAudio
//   - outbound audio (translated speech to play back over the trunk) <-
//     CallerHearsAudio / AgentHearsAudio
//
// A real integration would replace the "in" channels below with audio
// decoded from RTP packets read off a live vSIP socket (after ClearStream's
// duplex-RTP integration supplies noise-suppressed PCM for the caller->ASR
// direction — see DEVLOG.md's 2026-07-08 entry for why that isn't
// available yet), and would replace the playback callbacks with code that
// re-encodes the PCM and writes it to the corresponding outbound RTP
// stream. Neither of those exists in this file; see doc.go.
type VSIPCallAdapter struct {
	sess *langstream.Session

	// sampleRate is stamped onto every asr.AudioFrame pushed into the
	// Session. A real adapter would set this from the codec negotiated
	// for the vSIP trunk leg (commonly 8000 for PSTN-originated audio).
	sampleRate int
}

// NewVSIPCallAdapter wraps sess (an already-running langstream.Session,
// see langstream.NewSession) with the plumbing a vSIP integration would
// use to move audio in and out of it.
func NewVSIPCallAdapter(sess *langstream.Session, sampleRate int) *VSIPCallAdapter {
	return &VSIPCallAdapter{sess: sess, sampleRate: sampleRate}
}

// PumpCallerAudio reads raw PCM frames from in — standing in for "frames
// just extracted from the vSIP trunk's inbound (caller-side) RTP stream" —
// and pushes each one into the Session's caller leg via PushCallerAudio.
// It returns when in is closed (all frames pushed successfully) or when
// ctx is cancelled or a push fails (whichever comes first), and reports
// how many frames were successfully pushed.
//
// In a real integration, in would be fed by an RTP receive loop extracting
// PCM payloads from inbound packets; here the caller constructs it however
// it likes (see main.go, which uses a simple buffered channel fed by a
// simulated audio source).
func (a *VSIPCallAdapter) PumpCallerAudio(ctx context.Context, in <-chan []byte) (pushed int, err error) {
	return a.pump(ctx, in, a.sess.PushCallerAudio)
}

// PumpAgentAudio is PumpCallerAudio's counterpart for the agent leg of the
// call (the far end of the vSIP trunk from the caller — e.g. the human
// agent or IVR the caller is speaking with).
func (a *VSIPCallAdapter) PumpAgentAudio(ctx context.Context, in <-chan []byte) (pushed int, err error) {
	return a.pump(ctx, in, a.sess.PushAgentAudio)
}

// pump is PumpCallerAudio and PumpAgentAudio's shared frame-push loop.
func (a *VSIPCallAdapter) pump(ctx context.Context, in <-chan []byte, push func(asr.AudioFrame) error) (pushed int, err error) {
	for {
		select {
		case <-ctx.Done():
			return pushed, ctx.Err()
		case pcm, ok := <-in:
			if !ok {
				return pushed, nil
			}
			frame := asr.AudioFrame{PCM: pcm, SampleRate: a.sampleRate}
			if err := push(frame); err != nil {
				return pushed, fmt.Errorf("pushing audio frame %d: %w", pushed, err)
			}
			pushed++
		}
	}
}

// PlaybackFunc is called once per synthesized audio chunk that should be
// sent back out over the call. A real integration would re-encode chunk.PCM
// (16-bit LE PCM at chunk.SampleRate) into the trunk's negotiated codec and
// write it to the appropriate outbound RTP stream; this example's
// PlaybackFunc (see main.go) just records/prints it.
type PlaybackFunc func(chunk tts.AudioChunk) error

// PumpCallerPlayback reads translated audio destined for the caller
// (Session.CallerHearsAudio — i.e. the agent's speech, translated into the
// caller's language) and invokes out for each chunk. It returns when the
// channel closes (Session.Close has fully shut the session down) or ctx is
// cancelled or out returns an error, and reports how many chunks were
// delivered.
func (a *VSIPCallAdapter) PumpCallerPlayback(ctx context.Context, out PlaybackFunc) (delivered int, err error) {
	return a.playback(ctx, a.sess.CallerHearsAudio(), out)
}

// PumpAgentPlayback is PumpCallerPlayback's counterpart for the audio the
// agent should hear (the caller's speech, translated into the agent's
// language, via Session.AgentHearsAudio).
func (a *VSIPCallAdapter) PumpAgentPlayback(ctx context.Context, out PlaybackFunc) (delivered int, err error) {
	return a.playback(ctx, a.sess.AgentHearsAudio(), out)
}

// playback is PumpCallerPlayback and PumpAgentPlayback's shared
// chunk-delivery loop.
func (a *VSIPCallAdapter) playback(ctx context.Context, in <-chan tts.AudioChunk, out PlaybackFunc) (delivered int, err error) {
	for {
		select {
		case <-ctx.Done():
			return delivered, ctx.Err()
		case chunk, ok := <-in:
			if !ok {
				return delivered, nil
			}
			if err := out(chunk); err != nil {
				return delivered, fmt.Errorf("delivering playback chunk %d: %w", delivered, err)
			}
			delivered++
		}
	}
}
