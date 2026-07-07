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

	s := &Session{
		cfg:       cfg,
		ctx:       sessCtx,
		cancel:    cancel,
		callerASR: callerASR,
		agentASR:  agentASR,
		agentOut:  make(chan tts.AudioChunk, outboundBuffer),
		callerOut: make(chan tts.AudioChunk, outboundBuffer),
		personas:  personas,
	}

	s.wg.Add(2)
	go s.runLeg(callerASR, cfg.CallerLanguage, cfg.AgentLanguage, s.agentOut)
	go s.runLeg(agentASR, cfg.AgentLanguage, cfg.CallerLanguage, s.callerOut)

	return s, nil
}

// Personas returns the Session's persona manager, so callers can adjust
// per-language voice assignment after construction (e.g. once an agent's
// preferred voice is known mid-call).
func (s *Session) Personas() *PersonaManager {
	return s.personas
}

// PushCallerAudio feeds one frame of caller audio into the caller-leg ASR
// stream. It returns an error if the session's ASR backend rejects the
// frame or the session has been closed.
func (s *Session) PushCallerAudio(frame asr.AudioFrame) error {
	return s.callerASR.PushAudio(s.ctx, frame)
}

// PushAgentAudio feeds one frame of agent audio into the agent-leg ASR
// stream. It returns an error if the session's ASR backend rejects the
// frame or the session has been closed.
func (s *Session) PushAgentAudio(frame asr.AudioFrame) error {
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
func (s *Session) runLeg(stream asr.StreamSession, srcLang, dstLang Language, out chan<- tts.AudioChunk) {
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

			chunk, err := s.cfg.Translator.Translate(s.ctx, tr.Text, translate.Language(srcLang), translate.Language(dstLang), tr.IsFinal)
			if err != nil {
				// Week 1 scope: drop the utterance on translation
				// failure rather than propagating an error out of a
				// long-lived goroutine. Week 3 adds graceful
				// degradation (e.g. passing through original audio
				// with a warning tone) per ROADMAP.md.
				continue
			}
			if chunk.Text == "" {
				continue
			}

			persona := s.personas.Get(dstLang)
			audio, err := s.cfg.TTS.SynthesizeStream(s.ctx, chunk.Text, persona)
			if err != nil {
				continue
			}
			if !s.forwardAudio(audio, out) {
				return
			}
		}
	}
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
