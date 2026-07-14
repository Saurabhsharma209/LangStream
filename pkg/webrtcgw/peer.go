package webrtcgw

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/tts"
)

// pcmaPayloadType is the static RTP payload type G.711 A-law uses per
// RFC 3551 -- fixed, never negotiated, which is part of why G.711 is such
// a simple codec to work with directly.
const pcmaPayloadType = 8

// audioFrameDuration is the size of each *outbound* audio sample this
// gateway writes to a browser's WebRTC track: 20ms, the same convention
// pkg/rtp/duplex.go uses for its ClearStream legs (see that package's
// PushAudio call sites) and the standard RTP packetization interval most
// WebRTC senders/receivers use for audio. This governs how translated TTS
// audio is paced going *out* to a browser.
const audioFrameDuration = 20 * time.Millisecond

// inboundBufferDuration (the *inbound* direction's counterpart) is
// defined in inbound_buffer.go, next to the inboundBuffer type it
// configures.

// sampleRate is fixed at 8kHz: G.711's own rate, and the same rate this
// repo's telephony/RTP code already assumes throughout (see
// cartesiaDefaultSampleRate's doc comment in pkg/tts).
const sampleRate = 8000

// newMediaEngine returns a *webrtc.MediaEngine that knows only PCMA
// (payload type 8) for audio -- see alaw.go's package doc comment for why
// this, not Opus, is what this gateway negotiates.
func newMediaEngine() (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: sampleRate,
			Channels:  1,
		},
		PayloadType: pcmaPayloadType,
	}
	if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("webrtcgw: registering PCMA codec: %w", err)
	}
	return m, nil
}

// peerRole distinguishes the two sides of a room. It controls which
// Session method a peer's inbound audio is pushed into and which
// Session channel its outbound track is fed from -- see room.go's
// wiring, and pkg/rtp/duplex.go's identical caller/agent split for
// ClearStream's telephony legs.
type peerRole int

const (
	roleCaller peerRole = iota
	roleAgent
)

func (r peerRole) String() string {
	if r == roleCaller {
		return "caller"
	}
	return "agent"
}

// pushAudioFunc matches langstream.Session's PushCallerAudio/PushAgentAudio
// signature, so a peer doesn't need to know which one it's calling.
type pushAudioFunc func(frame asr.AudioFrame) error

// peer wraps one browser participant's WebRTC connection: the inbound
// track (their mic, bridged into the shared Session) and the outbound
// track (translated audio synthesized for them to hear).
type peer struct {
	role peerRole
	pc   *webrtc.PeerConnection

	outboundTrack *webrtc.TrackLocalStaticSample

	mu           sync.Mutex
	pendingICE   []webrtc.ICECandidateInit
	remoteSet    bool
	onICEForward func(webrtc.ICECandidateInit)
}

// newPeer constructs a PeerConnection restricted to PCMA audio, with one
// sendrecv audio transceiver and its own outbound TrackLocalStaticSample
// already attached (so an offer/answer created from it already contains
// this gateway's send direction, not just an inbound-only "recvonly"
// slot).
func newPeer(role peerRole, iceServers []webrtc.ICEServer) (*peer, error) {
	mediaEngine, err := newMediaEngine()
	if err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, fmt.Errorf("webrtcgw: creating peer connection: %w", err)
	}

	outboundTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: sampleRate, Channels: 1},
		"audio", "langstream-"+role.String(),
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("webrtcgw: creating outbound track: %w", err)
	}
	if _, err := pc.AddTrack(outboundTrack); err != nil {
		pc.Close()
		return nil, fmt.Errorf("webrtcgw: adding outbound track: %w", err)
	}

	p := &peer{role: role, pc: pc, outboundTrack: outboundTrack}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.mu.Lock()
		forward := p.onICEForward
		p.mu.Unlock()
		if forward != nil {
			forward(c.ToJSON())
		}
	})

	return p, nil
}

// startInboundBridge reads this peer's inbound audio track (their mic,
// PCMA-encoded RTP) for as long as ctx is alive, decodes each packet to
// PCM16, and pushes it into the shared Session via push. It runs until
// the track ends (peer disconnects) or ctx is cancelled.
func (p *peer) startInboundBridge(ctx context.Context, push pushAudioFunc, onErr func(error)) {
	p.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			buf := newInboundBuffer(inboundBufferDuration, func(pcm []byte) {
				frame := asr.AudioFrame{PCM: pcm, SampleRate: sampleRate}
				if err := push(frame); err != nil {
					onErr(fmt.Errorf("webrtcgw: %s leg: pushing audio into session: %w", p.role, err))
				}
			})

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				packet, _, err := track.ReadRTP()
				if err != nil {
					if err != io.EOF && ctx.Err() == nil {
						onErr(fmt.Errorf("webrtcgw: %s leg: reading inbound RTP: %w", p.role, err))
					}
					buf.flush()
					return
				}
				if len(packet.Payload) == 0 {
					continue
				}
				buf.add(alawToPCM16(packet.Payload))
			}
		}()
	})
}

// startOutboundBridge reads tts.AudioChunk values from in (the Session's
// AgentHearsAudio()/CallerHearsAudio() channel, whichever this peer
// should hear) for as long as ctx is alive, encodes each to PCMA, and
// writes it to this peer's outbound track as a sequence of
// audioFrameDuration-sized samples -- a single AudioChunk from TTS is
// usually much larger than one 20ms RTP packet's worth of audio, so it is
// split into evenly-paced samples rather than sent as one oversized
// WriteSample call (which would desync WriteSample's internal RTP
// timestamp pacing from real wall-clock playback time).
func (p *peer) startOutboundBridge(ctx context.Context, in <-chan tts.AudioChunk, onErr func(error)) {
	go func() {
		const bytesPerFrame = int(sampleRate) * 2 / 1000 * int(audioFrameDuration/time.Millisecond) // PCM16 bytes per 20ms
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-in:
				if !ok {
					return
				}
				pcm := chunk.PCM
				for off := 0; off < len(pcm); off += bytesPerFrame {
					end := off + bytesPerFrame
					if end > len(pcm) {
						end = len(pcm)
					}
					alawFrame := pcm16ToALaw(pcm[off:end])
					sample := media.Sample{Data: alawFrame, Duration: audioFrameDuration}
					if err := p.outboundTrack.WriteSample(sample); err != nil {
						onErr(fmt.Errorf("webrtcgw: %s leg: writing outbound sample: %w", p.role, err))
						return
					}
				}
			}
		}
	}()
}

// handleOffer applies a remote SDP offer (from the browser) and returns
// this peer's SDP answer. Any ICE candidates that arrived before the
// offer (buffered in pendingICE, since trickle ICE from the browser can
// race the offer/answer exchange over an unordered transport like a
// WebSocket message queue processed slightly out of order) are applied
// immediately afterward.
func (p *peer) handleOffer(sdp string) (string, error) {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("webrtcgw: %s leg: SetRemoteDescription: %w", p.role, err)
	}

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("webrtcgw: %s leg: CreateAnswer: %w", p.role, err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("webrtcgw: %s leg: SetLocalDescription: %w", p.role, err)
	}

	p.mu.Lock()
	p.remoteSet = true
	pending := p.pendingICE
	p.pendingICE = nil
	p.mu.Unlock()
	for _, c := range pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			return "", fmt.Errorf("webrtcgw: %s leg: applying buffered ICE candidate: %w", p.role, err)
		}
	}

	return answer.SDP, nil
}

// addICECandidate applies a trickled remote ICE candidate, buffering it
// if the offer/answer exchange (which AddICECandidate requires to have
// already completed) hasn't happened yet.
func (p *peer) addICECandidate(c webrtc.ICECandidateInit) error {
	p.mu.Lock()
	if !p.remoteSet {
		p.pendingICE = append(p.pendingICE, c)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.pc.AddICECandidate(c)
}

// setICEForward registers the callback used to forward this gateway's own
// local ICE candidates back to the browser over the signaling channel.
func (p *peer) setICEForward(fn func(webrtc.ICECandidateInit)) {
	p.mu.Lock()
	p.onICEForward = fn
	p.mu.Unlock()
}

func (p *peer) close() error {
	return p.pc.Close()
}
