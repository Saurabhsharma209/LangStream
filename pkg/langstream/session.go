// Package langstream implements LangStream's duplex call-translation
// orchestrator: it wires two RTP legs (caller, agent) through
// ASR -> MT -> TTS in both directions so each party hears the other
// speaking in their own language.
//
// The orchestrator depends only on the pkg/asr, pkg/translate, and pkg/tts
// interfaces, never on a concrete backend, so it works identically against
// mocks (Week 1) and real vendor integrations (Week 2+).
package langstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/exotel/langstream/pkg/asr"
	"github.com/exotel/langstream/pkg/observability"
	"github.com/exotel/langstream/pkg/translate"
	"github.com/exotel/langstream/pkg/tts"
)

// finalFlushTimeout bounds how long Close waits for both leg goroutines to
// drain a final transcript flushed by the ASR streams' Close() calls before
// forcing shutdown via context cancellation. Mocks settle in microseconds;
// this is generous on purpose so it never fires in Week 1. Week 2 should
// make this configurable per-vendor once real ASR flush behavior is known.
const finalFlushTimeout = 3 * time.Second

// Language is LangStream's BCP-47-ish language tag (e.g. "hi", "en"). It is
// a distinct string type from asr.Language/translate.Language/tts.Language
// so that this package's public API doesn't force callers to import three
// sub-packages just to name a language; conversions at the boundary are
// simple string conversions since all four types share the same underlying
// representation.
type Language string

// SessionConfig configures a duplex translation Session.
type SessionConfig struct {
	// CallerLanguage is the language the caller speaks and hears.
	CallerLanguage Language
	// AgentLanguage is the language the agent speaks and hears.
	AgentLanguage Language

	// ASR is the speech-recognition backend used for both legs. Two
	// independent StreamSessions are opened against it (one per leg).
	ASR asr.Recognizer
	// Translator performs text translation between CallerLanguage and
	// AgentLanguage (and back).
	Translator translate.Translator
	// TTS synthesizes translated text back into audio for the listening
	// party.
	TTS tts.Synthesizer

	// VoicePersona, if set, is used as the default synthesis voice for
	// whichever language it names (VoicePersona.Language). It is a
	// convenience for the common "one custom voice" case; for anything
	// more elaborate, use PersonaManager.Set on the Session's persona
	// manager after construction.
	VoicePersona *tts.Persona

	// CodeSwitching, if true, disables the fixed per-leg language hint
	// passed to asr.Recognizer.StartStream (passing "" instead), asking
	// the backend to auto-detect language / handle intra-utterance
	// code-switching (e.g. Hinglish) if it supports that (Sarvam does;
	// see asr.Recognizer.StartStream doc). It has no effect on backends
	// that ignore the hint.
	CodeSwitching bool

	// Fallback configures graceful-degradation behavior (ROADMAP.md Week
	// 3: low ASR confidence, MT/TTS timeouts or errors, and permanent
	// leg failure all fall back to passing through the original,
	// untranslated audio instead of silently dropping or mistranslating
	// it — see fallback.go). The zero value is filled in entirely with
	// DefaultFallbackConfig's defaults by NewSession, so existing callers
	// that never set this field get the new behavior for free. See
	// FallbackConfig's doc comment if you do want to set individual
	// fields explicitly.
	Fallback FallbackConfig
}

// validate checks that cfg is complete enough to build a Session.
func (cfg SessionConfig) validate() error {
	if cfg.CallerLanguage == "" {
		return errors.New("langstream: SessionConfig.CallerLanguage must be set")
	}
	if cfg.AgentLanguage == "" {
		return errors.New("langstream: SessionConfig.AgentLanguage must be set")
	}
	if cfg.ASR == nil {
		return errors.New("langstream: SessionConfig.ASR must not be nil")
	}
	if cfg.Translator == nil {
		return errors.New("langstream: SessionConfig.Translator must not be nil")
	}
	if cfg.TTS == nil {
		return errors.New("langstream: SessionConfig.TTS must not be nil")
	}
	return nil
}

// outboundBuffer is the buffer size of each leg's outbound audio channel.
// It absorbs brief consumer stalls (e.g. RTP writer scheduling jitter)
// without blocking the translation pipeline.
const outboundBuffer = 32

// Session is a live duplex translation session between a caller and an
// agent. It owns two independent pipelines ("legs"):
//
//   - caller leg: caller audio -> ASR -> Translator -> TTS -> agent hears it
//   - agent leg:  agent audio  -> ASR -> Translator -> TTS -> caller hears it
//
// Each leg runs on exactly one goroutine for the lifetime of the Session,
// started by NewSession and stopped by Close. Session is safe for
// concurrent use by multiple goroutines (e.g. one pushing caller audio,
// one pushing agent audio, one reading each outbound channel, one calling
// Close).
//
// Each leg also degrades gracefully instead of silently dropping or
// mistranslating audio: see fallback.go and the Fallback field of
// SessionConfig for the low-confidence / MT-or-TTS-timeout-or-error /
// permanent-leg-failure behavior.
type Session struct {
	cfg SessionConfig

	ctx    context.Context
	cancel context.CancelFunc

	callerASR asr.StreamSession
	agentASR  asr.StreamSession

	// agentOut carries audio synthesized from the caller's speech
	// (translated into AgentLanguage) for the agent to hear.
	agentOut chan tts.AudioChunk
	// callerOut carries audio synthesized from the agent's speech
	// (translated into CallerLanguage) for the caller to hear.
	callerOut chan tts.AudioChunk

	personas *PersonaManager

	// callerLeg/agentLeg hold per-leg fallback bookkeeping: the raw-audio
	// ring buffer used for passthrough and the permanent-degradation
	// state. callerLeg corresponds to the caller->agent pipeline (fed by
	// PushCallerAudio), agentLeg to the agent->caller pipeline (fed by
	// PushAgentAudio). See fallback.go.
	callerLeg *legState
	agentLeg  *legState

	fallback FallbackConfig
	// metrics records fallback events via pkg/observability's existing
	// exported RecordEvent/RecordError API (see fallback.go). Never nil
	// after NewSession returns.
	metrics *observability.LatencyRecorder

	wg sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// NewSession validates cfg and starts a new duplex translation session.
// The returned Session must eventually be Close()d to release the
// underlying ASR streams and stop its internal goroutines.
func NewSession(ctx context.Context, cfg SessionConfig) (*Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(ctx)

	personas := NewPersonaManager()
	if cfg.VoicePersona != nil {
		personas.Set(Language(cfg.VoicePersona.Language), *cfg.VoicePersona)
	}

	callerHint := cfg.CallerLanguage
	agentHint := cfg.AgentLanguage
	if cfg.CodeSwitching {
		callerHint = ""
		agentHint = ""
	}

	callerASR, err := cfg.ASR.StartStream(sessCtx, asr.Language(callerHint))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("langstream: starting caller ASR stream: %w", err)
	}

	agentASR, err := cfg.ASR.StartStream(sessCtx, asr.Language(agentHint))
	if err != nil {
		_ = callerASR.Close()
		cancel()
		return nil, fmt.Errorf("langstream: starting agent ASR stream: %w", err)
	}

	// A completely untouched SessionConfig.Fallback (the common case
	// today) gets full defaults, including DegradeToneEnabled=true. If
	// the caller set any field explicitly, only the numeric fields are
	// defaulted (see FallbackConfig.withDefaults's doc comment for why).
	fallback := cfg.Fallback
	if fallback == (FallbackConfig{}) {
		fallback = DefaultFallbackConfig()
	} else {
		fallback = fallback.withDefaults()
	}

	metrics := fallback.Metrics
	if metrics == nil {
		metrics = observability.NewLatencyRecorder()
	}

	s := &Session{
		cfg:       cfg,
		ctx:       sessCtx,
		cancel:    cancel,
		callerASR: callerASR,
		agentASR:  agentASR,
		agentOut:  make(chan tts.AudioChunk, outboundBuffer),
		callerOut: make(chan tts.AudioChunk, outboundBuffer),
		personas:  personas,
		callerLeg: newLegState("caller", audioBufferFrames),
		agentLeg:  newLegState("agent", audioBufferFrames),
		fallback:  fallback,
		metrics:   metrics,
	}

	s.wg.Add(2)
	go s.runLeg(s.callerLeg, callerASR, cfg.CallerLanguage, cfg.AgentLanguage, s.agentOut)
	go s.runLeg(s.agentLeg, agentASR, cfg.AgentLanguage, cfg.CallerLanguage, s.callerOut)

	return s, nil
}

// Personas returns the Session's persona manager, so callers can adjust
// per-language voice assignment after construction (e.g. once an agent's
// preferred voice is known mid-call).
func (s *Session) Personas() *PersonaManager {
	return s.personas
}

// Metrics returns the *observability.LatencyRecorder this Session records
// fallback events into (see FallbackConfig.Metrics's doc comment). If
// SessionConfig.Fallback.Metrics was left nil, this is a private recorder
// Session created for itself; pass a shared recorder in via
// SessionConfig.Fallback.Metrics instead if you want fallback events from
// multiple sessions aggregated in one place (e.g. for a future
// observability dashboard).
func (s *Session) Metrics() *observability.LatencyRecorder {
	return s.metrics
}

// CallerLegDegraded reports whether the caller leg (caller audio -> ASR ->
// Translator -> TTS -> agent hears it) has been marked permanently
// degraded (see FallbackConfig.MaxConsecutiveFailures and FatalError).
// Once true it stays true for the rest of the Session's life: raw caller
// audio is passed through to the agent instead of being translated.
func (s *Session) CallerLegDegraded() bool {
	return s.callerLeg.isDegraded()
}

// AgentLegDegraded is CallerLegDegraded's counterpart for the agent leg
// (agent audio -> ASR -> Translator -> TTS -> caller hears it).
func (s *Session) AgentLegDegraded() bool {
	return s.agentLeg.isDegraded()
}

// PushCallerAudio feeds one frame of caller audio into the caller-leg ASR
// stream. It returns an error if the session's ASR backend rejects the
// frame or the session has been closed.
//
// The frame is also retained in the caller leg's raw-audio buffer (see
// fallback.go) so that if this utterance ends up falling back to
// passthrough (low confidence, MT/TTS failure, or a permanently degraded
// leg), the *original* audio is available to forward instead of silently
// dropping or mistranslating it.
func (s *Session) PushCallerAudio(frame asr.AudioFrame) error {
	s.callerLeg.audio.push(frame.PCM, frame.SampleRate)
	return s.callerASR.PushAudio(s.ctx, frame)
}

// PushAgentAudio feeds one frame of agent audio into the agent-leg ASR
// stream. It returns an error if the session's ASR backend rejects the
// frame or the session has been closed. See PushCallerAudio's doc comment
// for the raw-audio buffering this also performs.
func (s *Session) PushAgentAudio(frame asr.AudioFrame) error {
	s.agentLeg.audio.push(frame.PCM, frame.SampleRate)
	return s.agentASR.PushAudio(s.ctx, frame)
}

// AgentHearsAudio returns the channel of synthesized audio (translated
// from the caller's speech into AgentLanguage) that should be played to
// the agent. The channel is closed once Close has fully shut the session
// down.
func (s *Session) AgentHearsAudio() <-chan tts.AudioChunk {
	return s.agentOut
}

// CallerHearsAudio returns the channel of synthesized audio (translated
// from the agent's speech into CallerLanguage) that should be played to
// the caller. The channel is closed once Close has fully shut the session
// down.
func (s *Session) CallerHearsAudio() <-chan tts.AudioChunk {
	return s.callerOut
}

// Close shuts both legs of the session down cleanly. It closes both ASR
// streams *before* cancelling the session context, giving each backend the
// chance to flush a final trailing transcript (exactly what happens when a
// caller or agent hangs up mid-utterance) through the still-live pipeline
// instead of racing that flush against context cancellation. It then waits
// (up to finalFlushTimeout) for both leg goroutines to drain that flush and
// exit on their own, cancels the context as a backstop in case a backend
// never closes its Transcripts() channel, and finally closes the outbound
// audio channels. It is safe to call multiple times and from multiple
// goroutines; only the first call does the work, and all callers observe
// the same result. After Close returns, no goroutine started by this
// Session is still running.
//
// Ordering note (fixed after a Day-1 integration-test finding, see
// langstream_integration_test.go): an earlier version cancelled the context
// first, which made asr.MockRecognizer's Close()-time flush race a
// context.Done() that was often already closed by the time it tried to
// send, silently dropping the last utterance of every call on hangup. A
// real streaming ASR vendor flushes trailing audio the same way on
// end-of-stream, so this ordering matters beyond the mock.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		if err := s.callerASR.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing caller ASR stream: %w", err))
		}
		if err := s.agentASR.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing agent ASR stream: %w", err))
		}

		// Both leg goroutines exit once their ASR stream's Transcripts()
		// channel closes (after delivering any flushed final transcript),
		// which should happen promptly now that the streams are already
		// closed above.
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(finalFlushTimeout):
			// Backstop: a backend that never closes its Transcripts()
			// channel would otherwise hang Close() forever. Cancel to
			// force both leg goroutines to unwind via ctx.Done(), then
			// wait for real (bounded by wg.Wait() having no more work).
			s.cancel()
			<-done
		}

		// Cancel unconditionally (no-op if already cancelled above) to
		// release the context's resources; nothing is still reading it.
		s.cancel()

		// Only safe to close now that both leg goroutines (the only
		// writers) have exited.
		close(s.agentOut)
		close(s.callerOut)

		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// runLeg is the pipeline for a single leg: it reads recognized speech from
// stream, translates final transcripts from srcLang to dstLang, synthesizes
// the translation, and forwards the resulting audio chunks to out. It runs
// until the Session's context is cancelled or stream's Transcripts channel
// closes, and never blocks forever on a stalled consumer or backend
// because every blocking operation is guarded by a select on s.ctx.Done().
//
// Graceful degradation (ROADMAP.md Week 3, see fallback.go): rather than
// silently dropping or mistranslating a final transcript, runLeg falls
// back to forwarding the utterance's original, untranslated audio
// (buffered in leg.audio by Push{Caller,Agent}Audio) whenever the leg is
// already permanently degraded, the transcript's confidence is below
// FallbackConfig.ConfidenceThreshold, the Translator errors or times out,
// or the Synthesizer errors or stalls. Repeated MT/TTS failures (or a
// single FatalError) permanently degrade the leg via leg.recordFailure.
func (s *Session) runLeg(leg *legState, stream asr.StreamSession, srcLang, dstLang Language, out chan<- tts.AudioChunk) {
	defer s.wg.Done()

	transcripts := stream.Transcripts()
	for {
		select {
		case <-s.ctx.Done():
			return
		case tr, ok := <-transcripts:
			if !ok {
				return
			}
			if !tr.IsFinal {
				// Week 1 scope: only final transcripts are translated
				// and synthesized. Incrementally translating partial
				// ASR output (to start TTS before the utterance ends,
				// shaving glass-to-glass latency) is a Week 2+
				// optimization tracked in ROADMAP.md and is intentionally
				// out of scope for the orchestrator skeleton.
				continue
			}

			// utteranceStart is the time the first audio frame of this
			// utterance was pushed (see audioRingBuffer.utteranceStart's
			// doc comment). It must be read *before* draining (drain
			// clears it) and backs the "asr_first_chunk"/"total" latency
			// samples recorded below.
			utteranceStart := leg.audio.utteranceStart()

			// Drain this utterance's buffered raw audio unconditionally:
			// it's either about to be forwarded as passthrough (see
			// below) or discarded because translation succeeded. Either
			// way the buffer must not carry stale audio into the next
			// utterance.
			frames := leg.audio.drain()

			if leg.isDegraded() {
				recordFallback(s.metrics, stageLegDegraded, leg.name)
				if !s.emitPassthroughTimed(frames, out, utteranceStart) {
					return
				}
				continue
			}

			if tr.Confidence > 0 && tr.Confidence < s.fallback.ConfidenceThreshold {
				recordFallback(s.metrics, stageASRConfidence, s.cfg.ASR.Name())
				if !s.emitPassthroughTimed(frames, out, utteranceStart) {
					return
				}
				continue
			}
			recordSuccessMetric(s.metrics, stageASRConfidence, s.cfg.ASR.Name())

			// ASR latency: from the utterance's first pushed audio frame
			// to this final transcript arriving. Recorded here (not
			// earlier) so a degraded-leg or low-confidence utterance --
			// which never reaches this point -- correctly never gets an
			// asr_first_chunk sample either, matching "mt"/"tts_first_chunk"
			// only being recorded for utterances that actually attempt
			// translation.
			if !utteranceStart.IsZero() {
				recordLatency(s.metrics, stageASRFirstChunk, msSince(utteranceStart))
			}

			mtStart := time.Now()
			mtCtx, mtCancel := context.WithTimeout(s.ctx, s.fallback.TranslateTimeout)
			chunk, err := s.cfg.Translator.Translate(mtCtx, tr.Text, translate.Language(srcLang), translate.Language(dstLang), tr.IsFinal)
			mtCancel()
			recordLatency(s.metrics, stageMT, msSince(mtStart))
			if err != nil {
				recordFallbackErr(s.metrics, stageTranslate, s.cfg.Translator.Name(), err)
				if leg.recordFailure(isFatal(err), s.fallback.MaxConsecutiveFailures) {
					recordFallback(s.metrics, stageLegDegraded, leg.name)
				}
				if !s.emitPassthroughTimed(frames, out, utteranceStart) {
					return
				}
				continue
			}
			leg.recordSuccess()
			recordSuccessMetric(s.metrics, stageTranslate, s.cfg.Translator.Name())

			if chunk.Text == "" {
				continue
			}

			persona := s.personas.Get(dstLang)
			ttsCtx, ttsCancel := context.WithCancel(s.ctx)
			audio, err := s.cfg.TTS.SynthesizeStream(ttsCtx, chunk.Text, persona)
			if err != nil {
				ttsCancel()
				recordFallbackErr(s.metrics, stageTTS, s.cfg.TTS.Name(), err)
				if leg.recordFailure(isFatal(err), s.fallback.MaxConsecutiveFailures) {
					recordFallback(s.metrics, stageLegDegraded, leg.name)
				}
				if !s.emitPassthroughTimed(frames, out, utteranceStart) {
					return
				}
				continue
			}

			completed, stalled, shuttingDown := s.forwardTTSWithStallGuard(audio, out, ttsCancel)
			if shuttingDown {
				return
			}
			if stalled {
				recordFallback(s.metrics, stageTTS, s.cfg.TTS.Name())
				if leg.recordFailure(false, s.fallback.MaxConsecutiveFailures) {
					recordFallback(s.metrics, stageLegDegraded, leg.name)
				}
				if !s.emitPassthroughTimed(frames, out, utteranceStart) {
					return
				}
				continue
			}
			if completed {
				leg.recordSuccess()
				recordSuccessMetric(s.metrics, stageTTS, s.cfg.TTS.Name())
				recordTotalIfStarted(s.metrics, utteranceStart)
			}
		}
	}
}

// emitPassthrough builds (see buildPassthroughChunks) and forwards the
// passthrough audio for one degraded utterance: the raw source audio in
// frames, optionally preceded by a warning tone per
// FallbackConfig.DegradeToneEnabled. It returns false if the session is
// shutting down mid-send (mirroring forwardAudio's contract, and callers
// should stop processing further transcripts in that case), true
// otherwise.
func (s *Session) emitPassthrough(frames []bufferedFrame, out chan<- tts.AudioChunk) bool {
	chunks := buildPassthroughChunks(frames, s.fallback.DegradeToneEnabled)
	return s.forwardAudio(chunksChannel(chunks), out)
}

// emitPassthroughTimed is emitPassthrough plus a "total" (glass-to-glass)
// latency sample: fallback/passthrough utterances skip MT/TTS but still
// matter for degraded-call latency (see this package's Task 1 charter),
// so every passthrough path records "total" the same way a fully
// successful translate+synthesize round trip does (see runLeg's
// `if completed` branch). The sample is only recorded if the forward
// completed normally (ok == true); a shutdown mid-send aborts the
// utterance, so no latency sample is recorded for it (mirroring
// forwardAudio/forwardTTSWithStallGuard's existing shuttingDown
// contract).
func (s *Session) emitPassthroughTimed(frames []bufferedFrame, out chan<- tts.AudioChunk, utteranceStart time.Time) bool {
	ok := s.emitPassthrough(frames, out)
	if ok {
		recordTotalIfStarted(s.metrics, utteranceStart)
	}
	return ok
}

// forwardTTSWithStallGuard waits up to FallbackConfig.SynthesizeTimeout for
// the first chunk on in (the channel returned by Synthesizer.SynthesizeStream).
// If the first chunk arrives within budget, it is forwarded and the rest of
// the stream is handed off to the normal, unbounded forwardAudio — a
// synthesis that has started producing audio is not cut off just because
// its *total* duration exceeds SynthesizeTimeout, only a backend that never
// starts responding is treated as stalled/failed. cancel is always called
// exactly once before this returns, releasing ttsCtx's resources whether
// the stream is left to finish naturally, abandoned as stalled, or the
// session is shutting down.
//
// Return values: completed is true if the stream was forwarded to a
// normal close (with or without any chunks). stalled is true if no chunk
// arrived within SynthesizeTimeout. shuttingDown is true if s.ctx was
// cancelled while waiting/forwarding, in which case the caller's leg
// goroutine should return immediately, matching forwardAudio's contract.
// At most one of completed/stalled/shuttingDown is true.
func (s *Session) forwardTTSWithStallGuard(in <-chan tts.AudioChunk, out chan<- tts.AudioChunk, cancel context.CancelFunc) (completed, stalled, shuttingDown bool) {
	defer cancel()

	synthesizeStart := time.Now()
	timer := time.NewTimer(s.fallback.SynthesizeTimeout)
	defer timer.Stop()

	select {
	case <-s.ctx.Done():
		return false, false, true
	case chunk, ok := <-in:
		if !ok {
			return true, false, false
		}
		// tts_first_chunk: time from starting SynthesizeStream to this,
		// its first chunk, actually arriving. Recorded only once we know
		// a real chunk arrived (ok == true) -- a closed-with-no-chunks
		// stream (!ok, handled above) or an outright stall (timer.C,
		// handled below) never gets a sample, matching "only measure the
		// stage that actually happened".
		recordLatency(s.metrics, stageTTSFirstChunk, msSince(synthesizeStart))
		select {
		case out <- chunk:
		case <-s.ctx.Done():
			return false, false, true
		}
		if chunk.IsFinal {
			return true, false, false
		}
	case <-timer.C:
		return false, true, false
	}

	if !s.forwardAudio(in, out) {
		return false, false, true
	}
	return true, false, false
}

// forwardAudio drains in and forwards every chunk to out until in closes
// or the session context is cancelled. It returns false if it stopped
// because the session is shutting down (so the caller should stop
// processing further transcripts too), true if it stopped because in
// closed normally.
func (s *Session) forwardAudio(in <-chan tts.AudioChunk, out chan<- tts.AudioChunk) bool {
	for {
		select {
		case <-s.ctx.Done():
			return false
		case chunk, ok := <-in:
			if !ok {
				return true
			}
			select {
			case out <- chunk:
			case <-s.ctx.Done():
				return false
			}
		}
	}
}
