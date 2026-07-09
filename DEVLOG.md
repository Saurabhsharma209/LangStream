# LangStream Dev Log

## 2026-07-07 — Week 1 verification + a second Day-1 bug

Saurabh asked to confirm Week 1 was fully executed. Ran a fresh `git clone`
of the pushed repo (not the working copy) to verify what actually landed on
GitHub, since a clean clone is the only way to catch "works locally, broke
on push" issues.

**Bug found: `.gitignore` silently dropped two whole packages.** The
original `.gitignore` had a bare `langstream` pattern intended to ignore the
compiled binary (`go build ./cmd/langstream` drops a `langstream` binary in
the repo root). Git's gitignore semantics match an unanchored pattern like
that against *any* file or directory of that name at *any* depth - so it
also matched, and silently excluded, `pkg/langstream/` and `cmd/langstream/`
themselves. Net effect: the first two pushes (Sprint 1 + the DEVLOG-only
follow-up commit) never contained the orchestrator package at all, and
GitHub Actions CI failed on both runs as a result (`go build ./...` on a
clean checkout correctly failed with "no required module provides package
.../pkg/langstream").

This is the same class of problem as the Session.Close() bug from Sprint 1:
it was invisible locally (the working copy still had the files on disk;
`git status --ignored` would have shown it, but nothing forced that check)
and only surfaces when you verify from a clean external checkout instead of
trusting the working directory.

**Fix:** anchored the pattern to the repo root (`/langstream` instead of
`langstream`), force-added the two packages, verified `go build ./... && go
vet ./... && go test ./... -race && gofmt -l .` clean from a fresh clone,
committed, and pushed. GitHub Actions CI should go green on this commit -
worth a manual check on the Actions tab to confirm, since this is exactly
the kind of thing that should be caught by CI but wasn't (CI itself was one
of the things silently dropped).

**Process fix for future sprints:** the daily scheduled task already clones
fresh each run (rather than reusing a working copy), which would have
caught this automatically going forward - this bug was specific to the
first manual push in this session.

**Week 1 (ROADMAP.md) is now confirmed complete and verified from a clean
clone:** stable ASR/MT/TTS interfaces, deterministic mocks, the duplex
session orchestrator with VAD and persona management, CI, latency
instrumentation, and a cross-workstream integration test suite - all built,
tested, and now actually present on `main`.

## 2026-07-07 — Sprint 1 (Roadmap Days 1-3, Week 1 foundations)

**Agents run:** PM+EM (orchestrator), PE, Tech, SRE, QA
**Build:** ✅ passing (`go build ./...`, `go vet ./...`, `go test ./... -race` x3, `gofmt -l .` clean)

### Changes
- Repo scaffold + `go.mod` (`github.com/exotel/langstream`)
- `pkg/asr/interface.go`, `pkg/translate/interface.go`, `pkg/tts/interface.go` — stable
  streaming interfaces every vendor backend implements (EM)
- `pkg/asr/mock.go`, `pkg/translate/mock.go`, `pkg/tts/mock.go` + tests — deterministic
  mock backends for hi/en, race-tested (PE)
- `pkg/langstream/session.go` — duplex orchestrator (`Session`, `NewSession`,
  `PushCallerAudio`/`PushAgentAudio`, `AgentHearsAudio`/`CallerHearsAudio`, `Close`) (Tech)
- `pkg/langstream/vad.go` — RMS-based voice activity + utterance-boundary detection (Tech)
- `pkg/langstream/personas.go` — per-language voice persona manager (Tech)
- `cmd/langstream/main.go` — `version` + `demo` CLI subcommands (Tech)
- `pkg/rtp/doc.go` — Week 2 duplex-RTP extension plan, skeleton only this week (Tech)
- `pkg/observability/metrics.go` — thread-safe latency recorder with real percentile
  math + Prometheus-text-format export (SRE)
- `Dockerfile`, `docker-compose.yml`, `Makefile`, `.github/workflows/ci.yml` (SRE)
- `langstream_integration_test.go` — first cross-workstream integration tests, wiring
  PE's real mocks into Tech's real orchestrator (QA)
- `tools/latency_benchmark/` — standalone latency benchmark harness + README (QA)
- `README.md`, `ROADMAP.md`, `references/workstreams.md`, `.gitignore` (PM/EM)

### Bug found and fixed (Day 1)
QA's integration test caught a real bug in `Session.Close()`: it cancelled the
session context *before* closing the ASR streams, which raced each backend's
Close()-time flush of its final buffered transcript against an already-cancelled
context — silently dropping the last utterance spoken before every call hangup
(100% reproduction). Fixed by EM: close ASR streams first, wait (bounded, 3s
backstop) for both leg goroutines to drain the flush, cancel context last. Test
renamed from `TestSessionClose_DropsFinalUtteranceOnHangup` (characterized the
bug) to `TestSessionClose_FlushesFinalUtteranceOnHangup` (guards the fix) — see
`pkg/langstream/session.go` `Close()` doc comment for the full explanation. This
is exactly the class of bug an integration test catches and a unit test can't:
each package was individually correct; the composition wasn't.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all packages pass, no flakes
- `gofmt -l .` — clean
- Fixed regression test re-run 10x under `-race` — stable
- `tools/latency_benchmark` runs end-to-end against mocks (numbers are not
  meaningful yet — see caveat printed by the tool itself — but the harness
  exists and works, which is the Week 1 goal)

### Blocked
- No real vendor API keys yet (Deepgram/Sarvam/OpenAI/Cartesia) — Week 2
  blocker, tracked in ROADMAP.md, not urgent today.

### Ops note
- Pushed to github.com/Saurabhsharma209/LangStream (main) using a PAT
  from Saurabh. A recurring 9am daily scheduled task (`langstream-daily-build`)
  now runs this same PE/Tech/SRE/QA loop automatically, compressing ~3
  roadmap-days per run, and pushes at the end of each run.

### Tomorrow (Sprint 2, Roadmap Days 4-6)
1. Push Sprint 1 to GitHub once credentials are available; wire CI to actually run
2. Start Week 2: real Deepgram (English) + Sarvam (Hindi) streaming ASR behind `pkg/asr`
3. Real GPT-4o streaming translation behind `pkg/translate`
4. Begin the duplex RTP extension of ClearStream's `pkg/rtp.Session` (highest-risk item — start early)

## 2026-07-08 — Sprint 2 (Roadmap Days 6-8, Week 2 real pipeline)

**Agents run:** EM (orchestrator) + PE-ASR, PE-Translate, PE-TTS, Tech (parallel batch 1), then QA (batch 2, after PE/Tech landed)
**Build:** ✅ passing (`go build ./...`, `go vet ./...`, `go test ./... -race -count=3`, `gofmt -l .` all clean)

### Changes
- `pkg/asr/deepgram.go`, `pkg/asr/sarvam.go`, `pkg/asr/backoff.go` — real streaming ASR
  clients for Deepgram (English) and Sarvam (Hindi, code-switching aware via `mode=codemix`),
  protocol verified against vendor docs via web search, `WithBaseURL` for testability,
  exponential-backoff reconnect logic (PE-ASR)
- `pkg/translate/gpt4o.go` — real GPT-4o streaming (SSE) translation client, Hindi↔English,
  Hinglish-aware system prompt, `WithBaseURL`/`WithAPIKey`/`WithModel` options (PE-Translate)
- `pkg/tts/cartesia.go`, `pkg/tts/cartesia_ws.go`, `pkg/tts/cartesia_voices.go` — real
  Cartesia streaming TTS client (hand-rolled stdlib WebSocket client, since `go.mod` had zero
  deps and adding one was outside this agent's file ownership), persona→voice mapping
  compatible with `pkg/langstream/personas.go`'s `"default-"+lang` convention (PE-TTS)
- `pkg/langstream/backends.go` — name-based backend registry (`RegisterASRBackend`,
  `NewASRBackend("deepgram")`, etc.) so real/mock backends are selected by name without the
  CLI needing to import vendor constructors directly; `cmd/langstream/main.go` got a
  `--backend` flag + `LANGSTREAM_{ASR,MT,TTS}_BACKEND` env vars (Tech)
- EM wired the four real vendor constructors into the registry post-hoc (`cmd/langstream/main.go`
  `init()`) once their exact names were known, and verified `langstream demo --backend deepgram`
  fails cleanly with a "DEEPGRAM_API_KEY not set" error (no panic) with no key present, and that
  env-var-only leg overrides (`LANGSTREAM_MT_BACKEND=gpt4o langstream demo`) resolve correctly
- `integration_vendor_test.go` — fake-server Hindi→English round-trip test wiring real
  Sarvam/GPT-4o/Cartesia clients into a real `langstream.Session`, plus two adversarial tests
  (ASR fatal error mid-stream, malformed TTS frame) proving the orchestrator degrades instead
  of hanging or panicking (QA)
- `tools/latency_benchmark` — additive `-vendor-fake` flag to measure round-trip latency
  against fake-server-backed real clients instead of only Week 1 mocks (QA)
- `go.mod`/`go.sum` — added `github.com/gorilla/websocket` (Deepgram/Sarvam client + test fakes)

### Bug found and fixed (PE-ASR, same-day)
Both `deepgram.go` and `sarvam.go` initially deadlocked on a fatal vendor error frame:
`failAndClose` was called synchronously from inside the `readLoop` goroutine and then called
`workerWG.Wait()`, which waited on that same goroutine's own `Done()` — never arriving. Fixed
by moving the wait-and-close teardown into a separate goroutine. Caught by PE-ASR's own
vendor-error-frame test under `-race`, confirmed with 10x re-runs.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all packages pass, no flakes
- `gofmt -l .` — clean
- Manual CLI smoke test: `langstream demo --backend mock` (works end-to-end),
  `langstream demo --backend deepgram` with no API key (fails with a clear, non-panicking
  error), `LANGSTREAM_MT_BACKEND=gpt4o langstream demo` (per-leg env override resolves
  correctly)
- QA's fake-server Hindi→English round trip passes; adversarial ASR-error and malformed-TTS
  tests both confirm bounded, non-hanging degradation

### ClearStream coordination checkpoint (duplex RTP) — needs Saurabh's input
Checked ClearStream's latest tag before starting (`git ls-remote --tags` → still `v0.1.0`, no
new release since 2026-07-07) and read its `pkg/rtp/session.go` and `pkg/rtp/playback.go` in
full. Finding: ClearStream's `rtp.Session` is a single-leg, network-to-network audio
pass-through (UDP in → jitter buffer → noise-suppression pipeline → UDP out), not a
PCM-in/PCM-out library call. It does export `InjectBotAudio(pcm16 []byte) bool` — a queue-based
hook for injecting synthesized audio into the *outbound* RTP stream — which would actually cover
LangStream's TTS→agent direction as-is, no ClearStream change needed there. But there is **no
exported hook for the reverse direction**: the caller's decoded, noise-suppressed PCM is
consumed entirely inside `handlePacket` and re-encoded straight back to RTP; nothing in the
public API surfaces it for an external consumer like LangStream's ASR leg to read.

**This means duplex RTP is not a clean `go.mod`-only import** — the ASR-in direction needs an
actual (small, additive) ClearStream code change, e.g. an optional
`Config.OnCleanAudio func([]int16, sampleRate int)` callback fired alongside the existing
forward-to-UDP path. Per the standing cross-repo rule, that change was NOT attempted this run —
no ClearStream files were touched, no ClearStream commit was made. This is flagged for Saurabh
as a real decision point, not something the automation resolved unilaterally: does he want to
(a) scope and review a ClearStream PR adding that callback, (b) have LangStream duplicate a
lightweight RTP receive path of its own instead of extending ClearStream's, or (c) defer duplex
RTP and pursue Week 3/4 items first with ClearStream feeding audio in some other way (e.g. a
recording/webhook path) for the pilot's initial cut. `pkg/rtp/doc.go`'s Week 2 plan already
anticipated needing to "compose two ClearStream-style single-leg Session instances" — that
composition is fine for the TTS-out leg but not sufficient for the ASR-in leg without the above.

### Blocked
- Still no real vendor API keys (Deepgram/Sarvam/OpenAI/Cartesia) — expected per the Week 2
  decision, not a new blocker. Fake-server tests prove the client code is correct; a real-key
  smoke test is the only thing left once keys exist.
- Duplex RTP (see coordination checkpoint above) — blocked on Saurabh's decision, not on agent
  capacity.

### Tomorrow (Sprint 3, Roadmap Days 9-10 pending Saurabh's RTP decision)
1. Get a decision from Saurabh on the ClearStream `OnCleanAudio`-style callback (or the
   alternative approaches above) so duplex RTP can be scoped
2. If vendor API keys become available, add real-network smoke tests on top of the existing
   fake-server tests (client code itself should not need to change)
3. Start Week 3 hardening items that don't depend on the RTP decision: jitter buffer tuning
   groundwork, fallback/degrade-gracefully behavior design, `docs/compliance.md` DPDP
   assessment skeleton

## 2026-07-09 — Sprint 3 (Roadmap Days 9-11, Week 3 hardening start)

**Agents run:** PM+EM (orchestrator) + Tech, SRE (parallel batch 1), then QA (batch 2, after Tech/SRE landed)
**Build:** ✅ passing (`go build ./...`, `go vet ./...`, `go test ./... -race -count=3`, `gofmt -l .` all clean)

### ClearStream coordination — still blocked, no change
Checked ClearStream's latest tag before doing anything else (`git ls-remote
--tags` → still `v0.1.0`, no new release since 2026-07-07/08). The
2026-07-08 finding stands: duplex RTP needs an actual (small, additive)
ClearStream code change (an `OnCleanAudio`-style callback for the
caller→ASR direction) that hasn't been authorized. Not attempted again
this run, per the standing cross-repo rule. **Still needs Saurabh's
decision** — see DEVLOG.md 2026-07-08 for the three options. Today's sprint
moved to Week 3 items that don't depend on that decision instead of
blocking on it.

### Changes
- `pkg/langstream/fallback.go`, edits to `pkg/langstream/session.go` —
  real graceful-degradation behavior: low ASR confidence, MT/TTS errors,
  and bounded timeouts now fall back to original-audio passthrough
  (optional synthesized warning tone) instead of dropping the utterance;
  repeated failures (`MaxConsecutiveFailures`, default 3) or a `FatalError`
  permanently degrade a leg (`CallerLegDegraded()`/`AgentLegDegraded()`)
  without crashing or hanging on subsequent audio (Tech)
- `pkg/rtp/jitter.go` — transport-agnostic jitter buffer (sequence
  wraparound, reordering, duplicate/late-packet handling, loss policy,
  capacity-bounded eviction), tested against a seeded simulated
  PSTN-like condition (jitter + reordering + 3% loss). Explicitly
  groundwork — no real transport behind it yet, not claimed as "tuned
  against real PSTN conditions" (Tech)
- `pkg/observability/metrics.go` extended + new `pkg/observability/dashboard.go`
  — error-rate and per-vendor cost tracking added to the existing
  `LatencyRecorder`, exported via Prometheus text and a real HTTP
  dashboard (`NewDashboardServer`: `/`, `/dashboard.json`, `/metrics`),
  fully tested via `httptest` including concurrent-use race tests (SRE)
- `fallback_integration_test.go`, `observability_dashboard_integration_test.go`
  — cross-workstream integration tests wiring Tech's fallback logic
  through a real `Session` + real mock backends, and SRE's dashboard
  through a real HTTP server fed by a real recorder driven by session
  activity; verifies the pieces actually compose, not just that each
  compiles alone (QA)
- `docs/compliance.md` — new. Preliminary DPDP data-residency assessment
  (finding: RBI localization rules, not DPDP itself, are the likely
  binding constraint for a BFSI anchor customer) and consent/disclosure
  language draft for AI-translated calls, both explicitly flagged as
  pending legal sign-off, not a compliance clearance (PM)
- `ROADMAP.md` — checked off Fallback behavior, Observability dashboard,
  DPDP assessment, and consent language; left jitter buffer and vSIP
  example unchecked with accurate status notes (PM)

### Bugs found/fixed
None. QA's integration tests (low-confidence passthrough, fatal-error
immediate degrade, repeated-failure threshold degrade, dashboard
end-to-end reflecting real session activity over real HTTP) all passed
against Tech's and SRE's code as written, first try. Re-ran QA's new
tests at `-count=10 -race` with no flakes.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all packages pass, no flakes
- `gofmt -l .` — clean
- QA's new integration tests specifically re-run at `-count=10 -race`

### Blocked
- Duplex RTP (and therefore full jitter-buffer PSTN tuning) — still
  blocked on Saurabh's ClearStream decision, unchanged since 2026-07-08.
- Vendor API keys still not available — not a blocker for this week's
  remaining items, same as prior sprints.

### Tomorrow (Sprint 4, Roadmap Days 12-13)
1. Get Saurabh's decision on the ClearStream `OnCleanAudio`-style callback
   (or the alternative approaches from 2026-07-08) — this is now blocking
   two roadmap items (duplex RTP itself, and real jitter-buffer tuning)
2. Wire `observability.NewDashboardServer` into `cmd/langstream`'s actual
   binary (small, Tech-owned integration task, not new work)
3. Exotel vSIP integration example (last unchecked non-RTP-dependent
   Week 3 item — confirm it doesn't actually need duplex RTP before
   starting; if it does, it's also blocked on the same decision)
4. Legal review pass on `docs/compliance.md` (outside engineering's
   ability to close — flag to Saurabh as a non-engineering dependency)
