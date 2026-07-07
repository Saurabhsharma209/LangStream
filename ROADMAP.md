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

- [ ] Deepgram streaming ASR (English) + Sarvam streaming ASR (Hindi,
      code-switching aware)
- [ ] GPT-4o streaming translation, Hindi↔English
- [ ] Cartesia streaming TTS, both languages; basic persona config
- [ ] Wire real backends behind the Week 1 interfaces (swap, don't rewrite)
- [ ] Extend ClearStream's `pkg/rtp` session for bidirectional media —
      this is the highest-risk item; budget extra time here
- [ ] First real Hindi↔English round-trip on recorded test audio (not
      live calls yet), measure actual glass-to-glass latency

## Week 3 — Pilot Hardening (Roadmap Days 11-15, target: ~Jul 11-13)

- [ ] Jitter buffer tuning against real PSTN conditions (lab latency
      always looks better than field latency — this is where that gap
      gets found and closed, or doesn't)
- [ ] Fallback behavior: what happens when translation lags, a leg drops,
      or confidence is low (never silently mistranslate — degrade
      gracefully, e.g. pass through original audio with a warning tone)
- [ ] Exotel vSIP integration example wired end-to-end
- [ ] Observability dashboard (latency percentiles, error rates, per-vendor cost)
- [ ] `docs/compliance.md`: DPDP data-residency assessment — can pilot
      traffic legally go through US-hosted GPT-4o, or does the anchor
      customer's data need to stay in-region from day one
- [ ] Consent/disclosure language for calls routed through AI translation

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
