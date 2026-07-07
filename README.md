# LangStream — Real-Time Call Translation SDK

> Duplex, real-time speech translation at the telephony layer. A caller
> speaks Hindi, an agent speaks English — each hears the other in their own
> language, live, mid-call. Built as the natural successor to
> [ClearStream](https://github.com/Saurabhsharma209/ClearStream) (Exotel's
> audio-enhancement SDK): better input audio makes every downstream ASR
> stage more accurate, so LangStream is designed to sit right after it in
> the pipeline.

**Status: Week 1 of a 1-month pilot build, `v0.1.0-alpha`.** This is not a
GA product. See [`ROADMAP.md`](ROADMAP.md) for the honest scope of what
"done" means this month (one language pair, engineer-monitored pilot with
1-2 anchor customers) versus what real GA requires later (multi-language,
multi-tenant, DPDP-compliant data residency, SLA — a separate, longer
track). See [`COMBINED_ROADMAP.md`](COMBINED_ROADMAP.md) for how this
project's timeline relates to ClearStream's, and
[`VERSIONING.md`](VERSIONING.md) for the SemVer policy and version
compatibility matrix between the two repos.

---

## What it does (target architecture)

```
CALLER LEG                              AGENT LEG
──────────────────────────────────────────────────────
Caller speaks (e.g. Hindi)
  ↓
ClearStream (denoise + AGC)             Agent speaks (e.g. English)
  ↓                                       ↓
ASR: Hindi → text                       ClearStream (denoise)
  ↓                                       ↓
MT: Hindi text → English text           ASR: English → text
  ↓                                       ↓
TTS: English text → English audio       MT: English → Hindi text
  ↓                                       ↓
Agent hears English ←────────────       TTS: Hindi audio → Caller hears Hindi
```

Every stage streams — nothing waits for a full sentence before starting the
next stage — because glass-to-glass latency is the number that decides
whether this product is usable on a live call.

## Current state (Week 1)

- Stable Go interfaces for the three pipeline stages: [`pkg/asr`](pkg/asr),
  [`pkg/translate`](pkg/translate), [`pkg/tts`](pkg/tts) — every vendor
  backend (Deepgram, Sarvam, GPT-4o, Cartesia, ...) implements these and
  nothing downstream depends on a specific vendor.
- Deterministic mock backends for all three, so the orchestrator is fully
  testable before any vendor API key exists.
- [`pkg/langstream`](pkg/langstream): the duplex session orchestrator
  (`Session`), RMS-based voice-activity detection (`VAD`), and per-language
  voice persona management (`PersonaManager`).
- [`pkg/observability`](pkg/observability): per-stage latency recording
  with real percentile math, in a Prometheus-text-compatible export format.
- [`tools/latency_benchmark`](tools/latency_benchmark): a harness that
  already measures session-level latency against the mocks today, so Week
  2 gets real vendor numbers by pointing it at real backends instead of
  building the tool from scratch.
- CI (`.github/workflows/ci.yml`): build, vet, race-tested unit tests, and
  a gofmt check on every push.

Real vendor integrations (Deepgram/Sarvam ASR, GPT-4o translation, Cartesia
TTS) and the duplex RTP extension of ClearStream's session layer land in
Week 2 — see `ROADMAP.md`.

## Building

```
go build ./...
go test ./... -race
go run ./cmd/langstream version
go run ./cmd/langstream demo
```

Or via `make`:

```
make ci     # fmt-check + vet + test + build, same as CI runs
make build
make test
make docker
```

## Repo layout / ownership

See [`references/workstreams.md`](references/workstreams.md) for the full
per-workstream charter and file ownership map (this project is built by six
coordinated agent roles — PM, EM, PE, Tech, SRE, QA — running daily; see
`DEVLOG.md` for the running build log).

## License

MIT (matches ClearStream).
