// Week 2 implementation of the duplex RTP shape sketched in doc.go: see
// that file's "Planned Week 2 shape" section for the original plan and
// COMBINED_ROADMAP.md / VERSIONING.md for why this was blocked for 5
// sprints (ClearStream had no way to hand LangStream clean caller audio)
// and how that got resolved (ClearStream's Session.CleanAudio()).
//
// DuplexSession, below, is the implementation: it composes two
// ClearStream rtp.Session instances (one per call leg) with a
// *langstream.Session, bridging PCM in both directions so the two
// projects' RTP handling and ASR/MT/TTS orchestration work together
// without either reimplementing the other's job.
package rtp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/exotel/clearstream/pkg/model"
	clearstream "github.com/exotel/clearstream/pkg/rtp"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/tts"
)

// defaultCleanAudioBufferSize is the default size (in frames) of the
// buffered channel ClearStream allocates for each leg's CleanAudio()
// feed (see clearstream.Config.CleanAudioBufferSize). It matches
// pkg/langstream/session.go's outboundBuffer constant: both exist for the
// same reason (absorb brief consumer stalls -- here, a momentarily slow
// asr.StreamSession.PushAudio call -- without blocking the producer), so
// there's no reason for the two numbers to differ absent a measured
// reason to tune one independently.
const defaultCleanAudioBufferSize = 32

// cleanAudioSampleRate is the sample rate of every frame ClearStream
// delivers via CleanAudio(): its doc comment guarantees "16kHz mono int16
// PCM" regardless of the negotiated telephony codec's own sample rate
// (ClearStream upsamples internally), so this is a fixed constant, not
// something resolved per leg config.
const cleanAudioSampleRate = 16000

// duplexStopTimeout bounds how long Stop waits for the six bridging
// goroutines to exit before giving up, mirroring the "bounded wait,
// cancel as backstop" pattern langstream.Session.Close uses via its own
// finalFlushTimeout constant (see session.go's doc comment on Close). It
// doesn't need to match that value -- mocks and loopback UDP both settle
// in milliseconds -- it's generous on purpose so it (ideally) never fires.
const duplexStopTimeout = 3 * time.Second

// DefaultTTSPacingTargetDelay is the JitterBuffer TargetDelay
// DuplexSession uses by default when repurposing JitterBuffer as an
// outbound TTS-pacing buffer instead of an inbound network-jitter buffer
// (see jitter.go's package doc comment for the full repurposing
// rationale, and PullLost's doc comment for what a "lost" packet means in
// this new context: a synthesized chunk that took unusually long to
// arrive got dropped to keep pacing/latency bounded, not a real network
// loss). It intentionally differs from jitter.go's own DefaultTargetDelay
// (60ms, tuned for inbound arrival-jitter absorption): here it bounds how
// long DuplexSession lets synthesized speech sit in the pacing buffer
// absorbing bursty per-chunk TTS generation latency before InjectBotAudio
// -- big enough to smooth typical chunk-to-chunk burstiness, small enough
// to not add noticeable extra latency on top of whatever the TTS backend
// itself already takes. 40ms (two DefaultTTSPacingInterval ticks) is a
// starting point, not a measured value; tuning it against real vendor TTS
// latency distributions is a natural follow-up once DuplexSession carries
// real traffic, mirroring jitter.go's own "groundwork now, tune against
// real conditions later" framing.
const DefaultTTSPacingTargetDelay = 40 * time.Millisecond

// DefaultTTSPacingInterval is the JitterBuffer PacketInterval
// DuplexSession uses by default for outbound TTS pacing: both the nominal
// spacing the pacing buffer schedules successive chunks' release at, and
// the tick interval runTTSPacer polls it on. It matches ClearStream's own
// downstream playback-loop tick (see clearstream/pkg/rtp/playback.go's
// startPlaybackLoop, which pops one 20ms-of-audio frame off its own
// PlaybackQueue every 20ms) so the two pacing stages line up -- though,
// unlike that fixed-duration-frame loop, each "packet" paced here is one
// whole tts.AudioChunk (see ttsChunkPacket), which may represent more or
// less than 20ms of audio; see feedTTSPacer/runTTSPacer's doc comments.
const DefaultTTSPacingInterval = 20 * time.Millisecond

// ttsPacer bundles a JitterBuffer with an atomic count of how many chunks
// feedTTSPacer has ever pushed into it. JitterBuffer's Pull/deadline math
// (built for network jitter, see jitter.go's package doc comment) has no
// notion of "no more packets are, or ever will be, coming" -- a missing
// network packet is unpredictably either just late or truly gone, never
// provably finished -- so without an explicit bound, once every real
// chunk had been resolved (delivered or declared lost), runTTSPacer
// ticking Pull() forever afterward would keep advancing past sequence
// numbers nothing will ever fill, logging a spurious "dropped chunk"
// warning on every single tick for the rest of DuplexSession's life
// (harmless to correctness, but log spam and wasted work). pushed gives
// runTTSPacer that bound: it only calls Pull while it has not yet
// resolved every chunk feedTTSPacer has actually pushed so far.
type ttsPacer struct {
	buf    *JitterBuffer
	pushed atomic.Uint64 // count of chunks feedTTSPacer has ever pushed
}

// newTTSPacer returns a ready-to-use ttsPacer backed by a JitterBuffer
// constructed from cfg.
func newTTSPacer(cfg Config) *ttsPacer {
	return &ttsPacer{buf: NewJitterBuffer(cfg)}
}

// LegConfig configures one ClearStream-backed RTP leg (caller-side or
// agent-side) of a DuplexSession. It is a thin wrapper around the subset
// of clearstream.Config a duplex caller actually needs to set per leg;
// fields ClearStream documents as "set by ClearStream, do not set
// manually" (SampleRate) or genuinely not relevant to this task (AGC,
// OnDTMF, Telemetry, DTMFPayloadType, FFmpegPath, OnStats, ...) are
// intentionally not exposed here to keep the shape a duplex caller has to
// reason about small; a future caller that needs one of those can extend
// LegConfig or construct clearstream.Config directly.
type LegConfig struct {
	// ListenAddr is the UDP address this leg receives RTP on (e.g.
	// "0.0.0.0:5004", or "127.0.0.1:0" in tests to get an OS-assigned
	// port).
	ListenAddr string
	// ForwardAddr is the UDP address this leg forwards clean/injected RTP
	// to -- for the caller leg, wherever caller-side clean audio (and,
	// via InjectBotAudio, translated bot audio for the caller to hear)
	// needs to go on the telephony side; for the agent leg, the agent's
	// corresponding endpoint.
	ForwardAddr string
	// ForwardAddrs is an optional set of additional fan-out destinations;
	// see clearstream.Config.ForwardAddrs.
	ForwardAddrs []string
	// PayloadType is the RTP payload type this leg's telephony side uses
	// (e.g. 0 for PCMU).
	PayloadType uint8
	// JitterDepth configures ClearStream's own single-leg jitter buffer
	// (distinct from this package's JitterBuffer in jitter.go, which is
	// transport-agnostic groundwork not yet wired to a real transport --
	// see jitter.go's doc comment). 0 uses ClearStream's own default.
	JitterDepth int
	// Suppressor is this leg's noise suppressor (e.g.
	// model.NewMockSuppressor() in tests; a real constructor in
	// production -- see clearstream.Config.Suppressor's doc comment).
	// Must not be nil.
	Suppressor model.Suppressor
	// Logger is this leg's ClearStream session logger. Defaults to
	// zap.NewNop() if nil -- ClearStream's Session unconditionally logs
	// through it (e.g. in Start/Stop), so it must never be left nil when
	// building the underlying clearstream.Config.
	Logger *zap.Logger
	// CleanAudioBufferSize overrides defaultCleanAudioBufferSize when > 0.
	// Callers should not normally need to set this; it exists for tuning
	// if a future measured workload needs a bigger or smaller buffer than
	// the shared default.
	CleanAudioBufferSize int
}

// clearStreamConfig builds the clearstream.Config this leg's Session is
// constructed from, filling in the defaults DuplexSession depends on: a
// zero CleanAudioBufferSize would silently disable the CleanAudio() feed
// the bridging goroutines require (see NewDuplexSession), and a nil
// Logger would panic the first time ClearStream's Session logs anything.
func (c LegConfig) clearStreamConfig() clearstream.Config {
	bufSize := c.CleanAudioBufferSize
	if bufSize <= 0 {
		bufSize = defaultCleanAudioBufferSize
	}
	logger := c.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return clearstream.Config{
		ListenAddr:           c.ListenAddr,
		ForwardAddr:          c.ForwardAddr,
		ForwardAddrs:         c.ForwardAddrs,
		PayloadType:          c.PayloadType,
		JitterDepth:          c.JitterDepth,
		Suppressor:           c.Suppressor,
		Logger:               logger,
		CleanAudioBufferSize: bufSize,
	}
}

// DuplexConfig configures a DuplexSession: two ClearStream-backed RTP legs
// (caller, agent) bridged through an already-constructed
// *langstream.Session.
type DuplexConfig struct {
	// CallerLeg is the caller-facing RTP leg: caller audio in, translated
	// agent speech (CallerHearsAudio) out.
	CallerLeg LegConfig
	// AgentLeg is the agent-facing RTP leg: agent audio in, translated
	// caller speech (AgentHearsAudio) out.
	AgentLeg LegConfig
	// Session is the duplex translation orchestrator this DuplexSession
	// bridges PCM to/from. It must already be constructed (see
	// langstream.NewSession) before calling NewDuplexSession, and its
	// lifecycle is NOT owned by DuplexSession: Stop does not Close it.
	// This is deliberate -- Session may be constructed, monitored (e.g.
	// CallerLegDegraded/Metrics), and eventually closed by call-control
	// code that has its own reasons to outlive or outlast the RTP
	// transport layer (e.g. a SIP BYE arriving independently of either
	// leg's UDP socket state). Callers that do want "closing the duplex
	// RTP session also ends the translation session" should call
	// Session.Close() themselves alongside DuplexSession.Stop().
	Session *langstream.Session
	// Logger is used for DuplexSession's own bridging-layer logging
	// (e.g. a dropped InjectBotAudio push). Defaults to zap.NewNop() if
	// nil. Distinct from each LegConfig's own Logger, which is ClearStream
	// Session-scoped.
	Logger *zap.Logger

	// TTSPacing configures the JitterBuffer DuplexSession uses, one
	// instance per leg, to smooth bursty TTS synthesis before
	// InjectBotAudio (see jitter.go's package doc comment for why
	// JitterBuffer -- originally built for inbound network-jitter
	// absorption -- was repurposed for this instead). Zero-value fields
	// default to DefaultTTSPacingTargetDelay / DefaultTTSPacingInterval /
	// DefaultMaxPacketsBuffered; most callers should leave this unset.
	TTSPacing Config
}

// ttsPacingConfig returns the Config each leg's TTS-pacing JitterBuffer is
// constructed from: cfg.TTSPacing with DefaultTTSPacingTargetDelay/
// DefaultTTSPacingInterval filled in for any zero-value field (Config's
// own withDefaults then fills MaxPacketsBuffered from
// DefaultMaxPacketsBuffered if that is also left zero -- there is no
// TTS-specific default for that field, the generic one applies as-is).
func (cfg DuplexConfig) ttsPacingConfig() Config {
	c := cfg.TTSPacing
	if c.TargetDelay <= 0 {
		c.TargetDelay = DefaultTTSPacingTargetDelay
	}
	if c.PacketInterval <= 0 {
		c.PacketInterval = DefaultTTSPacingInterval
	}
	return c.withDefaults()
}

// validate checks that cfg is complete enough to build a DuplexSession.
// It intentionally does not duplicate clearstream.NewSession's own
// ListenAddr/ForwardAddr validation (that still runs, and still errors,
// inside NewDuplexSession) -- this only guards the one precondition
// ClearStream can't check for us: that there is actually a Session to
// bridge to.
func (cfg DuplexConfig) validate() error {
	if cfg.Session == nil {
		return errors.New("rtp: DuplexConfig.Session must not be nil")
	}
	return nil
}

// DuplexSession bridges two ClearStream rtp.Session instances (one per
// call leg) to a *langstream.Session, so a real (or loopback, in tests)
// RTP transport can drive LangStream's ASR->MT->TTS orchestrator without
// either project reimplementing the other's job:
//
//   - caller leg CleanAudio() -> asr.AudioFrame -> Session.PushCallerAudio
//   - agent leg  CleanAudio() -> asr.AudioFrame -> Session.PushAgentAudio
//   - Session.AgentHearsAudio()  -> agent-leg TTS pacing buffer -> agent  leg InjectBotAudio
//   - Session.CallerHearsAudio() -> caller-leg TTS pacing buffer -> caller leg InjectBotAudio
//
// The TTS-out direction is paced through a per-leg JitterBuffer (see
// jitter.go's package doc comment for why: JitterBuffer was originally
// built for inbound network-jitter absorption, and is repurposed here as
// an outbound smoothing stage for bursty TTS synthesis instead, now that
// ClearStream jitter-buffers each leg's *inbound* audio internally before
// ever handing it to CleanAudio()) via feedTTSPacer/runTTSPacer, rather
// than calling InjectBotAudio directly off Session.AgentHearsAudio()/
// CallerHearsAudio().
//
// Construct with NewDuplexSession, start with Start, and always Stop it
// exactly once done (Stop is idempotent and safe to call more than once,
// mirroring langstream.Session.Close's contract).
type DuplexSession struct {
	cfg DuplexConfig

	callerSess *clearstream.Session
	agentSess  *clearstream.Session
	session    *langstream.Session

	// callerPacer/agentPacer are the per-leg outbound TTS-pacing buffers
	// feedTTSPacer/runTTSPacer bridge Session.CallerHearsAudio()/
	// AgentHearsAudio() through before InjectBotAudio -- see jitter.go's
	// package doc comment and DuplexSession's own doc comment for why.
	callerPacer *ttsPacer
	agentPacer  *ttsPacer

	logger *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	// lifecycleMu guards startedFlag/stoppedFlag and serializes Start/Stop
	// against each other -- it does not stay held for the full duration
	// of either method's slower work (starting/stopping the ClearStream
	// sessions, waiting out wg), only long enough to atomically
	// check-and-set the relevant flag(s).
	//
	// This replaces an earlier design (independent atomic.Bool +
	// sync.Once per method) that QA's integration testing (2026-07-12)
	// caught as a genuine, race-detector-confirmed bug: Start's
	// wg.Add(4) and Stop's wg.Wait() had no happens-before edge between
	// them, so a Stop() call that won the race to reach wg.Wait() before
	// a concurrent Start() reached wg.Add(4) was exactly the "Add with
	// positive delta racing Wait while the counter may still be zero"
	// pattern sync.WaitGroup's own doc comment calls out as a data race.
	// Reproduced via go test -race, roughly 1-in-5 to 1-in-6 runs of a
	// Start()/Stop() called concurrently from separate goroutines.
	//
	// The fix: Start holds lifecycleMu for its *entire* body (including
	// wg.Add(4) and starting both ClearStream sessions) before any
	// concurrent Stop can proceed past its own flag check, so by the
	// time Stop() observes startedFlag, either Start() has fully
	// finished (wg.Add already happened-before, safe to Wait) or Start()
	// never got to run at all (stoppedFlag already true, Start bails out
	// before touching wg or either ClearStream session).
	lifecycleMu sync.Mutex
	startedFlag bool
	stoppedFlag bool

	// stopOnce guarantees Stop's full body (ClearStream shutdown +
	// wg.Wait) executes exactly once, and that every concurrent caller
	// of Stop -- not just the first -- blocks until that single
	// execution finishes before reading stopErr, so stopErr itself is
	// never read while it might still be concurrently written. lifecycleMu
	// above is a separate, narrower lock: it only protects the
	// Start()/Stop() *flag* check-and-set against each other (the actual
	// race QA found), not Stop's full idempotent-return contract, which
	// is what stopOnce is still for.
	stopOnce sync.Once
	stopErr  error
}

// NewDuplexSession validates cfg and constructs (but does not Start) both
// ClearStream leg sessions. This mirrors clearstream.NewSession's own
// construct-then-start split (NewSession binds the UDP socket; Start
// launches the background goroutines) rather than langstream.NewSession's
// construct-and-implicitly-start shape, because the two ClearStream
// Sessions' UDP sockets are already live (bound) the moment NewSession
// returns -- deferring only the goroutines (and the bridging goroutines
// this package adds on top) to a separate Start gives a caller a point to
// wire up anything else session-related (e.g. registering OnStats/OnDTMF
// callbacks directly on the underlying Sessions, if a future caller needs
// that) before any packet is actually processed.
func NewDuplexSession(cfg DuplexConfig) (*DuplexSession, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	callerSess, err := clearstream.NewSession(cfg.CallerLeg.clearStreamConfig())
	if err != nil {
		return nil, fmt.Errorf("rtp: constructing caller leg ClearStream session: %w", err)
	}

	agentSess, err := clearstream.NewSession(cfg.AgentLeg.clearStreamConfig())
	if err != nil {
		// callerSess's UDP socket is already bound (clearstream.NewSession
		// calls net.ListenUDP before returning). ClearStream's own
		// Session.Stop() cannot be safely called on a Session that was
		// never Start()ed -- it blocks forever on an internal readiness
		// channel that only Start()'s listenRTCP goroutine ever closes --
		// and ClearStream exposes no other way to release the bound
		// socket. Starting it and immediately stopping it again is the
		// only way to release that socket cleanly with today's
		// ClearStream API. Flagging this as a small upstream API gap
		// worth raising with ClearStream (e.g. a Close()-without-Start(),
		// or deferring ListenUDP to Start()) rather than working around
		// it more invasively here.
		callerSess.Start()
		callerSess.Stop()
		return nil, fmt.Errorf("rtp: constructing agent leg ClearStream session: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &DuplexSession{
		cfg:         cfg,
		callerSess:  callerSess,
		agentSess:   agentSess,
		session:     cfg.Session,
		callerPacer: newTTSPacer(cfg.ttsPacingConfig()),
		agentPacer:  newTTSPacer(cfg.ttsPacingConfig()),
		logger:      logger,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Start starts both underlying ClearStream sessions and the six bridging
// goroutines described in DuplexSession's doc comment (two CleanAudio()
// producers, and, per leg, a TTS-pacer feed goroutine plus the pacer's own
// release goroutine). It is idempotent: calling it more than once has no
// additional effect beyond the first call.
func (d *DuplexSession) Start() {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.startedFlag || d.stoppedFlag {
		// Already started (idempotent no-op, matching the previous
		// sync.Once-based contract), or Stop() already won the race and
		// this DuplexSession is shutting down/shut down -- in the
		// latter case, proceeding would call wg.Add(6) with no
		// guarantee Stop's wg.Wait() hasn't already been reached. See
		// lifecycleMu's doc comment.
		return
	}
	d.startedFlag = true

	d.callerSess.Start()
	d.agentSess.Start()

	d.wg.Add(6)
	go d.bridgeCleanAudio(d.callerSess, d.session.PushCallerAudio, "caller")
	go d.bridgeCleanAudio(d.agentSess, d.session.PushAgentAudio, "agent")
	go d.feedTTSPacer(d.session.AgentHearsAudio(), d.agentPacer, "agent")
	go d.feedTTSPacer(d.session.CallerHearsAudio(), d.callerPacer, "caller")
	go d.runTTSPacer(d.agentPacer, d.agentSess.InjectBotAudio, "agent")
	go d.runTTSPacer(d.callerPacer, d.callerSess.InjectBotAudio, "caller")
}

// Stop stops both underlying ClearStream sessions and waits (up to
// duplexStopTimeout) for the six bridging goroutines to exit. It is
// idempotent and safe to call more than once or from multiple goroutines
// concurrently; only the first call does the work, and every caller
// observes the same result. Stop does NOT close DuplexConfig.Session --
// see that field's doc comment for why.
func (d *DuplexSession) Stop() error {
	d.stopOnce.Do(func() {
		d.lifecycleMu.Lock()
		d.stoppedFlag = true
		started := d.startedFlag
		d.lifecycleMu.Unlock()

		d.stopLocked(started)
	})
	return d.stopErr
}

// stopLocked is Stop's actual body, run exactly once (via d.stopOnce)
// after lifecycleMu has already been used to set stoppedFlag and read
// startedFlag. Splitting it out keeps Stop itself short and makes the
// stopOnce/lifecycleMu relationship (Once for idempotent-return-to-every-
// caller, mutex for the narrower Start/Stop flag race) easy to see in one
// place.
func (d *DuplexSession) stopLocked(started bool) {
	if started {
		// Stop the ClearStream sessions first, before cancelling d.ctx.
		// Each call blocks until that session's own goroutines
		// (including its CleanAudio() producer) have fully exited,
		// which closes both CleanAudio() channels as a side effect --
		// so bridgeCleanAudio below observes a clean channel close
		// instead of racing context cancellation. This mirrors
		// langstream.Session.Close's own documented ordering (stop
		// producers before cancelling the shared context) and for the
		// same reason: a race here was exactly the Day-1 bug that
		// ordering note describes, for a different pair of channels.
		//
		// By the time `started` is read as true above, Start()'s own
		// wg.Add(4) has already happened-before (lifecycleMu forces
		// Start() to either fully finish -- including wg.Add(4) -- or
		// never run at all before Stop() can observe startedFlag=true),
		// so it's always safe to Wait() below.
		d.callerSess.Stop()
		d.agentSess.Stop()
	}
	// If Start was never called (started is false here), neither
	// ClearStream session's goroutines (nor this package's six bridging
	// goroutines) were ever started, so there is nothing above to stop
	// and nothing below to wait for -- but note the two Sessions' UDP
	// sockets (bound by NewDuplexSession/clearstream.NewSession) are
	// still open in that case; see NewDuplexSession's doc comment on the
	// same underlying ClearStream API gap.

	// Cancel unconditionally as a backstop: bridgeHears reads from
	// DuplexConfig.Session's AgentHearsAudio()/CallerHearsAudio(),
	// channels this DuplexSession does not own and that ClearStream
	// stopping above has no effect on (that Session may outlive this
	// DuplexSession, or be closed independently by call-control code).
	// Context cancellation is the only thing that guarantees
	// bridgeHears's two goroutines exit even if Session is never closed.
	d.cancel()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(duplexStopTimeout):
		d.stopErr = fmt.Errorf("rtp: DuplexSession.Stop: timed out after %s waiting for bridging goroutines to exit", duplexStopTimeout)
	}
}

// bridgeCleanAudio is the caller-leg/agent-leg CleanAudio() -> Push*Audio
// bridging goroutine: it reads clean, post-suppression PCM frames off
// leg's CleanAudio() channel, converts them to an asr.AudioFrame, and
// hands them to push (Session.PushCallerAudio or Session.PushAgentAudio).
// It exits when leg's CleanAudio() channel closes (leg.Stop() was called)
// or d.ctx is cancelled (Stop's unconditional backstop), whichever comes
// first -- it never blocks forever on either, and never ranges over the
// channel unconditionally (which would leak this goroutine if leg's
// CleanAudioBufferSize were ever 0 and CleanAudio() returned nil: ranging
// over a nil channel blocks forever, but a select naming both the channel
// receive and ctx.Done() does not, so d.cancel() alone is still enough to
// unblock this goroutine even in that case).
func (d *DuplexSession) bridgeCleanAudio(leg *clearstream.Session, push func(asr.AudioFrame) error, legName string) {
	defer d.wg.Done()

	ch := leg.CleanAudio()
	for {
		select {
		case <-d.ctx.Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			// TimestampMS: ClearStream's CleanAudioFrame.Timestamp is the
			// *RTP* timestamp of the source packet -- a per-SSRC counter
			// that starts at a random offset (RFC 3550) and advances at
			// the source codec's own clock rate (e.g. 8000/s for PCMU),
			// not epoch time. Reinterpreting it directly as epoch
			// milliseconds would produce meaningless, non-monotonic
			// values across SSRC changes/session restarts. Wall-clock
			// arrival time is simpler, always meaningful, and good
			// enough for asr.AudioFrame.TimestampMS's actual use today
			// (informational/logging -- neither MockRecognizer nor any
			// other current asr.StreamSession implementation derives
			// timing decisions from it); a future vendor integration
			// that needs sample-accurate source-clock timestamps should
			// carry frame.Timestamp through some other channel rather
			// than overloading this field.
			af := asr.AudioFrame{
				PCM:         int16PCMToBytes(frame.PCM),
				SampleRate:  cleanAudioSampleRate,
				TimestampMS: time.Now().UnixMilli(),
			}
			if err := push(af); err != nil {
				d.logger.Warn("langstream/rtp: pushing clean audio failed",
					zap.String("leg", legName), zap.Error(err))
			}
		}
	}
}

// feedTTSPacer is the producer half of DuplexSession's outbound
// TTS-pacing bridge (see runTTSPacer for the consumer half that actually
// calls InjectBotAudio, and jitter.go's package doc comment for why a
// JitterBuffer -- originally built for inbound network-jitter absorption
// -- is repurposed here instead). It reads synthesized tts.AudioChunks off
// in (Session.AgentHearsAudio() or Session.CallerHearsAudio()) as ASR->MT->
// TTS actually produces them -- however bursty that timing is -- and
// pushes each into pacer tagged with a strictly increasing SeqNum. There
// is no reordering for pacer to do here (in is a single Go channel, so
// chunks already arrive in synthesis order); the only thing pacer adds is
// the release-timing smoothing runTTSPacer applies on the way out.
//
// It exits when in closes (Session was closed by whoever owns its
// lifecycle) or d.ctx is cancelled (Stop's backstop), whichever comes
// first. Note that pacer may still hold un-released chunks when this
// returns -- that's fine: runTTSPacer keeps draining pacer on its own
// ticker independently of whether the feed side has exited, so a final
// chunk pushed just before in closes is still released and injected.
func (d *DuplexSession) feedTTSPacer(in <-chan tts.AudioChunk, pacer *ttsPacer, legName string) {
	defer d.wg.Done()

	var seq uint16
	for {
		select {
		case <-d.ctx.Done():
			return
		case chunk, ok := <-in:
			if !ok {
				return
			}
			if len(chunk.PCM) == 0 {
				continue
			}
			// Payload carries chunk.PCM as-is (already 16-bit LE mono --
			// the same byte layout InjectBotAudio expects, so no
			// conversion is needed here or in runTTSPacer).
			pacer.buf.Push(Packet{SeqNum: seq, Payload: chunk.PCM}, time.Now())
			seq++
			// Recorded *after* Push so runTTSPacer, which loops "while
			// there is still an unresolved pushed chunk", never observes
			// pushed incremented before the corresponding packet is
			// actually visible to Pull.
			pacer.pushed.Add(1)
		}
	}
}

// runTTSPacer is the consumer half of DuplexSession's outbound TTS-pacing
// bridge: on a DefaultTTSPacingInterval-ish ticker (see
// DuplexConfig.TTSPacing), it releases at most one chunk per tick to
// inject (leg.InjectBotAudio) -- mirroring how a real playout loop would
// call JitterBuffer.Pull once per fixed interval (see jitter.go's package
// doc comment: Pull returns an already-buffered packet immediately
// regardless of its scheduled deadline, so the pacing/spreading-out
// behavior this stage exists for comes entirely from calling Pull at most
// once per tick here, not from Pull itself withholding an already-arrived
// packet). ClearStream's own InjectBotAudio/PlaybackQueue already does the
// fixed-size, fixed-cadence RTP framing/pacing downstream of this (see
// clearstream/pkg/rtp/playback.go's startPlaybackLoop), so this stage's
// job is only to spread bursty TTS *arrivals* out over time before they
// reach that queue, not to reproduce its per-frame RTP cadence itself.
//
// A PullLost status here means pacer's TargetDelay budget elapsed before
// the next expected chunk arrived -- i.e. TTS synthesis stalled long
// enough that this chunk was dropped to keep pacing/output latency
// bounded, not that a network packet was actually lost (see
// jitter.go's PullLost doc comment and the package doc comment's
// repurposing note). It's logged, not fatal, and this tick keeps checking
// past any run of consecutive lost slots so a stale backlog doesn't
// permanently stall releases -- but it still injects at most one *found*
// chunk per tick, same as the normal case, and it never calls Pull for a
// sequence number feedTTSPacer hasn't actually pushed yet (tracked via
// pacer.pushed -- see ttsPacer's doc comment for why: JitterBuffer itself
// has no notion of "no more packets are ever coming", so without this
// bound, every tick after the last real chunk was resolved would keep
// declaring a nonexistent future chunk lost, forever).
//
// runTTSPacer exits only when d.ctx is cancelled (Stop's backstop) --
// deliberately not when its feedTTSPacer counterpart's input channel
// closes, so any chunk already buffered in pacer at that point still gets
// released and injected instead of silently discarded.
func (d *DuplexSession) runTTSPacer(pacer *ttsPacer, inject func([]byte) bool, legName string) {
	defer d.wg.Done()

	ticker := time.NewTicker(d.cfg.ttsPacingConfig().PacketInterval)
	defer ticker.Stop()

	var consumed uint64 // count of sequence numbers this goroutine has resolved (PullOK or PullLost)
	for {
		select {
		case <-d.ctx.Done():
			return
		case now := <-ticker.C:
			for consumed < pacer.pushed.Load() {
				pkt, status := pacer.buf.Pull(now)
				if status == PullLost {
					consumed++
					d.logger.Warn("langstream/rtp: TTS pacing buffer dropped a synthesized chunk (synthesis stalled past the pacing budget)",
						zap.String("leg", legName))
					continue
				}
				if status == PullOK {
					consumed++
					if !inject(pkt.Payload) {
						d.logger.Warn("langstream/rtp: InjectBotAudio dropped one or more frames (playback queue full)",
							zap.String("leg", legName))
					}
				}
				break
			}
		}
	}
}

// int16PCMToBytes converts samples (native-endian []int16, ClearStream's
// CleanAudioFrame.PCM representation) to 16-bit little-endian PCM bytes,
// matching asr.AudioFrame.PCM's documented byte layout ("16-bit LE,
// mono"). See pcmBytesToInt16 for the inverse conversion.
func int16PCMToBytes(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], uint16(s))
	}
	return buf
}

// pcmBytesToInt16 converts 16-bit little-endian PCM bytes (as produced by
// int16PCMToBytes, and as carried by asr.AudioFrame.PCM/tts.AudioChunk.PCM
// throughout this codebase) back to a native []int16 slice. It is the
// inverse of int16PCMToBytes; DuplexSession's own bridging goroutines
// don't need this direction (InjectBotAudio already accepts raw PCM16
// bytes directly, see bridgeHears), but it's kept alongside
// int16PCMToBytes as a pair so the conversion's correctness (in
// particular, round-trip and endianness) can be tested directly, and so
// any future code in this package that needs to inspect/build PCM16 byte
// buffers from int16 samples (e.g. test helpers constructing RTP payloads)
// has a single, tested place to do it. A trailing odd byte (len(b)%2 != 0)
// is dropped rather than causing a panic or an out-of-bounds read.
func pcmBytesToInt16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2 : i*2+2]))
	}
	return out
}
