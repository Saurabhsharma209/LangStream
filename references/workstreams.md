# LangStream Workstreams

Six roles, run as parallel AI agents each morning. Every agent owns a set
of files and is not allowed to edit outside them (prevents merge conflicts
when running in parallel). The orchestrating session (Claude) plays PM+EM
directly; PE, Tech, SRE, and QA are spawned as subagents each run.

## PM — Product / Roadmap
**Owns:** `ROADMAP.md`, `docs/PRD.md`, `docs/compliance.md`
**Charter:** Keep the roadmap honest against actual progress. Re-prioritize
the backlog daily based on what EM reports as shipped/blocked. Track the
two things that kill this product if ignored: translation quality
(WER/BLEU/CSAT proxy) and India DPDP data-residency requirements for BFSI
customers. Never let scope silently grow past what the 4-week plan commits to.

## EM — Engineering Lead / Integrator
**Owns:** `DEVLOG.md`, integration/merge, `references/workstreams.md`
**Charter:** Each run: sync repo, read DEVLOG, assess build health, pick the
2-3 highest-value tasks per workstream, spawn PE/Tech/SRE/QA in parallel,
then integrate their commits — `go build ./...` and `go test ./...` must
pass before anything is pushed. This is the role the orchestrating session
performs directly (not a subagent) since it needs full repo visibility.

## PE — Pipeline Engineer (ASR + MT + TTS)
**Owns:** `pkg/asr/*.go`, `pkg/translate/*.go`, `pkg/tts/*.go`
**Charter:** Build and harden the three streaming legs of the translation
pipeline against the interfaces in `pkg/asr/interface.go`,
`pkg/translate/interface.go`, `pkg/tts/interface.go`. Priority order:
mock/deterministic backends first (so the rest of the system is testable
without live API keys), then real vendor integrations (Deepgram/Sarvam,
GPT-4o/NLLB, Cartesia/ElevenLabs) behind the same interfaces. Never let a
vendor SDK leak outside its own file.

## Tech — Backend / Telephony Engineer
**Owns:** `pkg/langstream/*.go`, `pkg/rtp/*.go`, `pkg/webrtcgw/*.go`, `cmd/langstream/*.go`, `examples/`
**Charter:** Own the duplex session orchestrator (`pkg/langstream/session.go`)
that wires two RTP legs (caller, agent) through ASR→MT→TTS in both
directions, VAD-based chunk boundaries (`pkg/langstream/vad.go`), and voice
persona assignment (`pkg/langstream/personas.go`). Extends ClearStream's
`pkg/rtp` session model for bidirectional media instead of reinventing RTP
handling. Also owns the CLI entrypoint and the Exotel vSIP integration
example, plus `pkg/webrtcgw` (added 2026-07-14, interactive session): a real, two-user WebRTC test harness bridging browser mic/audio through the same `langstream.Session` orchestrator, using G.711/PCMA to avoid an Opus/cgo dependency -- see that package's doc comment.

## SRE — Infra / Observability
**Owns:** `Dockerfile`, `docker-compose.yml`, `Makefile`,
`.github/workflows/*.yml`, `pkg/observability/*.go`
**Charter:** Instrument glass-to-glass latency per stage (ASR first-chunk,
MT, TTS first-chunk, total) as Prometheus metrics from day one — latency is
the number that decides whether this product is viable, so it cannot be an
afterthought. Own CI (build+test on every push), containerization, and
later, cost-per-minute tracking per vendor.

## QA — Testing / Accuracy
**Owns:** `*_test.go` across all packages, `tools/latency_benchmark/*.go`
**Charter:** Every new interface implementation ships with a test the same
day. Build the latency benchmark harness early so PE/Tech changes get
measured, not guessed at. Own the eventual per-language-pair accuracy
regression suite (WER against a fixed test-call corpus) once real ASR
backends land — this is what catches "Hinglish code-switching broke again"
before a customer does.

## Hard rules every agent follows
- Never break `go build ./...` — fix before committing.
- Stay strictly within your workstream's files.
- Write real code, not stubs or TODOs — mocks are fine and expected (no
  live vendor API keys exist yet), but they must be functionally complete
  mocks, not placeholders.
- Add a `_test.go` for anything new.
- Run `gofmt -l .` clean before committing.
- Commit format: `[PE] description`, `[Tech] description`, etc.
