# LangStream — 1-Month Roadmap

**Goal for this month: a working, engineer-monitored pilot** — one language
pair (Hindi ↔ English), running on live calls for 1-2 anchor customers.
Not GA. See `docs/PRD.md` for what GA actually requires (multi-language,
multi-tenant, DPDP-compliant data residency, SLA) — that's a separate,
longer track that starts once this pilot proves latency and CSAT hold up.

**Execution model:** 6 AI agents (PM, EM, PE, Tech, SRE, QA — see
`references/workstreams.md`) run every morning at 9am. Each run compresses
roughly 3 roadmap-days of work into one session by running PE/Tech/SRE/QA
in parallel against stable interfaces, with EM integrating and PM updating
this roadmap. At that pace a 20-roadmap-day plan lands in ~7 calendar days
*if the pipeline stays clean* — the honest constraint is that vendor
integration and live-call debugging (Week 2-4) don't compress as cleanly
as scaffolding (Week 1) does, so treat the calendar dates below as a
floor, not a promise.

Start date: 2026-07-07.

---

## Week 1 — Foundations (Roadmap Days 1-5, target: Sprint 1-2, ~Jul 7-8)

- [x] Repo scaffold, `go.mod`, workstream ownership map
- [x] Stable interfaces: `asr.Recognizer`, `translate.Translator`, `tts.Synthesizer`
- [x] Mock backends for all three (deterministic, no API keys needed) so
      the orchestrator is testable end-to-end before any vendor is wired in
- [x] `pkg/langstream/session.go` — duplex orchestrator skeleton (caller
      leg + agent leg, each running ASR→MT→TTS independently)
- [x] `pkg/langstream/vad.go` — chunk-boundary detection (reuse ClearStream's
      VAD approach)
- [x] CI: `go build` + `go test` on every push
- [x] Latency instrumentation stub (Prometheus-text-compatible export) —
      wired to mocks first so the measurement path exists before it matters
- [x] End-to-end mock test: fake caller audio in, fake translated audio out,
      full session lifecycle, asserting latency budget is *measured*
      (not yet met — mocks don't reflect real vendor latency)

**Week 1 status: complete** (2026-07-07, Sprint 1). All items shipped, tested
(`go build`/`vet`/`test -race` clean, `gofmt` clean), and pushed. One real
bug was found by QA's integration test and fixed same day — see DEVLOG.md.
A second bug (a `.gitignore` pattern that silently excluded `pkg/langstream/`
and `cmd/langstream/` from the first push, breaking CI) was caught via a
fresh-clone verification and fixed the same day — see DEVLOG.md.

## Week 2 — Real Pipeline (Roadmap Days 6-10, target: ~Jul 9-10)

**Decision (2026-07-07):** no vendor API keys (Deepgram/Sarvam/OpenAI/
Cartesia) exist yet, and Saurabh has chosen to proceed without waiting for
them. This means: PE writes real, complete vendor client code (correct
request/response shapes, streaming/chunking logic, error handling) behind
the Week 1 interfaces, and QA tests it against fake/mock HTTP or WebSocket
servers standing in for each vendor — not against the mock backends from
Week 1 (those stay as-is for orchestrator-level tests), but not against a
live vendor either. Live end-to-end testing with real keys is deferred
until keys are available; when that happens, the fake-server tests should
still pass unchanged (proving the client code itself was correct) and only
a real-network smoke test needs to be added on top. Missing keys is not a
blocker for this week's checklist items below — do not report it as one.

- [x] Deepgram streaming ASR (English) + Sarvam streaming ASR (Hindi,
      code-switching aware)
- [x] GPT-4o streaming translation, Hindi↔English
- [x] Cartesia streaming TTS, both languages; basic persona config
- [x] Wire real backends behind the Week 1 interfaces (swap, don't rewrite)
- [ ] Extend ClearStream's `pkg/rtp` session for bidirectional media —
      this is the highest-risk item; budget extra time here. **Coordination
      checkpoint** (see `COMBINED_ROADMAP.md`): check ClearStream's latest
      tag first; if `pkg/rtp` needs an actual code change to support
      duplex use (not just importing it as-is), stop and report to
      Saurabh rather than pushing to the ClearStream repo unilaterally.
      **Status (2026-07-08): checked, blocked, needs Saurabh's input — see
      DEVLOG.md.** ClearStream is still at `v0.1.0` (no new tag). Its
      `pkg/rtp.Session` is a single-leg, network-to-network design (UDP in
      → denoise → UDP out) with an exported `InjectBotAudio([]byte) bool`
      hook that *would* cover LangStream's TTS-out direction as-is, but
      there is no exported hook to read the decoded/cleaned PCM for the
      caller→ASR direction — that audio never leaves `handlePacket`
      today. A real (small, additive) ClearStream code change — e.g. an
      optional `Config.OnCleanAudio func([]int16)` callback — is needed
      for the ASR-in direction. Not attempted this run per the standing
      cross-repo rule; not started until Saurabh decides how to proceed.
- [x] First real Hindi↔English round-trip on recorded test audio (not
      live calls yet), measure actual glass-to-glass latency — done
      against fake local vendor servers (Deepgram/Sarvam/GPT-4o/Cartesia
      protocol-accurate fakes), per the Week 2 decision above; live-key
      version deferred until real vendor keys exist.

**Week 2 status (2026-07-08, Sprint 2): 5 of 6 items complete.** Real,
protocol-accurate vendor client code for ASR/MT/TTS is written, tested
against fake vendor servers, and wired behind the Week 1 interfaces via a
name-based backend registry (`pkg/langstream/backends.go`,
`langstream demo --backend deepgram|sarvam|gpt4o|cartesia|mock`). The one
remaining item (duplex RTP) is a genuine decision point for Saurabh, not a
scheduling slip — see DEVLOG.md 2026-07-08 for the full writeup.

## Week 3 — Pilot Hardening (Roadmap Days 11-15, target: ~Jul 11-13)

- [ ] Jitter buffer tuning against real PSTN conditions (lab latency
      always looks better than field latency — this is where that gap
      gets found and closed, or doesn't). **Status (2026-07-09): groundwork
      done, real-condition tuning still pending.** `pkg/rtp/jitter.go` is a
      real, tested, transport-agnostic jitter buffer (reordering,
      duplicate/late-packet handling, loss policy, simulated PSTN-like
      conditions in tests) — but it has no live transport behind it yet,
      so it's an algorithm proven in simulation, not tuned against real
      PSTN traces. Depends on the same duplex-RTP decision below.
- [x] Fallback behavior: what happens when translation lags, a leg drops,
      or confidence is low (never silently mistranslate — degrade
      gracefully, e.g. pass through original audio with a warning tone).
      **Done (2026-07-09):** low-confidence ASR, MT/TTS errors, and
      timeouts all fall back to original-audio passthrough with an
      optional warning tone; repeated failures or a fatal backend error
      permanently degrade a leg without crashing or hanging. See
      `pkg/langstream/fallback.go`, integration-tested end to end.
- [ ] Exotel vSIP integration example wired end-to-end. **Status (2026-07-10): contract/shape example added, not end-to-end.** `examples/vsip_example/` now shows the intended integration shape (`VSIPCallAdapter` pushing/pulling PCM through a real `langstream.Session`), integration-tested against a real Session with mock backends. Real SIP/RTP socket plumbing and the ClearStream duplex-RTP piece are still not implemented — both depend on the same 2026-07-08 ClearStream decision. Left unchecked deliberately; do not mark this done until real transport is wired.
- [x] Observability dashboard (latency percentiles, error rates, per-vendor cost).
      **Done (2026-07-09):** `pkg/observability` now tracks error rates and
      per-vendor cost alongside the existing latency percentiles, and
      serves them via a real, tested HTTP dashboard
      (`observability.NewDashboardServer`, `/`, `/dashboard.json`,
      `/metrics`). Not yet started inside `cmd/langstream`'s actual
      binary — that wiring (pointing the CLI's recorder at the dashboard
      server on startup) is a small next-sprint task, not a new blocker.
      **CLI wiring done (2026-07-10):** `langstream serve --addr :8080` now starts a live Session and mounts the dashboard on it; `docker-compose.yml` runs this by default. Integration-tested (real HTTP hits against a running binary, real session activity reflected in `/dashboard.json`).
- [x] `docs/compliance.md`: DPDP data-residency assessment — can pilot
      traffic legally go through US-hosted GPT-4o, or does the anchor
      customer's data need to stay in-region from day one.
      **Drafted (2026-07-09), pending legal sign-off** — see
      `docs/compliance.md`. Preliminary finding: DPDP itself is
      permissive on cross-border transfer by default, but RBI's
      data-localization rules for financial/payments data are the more
      likely binding constraint for a BFSI anchor customer, and the two
      should not be conflated. Recommends confirming vendor DPAs and
      getting legal sign-off before routing a BFSI customer's calls
      through any US-hosted vendor (GPT-4o today).
- [x] Consent/disclosure language for calls routed through AI translation.
      **Drafted (2026-07-09), pending legal sign-off** — see
      `docs/compliance.md` §4 for short (IVR prompt) and long (written
      consent) variants, with open items flagged for legal (opt-in vs.
      opt-out, retention period, per-vendor training-data claims).

## Week 4 — Pilot Launch (Roadmap Days 16-20, target: ~Jul 14-16)

- [ ] Live pilot with 1-2 anchor customers, Hindi↔English, engineer-monitored
- [ ] Real WER, latency, and CSAT measurement on live traffic (not lab data)
- [ ] Daily bug-fix loop against pilot findings
- [ ] Go/no-go writeup: does translated-voice CSAT hold up against a
      human-translated or single-language baseline? If not, this is the
      point to say so plainly rather than push to GA on hope
- [ ] If go: staffing and timeline proposal for the GA track (8-9
      engineers, 3-4 months, per the earlier team-sizing discussion)

---

## Explicitly out of scope for this month
- More than one language pair
- Self-hosted models (Whisper/NLLB/Coqui) for cost reduction — stay on
  paid APIs until the pilot proves the product is worth optimizing
- Multi-tenant provisioning, billing, or a customer-facing console
- Voice cloning / preserving the original speaker's timbre
- Any claim of "GA" — this roadmap produces a pilot, not a shippable product
