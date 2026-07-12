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

// duplexStopTimeout bounds how long Stop waits for the four bridging
// goroutines to exit before giving up, mirroring the "bounded wait,
// cancel as backstop" pattern langstream.Session.Close uses via its own
// finalFlushTimeout constant (see session.go's doc comment on Close). It
// doesn't need to match that value -- mocks and loopback UDP both settle
// in milliseconds -- it's generous on purpose so it (ideally) never fires.
const duplexStopTimeout = 3 * time.Second

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
//   - Session.AgentHearsAudio()  -> agent leg  InjectBotAudio
//   - Session.CallerHearsAudio() -> caller leg InjectBotAudio
//
// Construct with NewDuplexSession, start with Start, and always Stop it
// exactly once done (Stop is idempotent and safe to call more than once,
// mirroring langstream.Session.Close's contract).
type DuplexSession struct {
	cfg DuplexConfig

	callerSess *clearstream.Session
	agentSess  *clearstream.Session
	session    *langstream.Session

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
		cfg:        cfg,
		callerSess: callerSess,
		agentSess:  agentSess,
		session:    cfg.Session,
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
	}, nil
}

// Start starts both underlying ClearStream sessions and the four
// bridging goroutines described in DuplexSession's doc comment. It is
// idempotent: calling it more than once has no additional effect beyond
// the first call.
func (d *DuplexSession) Start() {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.startedFlag || d.stoppedFlag {
		// Already started (idempotent no-op, matching the previous
		// sync.Once-based contract), or Stop() already won the race and
		// this DuplexSession is shutting down/shut down -- in the
		// latter case, proceeding would call wg.Add(4) with no
		// guarantee Stop's wg.Wait() hasn't already been reached. See
		// lifecycleMu's doc comment.
		return
	}
	d.startedFlag = true

	d.callerSess.Start()
	d.agentSess.Start()

	d.wg.Add(4)
	go d.bridgeCleanAudio(d.callerSess, d.session.PushCallerAudio, "caller")
	go d.bridgeCleanAudio(d.agentSess, d.session.PushAgentAudio, "agent")
	go d.bridgeHears(d.session.AgentHearsAudio(), d.agentSess.InjectBotAudio, "agent")
	go d.bridgeHears(d.session.CallerHearsAudio(), d.callerSess.InjectBotAudio, "caller")
}

// Stop stops both underlying ClearStream sessions and waits (up to
// duplexStopTimeout) for the four bridging goroutines to exit. It is
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
	// ClearStream session's goroutines (nor this package's four bridging
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

// bridgeHears is the Session.AgentHearsAudio()/CallerHearsAudio() ->
// InjectBotAudio bridging goroutine: it reads synthesized tts.AudioChunks
// off in and forwards their PCM (already 16-bit LE mono -- the same byte
// layout InjectBotAudio expects, so no conversion is needed here) to
// inject (leg.InjectBotAudio). It exits when in closes (Session was
// closed by whoever owns its lifecycle) or d.ctx is cancelled (Stop's
// backstop), whichever comes first.
func (d *DuplexSession) bridgeHears(in <-chan tts.AudioChunk, inject func([]byte) bool, legName string) {
	defer d.wg.Done()

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
			if !inject(chunk.PCM) {
				d.logger.Warn("langstream/rtp: InjectBotAudio dropped one or more frames (playback queue full)",
					zap.String("leg", legName))
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
