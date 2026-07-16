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
- [x] Extend ClearStream's `pkg/rtp` session for bidirectional media —
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
      **Done (2026-07-12).** ClearStream's own daily automation resolved
      the decision by adding `rtp.Session.CleanAudio() <-chan
      CleanAudioFrame` (opt-in, non-blocking, drop-oldest-on-full —
      ClearStream's ROADMAP.md "Resolved Decisions" 2026-07-12 entry).
      Same day, LangStream added `pkg/rtp/duplex.go`'s `DuplexSession`:
      composes two ClearStream `rtp.Session` instances (caller/agent
      legs), bridging `CleanAudio()` → `asr.AudioFrame` →
      `Session.Push{Caller,Agent}Audio` and `Session.{Agent,Caller}HearsAudio()`
      → `InjectBotAudio`. Proven against real (loopback) UDP RTP end to
      end, not just mocked — see `pkg/rtp/duplex_test.go`'s
      `TestDuplexSession_EndToEndLoopback` and the additional QA
      integration tests (bidirectional concurrent traffic, backpressure/
      goroutine-leak, shutdown-ordering, construction-failure-path — see
      DEVLOG.md 2026-07-12). QA's shutdown-ordering test caught, and the
      EM fixed same day, a real `Start()`/`Wait()` `sync.WaitGroup` data
      race. `go.mod` now pins ClearStream via a pseudo-version + `replace`
      (no ClearStream semver tag exists past the fixing commit yet — see
      `VERSIONING.md`). **Not yet done:** wiring `DuplexSession` into
      `cmd/langstream`'s CLI or `examples/vsip_example`'s real SIP/socket
      plumbing — that's real address/config plumbing, intentionally left
      for a follow-up sprint rather than rushed alongside the bridge
      itself.
- [x] First real Hindi↔English round-trip on recorded test audio (not
      live calls yet), measure actual glass-to-glass latency — done
      against fake local vendor servers (Deepgram/Sarvam/GPT-4o/Cartesia
      protocol-accurate fakes), per the Week 2 decision above; live-key
      version deferred until real vendor keys exist.

**Week 2 status: 6 of 6 items complete (2026-07-12).** Real,
protocol-accurate vendor client code for ASR/MT/TTS is written, tested
against fake vendor servers, and wired behind the Week 1 interfaces via a
name-based backend registry (`pkg/langstream/backends.go`,
`langstream demo --backend deepgram|sarvam|gpt4o|cartesia|mock`). The
remaining item (duplex RTP) was a genuine decision point for Saurabh, not
a scheduling slip (see DEVLOG.md 2026-07-08 for the original writeup) —
resolved and implemented 2026-07-12, see the item above and DEVLOG.md
2026-07-12.

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
      **Update (2026-07-12):** added 3 harsher stress-test scenarios
      (~13% loss, bursty multi-position reordering, mid-stream jitter
      spike) — still simulation-only groundwork. **The duplex-RTP
      decision this depended on is now resolved (see the Week 2 item
      above)** — real transport (`pkg/rtp.DuplexSession`, loopback-UDP
      proven) exists as of 2026-07-12, but this jitter buffer isn't wired
      into it yet (`DuplexSession` currently relies on ClearStream's own
      internal `JitterDepth` per leg, not this package's jitter.go). Real-
      condition tuning against actual PSTN traces still needs live/pilot
      traffic, which doesn't exist yet either way. Not checked off.
      **Decision + wiring (2026-07-13):** the original inbound-network-
      jitter use case for this buffer is now redundant — ClearStream
      already jitter-buffers each leg internally before handing off
      clean PCM via `CleanAudio()`. Repurposed the existing `JitterBuffer`
      as an OUTBOUND pacing/smoothing stage on the TTS→`InjectBotAudio`
      path instead (`pkg/rtp/duplex.go`'s `feedTTSPacer`/`runTTSPacer`,
      `pkg/rtp/jitter.go`'s new `ttsPacer` wrapper), since TTS synthesis
      is bursty and benefits from pacing before injection. Tested
      (`pkg/rtp/tts_pacing_test.go`); also tightened the stress tests'
      packet-accounting invariant (`played+lost == n`, exact, see
      DEVLOG.md) which caught a pre-existing phantom-loss measurement bug
      in the test harness itself (not `jitter.go`). Still not checked off
      — real-condition tuning against live PSTN traces remains blocked on
      pilot traffic, unchanged. But the "does this buffer still have a
      role" question from 2026-07-12 is now resolved and shipped.
- [x] Fallback behavior: what happens when translation lags, a leg drops,
      or confidence is low (never silently mistranslate — degrade
      gracefully, e.g. pass through original audio with a warning tone).
      **Done (2026-07-09):** low-confidence ASR, MT/TTS errors, and
      timeouts all fall back to original-audio passthrough with an
      optional warning tone; repeated failures or a fatal backend error
      permanently degrade a leg without crashing or hanging. See
      `pkg/langstream/fallback.go`, integration-tested end to end.
- [x] Exotel vSIP integration example wired end-to-end. **Status (2026-07-10): contract/shape example added, not end-to-end.** `examples/vsip_example/` now shows the intended integration shape (`VSIPCallAdapter` pushing/pulling PCM through a real `langstream.Session`), integration-tested against a real Session with mock backends. Real SIP/RTP socket plumbing and the ClearStream duplex-RTP piece are still not implemented. **Update (2026-07-12):** the ClearStream duplex-RTP dependency itself is resolved and built (`pkg/rtp.DuplexSession`, see the Week 2 item above) — this item is now unblocked, but wiring `examples/vsip_example` to use `DuplexSession` plus real SIP/socket address plumbing is still todo, deliberately scoped out of the 2026-07-12 sprint.
      **Done (2026-07-13):** `cmd/langstream duplex` subcommand now
      constructs and runs a real `rtp.DuplexSession` from CLI flags (both
      legs' listen/forward UDP addresses, payload type, jitter depth,
      suppressor backend), with graceful SIGINT/SIGTERM shutdown and the
      observability dashboard mounted. `examples/vsip_example` now also
      runs a real loopback-UDP `DuplexSession` end-to-end
      (`real_rtp.go`'s `runRealRTPDemo`), alongside the existing shape-
      only `VSIPCallAdapter` demo. Backends are still mocked (per
      ROADMAP's Week 2 decision — no vendor keys yet), but the RTP/socket
      wiring itself is real, not simulated. Integration-tested
      (`cmd/langstream/duplex_test.go`, `examples/vsip_example/real_rtp_test.go`).
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

**Week 3 status: 5 of 6 items complete (2026-07-13).** Only real-condition
jitter-buffer tuning against live PSTN traces remains unchecked, and it
genuinely can't be closed by agent automation — it needs live/pilot call
traffic, which doesn't exist until Week 4 starts. Everything else
(fallback behavior, vSIP example wired end-to-end, observability
dashboard, DPDP/consent drafts) is done.

**2026-07-14 note:** no new Week 3/4 checklist items closed today — the
one remaining Week 3 item and all of Week 4 are genuinely blocked (live
traffic / a business decision on anchor customers), not on more agent
work, so today's scheduled run did hardening instead of inventing scope:
fixed an untested `runServe` shutdown-ordering bug (same class as the
2026-07-13 duplex fix), found and fixed two pre-existing flaky-test races
in `pkg/langstream/latency_test.go`, and continued strengthening the WER
corpus (15→25 entries) and jitter stress tests. See DEVLOG.md's 2026-07-14
entry. Week 4 cannot meaningfully start until Saurabh decides on anchor
customers / live traffic.

**2026-07-15 note:** same situation — still genuinely blocked, so today's
scheduled run (Sprint 9) again did hardening across all four workstreams
in parallel rather than inventing roadmap scope: added retry/backoff to
the three vendor clients that had none (GPT-4o, Cartesia, ElevenLabs);
wired the existing `observability.RecordCost` API into all five real
vendor clients, since it had zero real callers before today despite the
dashboard already being able to display it; added an idle-room timeout +
max-concurrent-rooms cap to `pkg/webrtcgw/room.go` (a resource leak for
abandoned single-peer rooms); and found+fixed one real regression
(`Join`'s `OnConnectionStateChange` cleanup hook was dropped mid-refactor,
reintroducing a leak on a full room's ICE/DTLS failure — the opposite
side of the bug being fixed). See DEVLOG.md's 2026-07-15 entry. Week 4
still cannot meaningfully start until Saurabh decides on anchor customers
/ live traffic.

**2026-07-16 note:** still genuinely blocked on Saurabh's anchor-customer/
live-traffic decision, so today's scheduled run (Sprint 10) again did
hardening across all four workstreams rather than inventing roadmap
scope: added a circuit-breaker/fail-fast layer on top of the retry/backoff
added 2026-07-15 (GPT-4o, Cartesia, ElevenLabs), added TURN server
credential support to `pkg/webrtcgw`/`cmd/langstream webrtc` (closing a
gap flagged 2026-07-14), added the GPT-4o/Cartesia/ElevenLabs
cost-recording integration test flagged as a follow-up in Sprint 9, and
grew the WER corpus (30→35). One real regression was found and fixed
during integration (a circuit breaker that could get permanently stuck
open if a probe call's context was cancelled or hit a permanent error —
see DEVLOG.md's 2026-07-16 entry) and one dashboard-visibility gap was
found and closed (circuit-open fast-fails were indistinguishable from
ordinary errors on the dashboard). Week 4 still cannot meaningfully start
until Saurabh decides on anchor customers / live traffic.

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

## Added out-of-band: WebRTC live-test harness (2026-07-14, interactive session)

Not part of the daily six-agent automation's normal roadmap execution --
requested directly by Saurabh in an interactive session, after testing the
Sarvam ASR fix (below) live and asking to be able to test real-time
translation over an actual browser call instead of only via
`langstream demo`'s one-shot mock harness.

**What shipped:** `langstream webrtc` (`cmd/langstream/webrtc.go`) + a new
`pkg/webrtcgw` package: a real, two-user, browser-facing test harness.
Two people each open a served page, join the same room with opposite
roles ("caller"/"agent"), grant mic access, and talk to each other live
through real ASR->MT->TTS -- no telephony/RTP infrastructure involved,
reusing the same `langstream.Session` duplex orchestrator
`pkg/rtp.DuplexSession` uses for ClearStream's telephony legs.

Key design decision: browsers' WebRTC audio is normally Opus, which would
need a codec library (cgo/libopus) to decode/encode in Go. Instead, this
gateway's `pion/webrtc` `MediaEngine` registers *only* G.711 PCMA (payload
type 8) for audio -- PCMA/PCMU are mandatory-to-implement codecs for every
WebRTC-compliant browser (RFC 7874, specifically for telephony-gateway
interop), so restricting our side to PCMA forces negotiation onto it with
zero special handling needed on the browser/client side. G.711 companding
is simple 8-bit math, not a real codec library -- this is what keeps the
whole gateway cgo-free. See `pkg/webrtcgw/alaw.go`'s package doc comment.

Also added: a real `pkg/tts` ElevenLabs backend (`--backend elevenlabs` /
`LANGSTREAM_TTS_BACKEND=elevenlabs`), verified live against the real
ElevenLabs API (the only TTS vendor key available in this session; no
Cartesia key exists). LangStream now has three real TTS backends
(Cartesia, ElevenLabs) plus mock.

**Real bug found and fixed live, during end-to-end testing with real
Sarvam + real ElevenLabs through the actual gateway (not just mocks):**
pushing every individual 20ms RTP-derived audio frame straight into
`Session.Push{Caller,Agent}Audio` (the naive, obvious approach) worked
fine for a one-shot `demo` (which explicitly `Close()`s the ASR session,
and Sarvam responds to the resulting flush signal) but silently never
finalized any utterance in a real, ongoing, never-closed room. Root-caused
by isolating chunk size as the only variable between a working and a
silently-broken run against the live Sarvam endpoint: Sarvam's own
server-side VAD needs each individual message's audio to span a large
enough window to detect a speech/silence transition within -- 20ms is too
short. Fixed with `pkg/webrtcgw/inbound_buffer.go`'s `inboundBuffer`:
accumulates ~400ms of decoded PCM before pushing into the Session (with a
unit-tested guarantee that whatever's left over is still flushed when a
track ends, so a real hangup mid-utterance doesn't silently drop the last
words). See DEVLOG.md's 2026-07-14 entry for the full investigation.

Verified end-to-end with two real (headless, pion-based) WebRTC clients
against the actual HTTP server, real ICE/DTLS/SRTP, real G.711 RTP both
directions, real Sarvam ASR, real ElevenLabs TTS, real Hindi speech audio
(GPT-4o/OpenAI untestable from this sandbox specifically -- see below --
so translation used the mock translator for this particular live run;
`go test`'s own suite uses a deterministic in-process ASR/MT/TTS stand-in
throughout, no live vendor calls in CI).

**Known environment constraint, not a code issue:** this sandbox's network
blocks `api.openai.com` at a security-gateway level (confirmed via a
direct HTTP request returning a Cisco Secure Access block page) --
`gpt4o`-backed translation could not be live-tested from here. Untested
here does not mean broken: the GPT-4o client itself is unchanged from
Week 2's already-tested implementation. Saurabh is continuing this test
on his own machine, where that domain is reachable.

## Explicitly out of scope for this month
- More than one language pair
- Self-hosted models (Whisper/NLLB/Coqui) for cost reduction — stay on
  paid APIs until the pilot proves the product is worth optimizing
- Multi-tenant provisioning, billing, or a customer-facing console
- Voice cloning / preserving the original speaker's timbre
- Any claim of "GA" — this roadmap produces a pilot, not a shippable product
