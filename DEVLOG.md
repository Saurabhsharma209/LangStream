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

## 2026-07-10 — Sprint 4 (Roadmap Days 12-13, Week 3 continued)

**Agents run:** PM+EM (orchestrator) + Tech, SRE (parallel batch 1), then QA (batch 2, after Tech/SRE landed)
**Build:** ✅ passing (`go build ./...`, `go vet ./...`, `go test ./... -race -count=3`, `gofmt -l .` all clean)

### ClearStream coordination — still blocked, no change
Checked ClearStream's latest tag before doing anything else (`git ls-remote
--tags` → still `v0.1.0`, no new release since 2026-07-07/08/09). The
2026-07-08 finding stands unchanged: duplex RTP needs an actual, small,
additive ClearStream code change (an `OnCleanAudio`-style callback for the
caller→ASR direction) that hasn't been authorized. Not attempted this run,
per the standing cross-repo rule. No ClearStream files touched, no
ClearStream commit made. **Still needs Saurabh's decision** — see
DEVLOG.md 2026-07-08 for the three options; nothing new to add today.
Because this blocks both real jitter-buffer tuning and true end-to-end
vSIP wiring, today's sprint intentionally scoped around it rather than
waiting on it.

### Changes
- `cmd/langstream/main.go` — new `serve` subcommand (`--addr`, default
  `:8080`) that builds a real `langstream.Session` via a shared `newSession`
  helper (also refactored `runDemo` onto it) and mounts
  `observability.NewDashboardServer` on it, with graceful SIGINT/SIGTERM
  shutdown via a testable `serveDashboard(ctx, srv) error` helper. This is
  the CLI wiring flagged as a next-sprint task in the 2026-07-09 entry
  (Tech)
- `examples/vsip_example/` (new) — `VSIPCallAdapter` contract/shape example
  showing how Exotel vSIP audio would push into / read out of a real
  `langstream.Session`. Explicitly documented as NOT including real SIP/RTP
  socket plumbing or ClearStream duplex-RTP integration — those remain
  blocked on the 2026-07-08 decision. Deliberately not claimed as
  "end-to-end" (Tech)
- `Dockerfile`, `docker-compose.yml` — port 8080 comment updated from
  "reserved for the future" to documenting the live dashboard; compose now
  runs `command: ["serve", "--addr", ":8080"]` instead of falling into
  `main()`'s no-args usage-and-exit path (which would have crash-looped
  under `restart: unless-stopped`); stale "Week 1 mock backends only" env
  comment corrected. `HEALTHCHECK NONE` added with a documented reason
  (distroless nonroot base has no shell/curl/wget for an in-image
  healthcheck; real health checking belongs at the k8s/compose
  orchestrator level, or would need a new `langstream healthcheck`
  subcommand as future Tech work) (SRE)
- `.github/workflows/ci.yml` — new parallel, non-blocking (`continue-on-error:
  true`) `docker-build` job actually building the Dockerfile in CI, so a
  broken image doesn't silently rot; existing build-test job untouched (SRE)
- `Makefile` — `make serve`, `make docker-run` targets (SRE)
- `pkg/qa/` (new package) — `WordErrorRate` (edit-distance-based WER,
  unit-tested against known-answer cases) + a small fixed English test
  corpus, plus a root-level `wer_measurement_test.go` wiring WER
  measurement against the existing fake-Sarvam-ASR test infrastructure.
  This is the first piece of the WER/accuracy regression suite the QA
  charter has called for since real ASR backends landed in Sprint 2 —
  explicitly flagged in comments as groundwork against fakes, not the
  live-traffic measurement Week 4 ultimately needs (QA)
- `cmd/langstream/serve_integration_test.go` — real end-to-end test:
  pushes genuine audio through a `Session`, confirms `/dashboard.json`
  and `/metrics` reflect real recorded activity (not a hand-populated
  recorder), plus a real-binary subprocess test that starts `serve`,
  hits it over real HTTP, sends real SIGTERM, and asserts bounded-time
  graceful exit (QA)
- `examples/vsip_example/adapter_content_test.go` — extends Tech's own
  adapter test (which already checked "≥1 chunk, last one final") with
  exact chunk-count and exact-PCM-content assertions against the
  deterministic mock backends, so a leg-swap or corrupted-frame bug in
  the adapter's plumbing would actually fail a test (QA)

### Bugs found/fixed
None. QA's integration tests (dashboard-over-real-HTTP, real-binary
SIGTERM shutdown, vSIP adapter content correctness, WER wiring against
fake ASR) all passed against Tech's and SRE's code as written, first try.
Re-ran all new/changed tests at `-race -count=10` with no flakes. One
non-bug observation worth carrying forward: `Session` only ever calls
`RecordEvent`/`RecordError` on the metrics recorder, never `Record`/
`RecordStage` — so session activity currently shows up under the
dashboard's error/event tracking, not its latency-percentile view. Not
wrong, but worth knowing before anyone builds a "real session traffic ⇒
latency percentiles move" expectation into a demo or pilot review.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all packages pass, no flakes, including
  the two new packages (`cmd/langstream`, `examples/vsip_example`) and
  the new `pkg/qa` package
- `gofmt -l .` — clean
- `git status --porcelain` / `git add -A -n` checked by QA specifically to
  rule out a repeat of the Sprint 1 `.gitignore` bug — all new files
  (`pkg/qa/`, `examples/vsip_example/`, new test files) are correctly
  trackable, not excluded
- Docker itself is not available in this sandbox — SRE verified
  Dockerfile/compose/CI YAML by inspection and YAML-parsed both files,
  but could not run `docker build` directly; flagged for someone with
  Docker access to sanity-check once before this reaches production

### Blocked
- Duplex RTP, real jitter-buffer PSTN tuning, and true end-to-end vSIP
  wiring — all still blocked on Saurabh's ClearStream decision, unchanged
  since 2026-07-08.
- Vendor API keys still not available — not a blocker for this week's
  remaining items, same as prior sprints.
- Docker-build verification needs a human (or a sandbox with Docker) to
  actually run `docker build` once; CI's new job is non-blocking until
  proven stable.
- Legal review of `docs/compliance.md` — outside engineering's ability to
  close, unchanged since 2026-07-09.

### Tomorrow (Sprint 5, Roadmap Days 14-15)
1. Get Saurabh's decision on the ClearStream `OnCleanAudio`-style callback
   (or the alternative approaches from 2026-07-08) — this is now the
   single blocker on three remaining Week 3 items (duplex RTP itself,
   real jitter-buffer tuning, true end-to-end vSIP wiring)
2. Have someone with Docker access run `docker build -t langstream:ci .`
   once against today's Dockerfile/compose changes, then flip the CI
   `docker-build` job from informational to blocking
3. Expand `pkg/qa`'s WER corpus and add a Hindi/code-switching case (Sarvam
   fake server already supports it) now that the harness exists
4. Legal review pass on `docs/compliance.md` — still a non-engineering
   dependency, still open

## 2026-07-12 — Sprint 5 (Roadmap Days 14-15+ groundwork, Week 3 continued)

**Agents run:** PM+EM (orchestrator) + Tech (batch 1), then QA (batch 2, after Tech landed)
**Build:** ✅ passing (`go build ./...`, `go vet ./...`, `go test ./... -race -count=3`, `gofmt -l .` all clean)

### ClearStream coordination — still blocked, no change
Checked ClearStream's latest tag first (`git ls-remote --tags` → still
`v0.1.0`, no new release since 2026-07-07/08/09/10). The 2026-07-08 finding
stands unchanged: duplex RTP needs an actual, small, additive ClearStream
code change (an `OnCleanAudio`-style callback for the caller→ASR direction)
that hasn't been authorized. Not attempted this run, per the standing
cross-repo rule. No ClearStream files touched, no ClearStream commit made.
**Still needs Saurabh's decision** — unchanged since 2026-07-08, now the
single blocker on real jitter-buffer PSTN tuning and true end-to-end vSIP
wiring (both still unchecked in Week 3). Because both remaining Week 3
checklist items are gated on this, today's sprint scoped around
unblocked groundwork instead: closing a real observability gap, hardening
jitter-buffer test coverage, and expanding the WER corpus. Only Tech and
QA were spawned — PE had no vendor-facing work queued and SRE's one open
item (a human/CI environment with Docker running `docker build` once) is
still not actionable from this sandbox (confirmed again: no `docker`
binary available here, same as 2026-07-10).

### Changes
- `pkg/langstream/fallback.go`, `pkg/langstream/session.go` — real
  per-stage latency instrumentation wired into `Session`: `"mt"` (real
  `Translator.Translate` duration), `"tts_first_chunk"` (time to first
  synthesized chunk), `"asr_first_chunk"` (utterance-start-to-final-
  transcript), and `"total"` (full utterance glass-to-glass, recorded for
  both successful and passthrough/degraded utterances) now all flow into
  `session.Metrics()` via the existing `Record`/`RecordStage` API that
  previously went unused (flagged as a "worth knowing" gap in the
  2026-07-10 entry: the dashboard's latency-percentile view only ever
  reflected hand-populated test data, never real session traffic). New
  `pkg/langstream/latency_test.go` unit-tests the wiring directly,
  including that a passthrough utterance correctly skips the
  never-attempted stages while still recording `"total"` (Tech)
- `pkg/rtp/jitter_test.go` — three new stress-test scenarios beyond the
  existing single seeded condition: ~13% packet loss (vs. the prior 3%
  baseline), bursty multi-position reordering (packets shuffled up to 7
  positions within 8-packet windows, not just adjacent swaps), and a
  sudden mid-stream jitter spike (~300ms hiccup the fixed `TargetDelay`
  can't absorb) — all asserting no panic, bounded memory
  (`MaxPacketsBuffered`), and monotonic, duplicate-free playout. Still
  simulation-only groundwork; no new production config fields added (Tech)
- `dashboard_latency_integration_test.go` (new, root) — cross-workstream
  integration test independently verifying the latency-wiring gap above
  is actually closed *from the dashboard's perspective*, not just the
  recorder's: builds a real `Session` with a deliberately-delayed mock
  translator, drives one utterance round trip, hits a real
  `NewDashboardServer` over real HTTP, and confirms `/dashboard.json`
  shows non-zero counts for all four stages (and only `"total"` for a
  forced passthrough utterance) — matching Tech's own unit-level
  contract at the next layer up (QA)
- `pkg/qa/corpus.go`, `pkg/qa/corpus_test.go`, `wer_measurement_test.go` —
  3 new Hindi/English code-switching (Hinglish) WER corpus entries
  (explicitly flagged as the next-sprint QA priority in the 2026-07-10
  entry), wired into both the hand-computed corpus tests and the
  real-fake-Sarvam-backed measurement test. Measured WER: 0.0 (identical),
  0.1667 (1-word substitution), 0.1429 (1-word deletion) — in the same
  plausible range as the existing English single-error cases, and
  confirms `WordErrorRate`'s tokenization handles a Devanagari/English
  script boundary correctly rather than mis-splitting multi-byte runes
  (QA)

### Bugs found/fixed
None. QA's independent dashboard-level integration test passed against
Tech's latency-wiring code first try, closing the 2026-07-10 observation
cleanly. QA also reviewed (did not modify) Tech's 3 new jitter stress
tests: found them sound (each asserts a real invariant, not just
"doesn't panic") but flagged one non-blocking gap for a future sprint —
none of the three assert a full `played + lost ≈ n` packet-accounting
invariant, so a regression that silently dropped packets without
incrementing `stats.Lost` wouldn't be caught directly today (it's
partially covered indirectly by existing lower-bound checks). Worth
tightening in a future sprint, not urgent.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all 10 packages pass, no flakes
- `gofmt -l .` — clean
- `git add -A -n` checked before committing — all 8 changed/new files
  (`pkg/langstream/fallback.go`, `pkg/langstream/session.go`,
  `pkg/langstream/latency_test.go`, `pkg/rtp/jitter_test.go`,
  `pkg/qa/corpus.go`, `pkg/qa/corpus_test.go`, `wer_measurement_test.go`,
  `dashboard_latency_integration_test.go`) correctly trackable, not
  excluded by `.gitignore` — same class of check that caught the Sprint 1
  bug, repeated deliberately every run
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Duplex RTP, real jitter-buffer PSTN tuning, and true end-to-end vSIP
  wiring — all still blocked on Saurabh's ClearStream decision, unchanged
  since 2026-07-08. This is now 4+ sprints without a decision; flagging
  prominently again in today's report.
- Vendor API keys still not available — not a blocker for this week's
  remaining items, same as prior sprints.
- Docker-build verification still needs a human (or a sandbox with
  Docker) to run `docker build` once — confirmed again this sandbox has
  no `docker` binary, unchanged since 2026-07-10.
- Legal review of `docs/compliance.md` — outside engineering's ability to
  close, unchanged since 2026-07-09.

### Tomorrow (Sprint 6)
1. Get Saurabh's decision on the ClearStream `OnCleanAudio`-style callback
   (or the alternative approaches from 2026-07-08) — this is the single
   blocker on the last two Week 3 items and, transitively, all of Week 4
2. If a decision arrives: scope and execute the ClearStream-side PR (as
   its own separately-reviewed change, per COMBINED_ROADMAP.md's standing
   agreement) and/or the LangStream-side duplex RTP session work
3. If no decision yet: tighten the jitter-buffer stress tests' packet-
   accounting invariant (`played + lost ≈ n`) per QA's note above, and/or
   continue expanding the Hinglish WER corpus with harder cases
4. Docker-build verification still needs a human with Docker access; flip
   CI's `docker-build` job from informational to blocking once done

## 2026-07-12 (unblock + duplex RTP) — interactive run, not a scheduled sprint

Saurabh messaged directly ("now you are unblocked check") after ClearStream's
own independent daily automation resolved the standing OnCleanAudio decision.
This entry covers that interactive session, run the same day as (and after)
today's regular Sprint 5 scheduled run above.

### ClearStream coordination — RESOLVED
Checked ClearStream's repo: still tagged `v0.1.0`, but `main` has moved past
it. Commit `4d5ea467888c97a61d501efe33ba271b039f3348` ("[RTP-SIP] Add
Session.CleanAudio() channel API for real-time clean-audio hand-off to
LangStream") resolves the decision blocking LangStream since 2026-07-08:
`rtp.Session.CleanAudio() <-chan CleanAudioFrame`, opt-in via
`Config.CleanAudioBufferSize` (0 = disabled/default), delivering owned
copies of post-suppression 16kHz PCM, non-blocking with drop-oldest-on-full
backpressure. ClearStream's own `ROADMAP.md` "Resolved Decisions" section
documents this and the two rejected alternatives (synchronous OnDTMF-style
callback; LangStream forking its own RTP loop). No ClearStream code was
touched by LangStream's automation — this was entirely ClearStream's own
daily automation's work, exactly per COMBINED_ROADMAP.md's standing
agreement.

### Changes
- `go.mod`, `go.sum`, `VERSIONING.md` — pinned `github.com/exotel/clearstream`
  at the exact resolving commit via a pseudo-version
  (`v0.0.0-20260712052406-4d5ea467888c`) plus a `replace` directive
  (ClearStream's own `go.mod` declares module path `github.com/exotel/
  clearstream`, which isn't its actual GitHub location —
  `github.com/Saurabhsharma209/ClearStream` — so a plain `require` can't
  resolve it). No ClearStream semver tag exists past this commit yet;
  `VERSIONING.md` flags switching to a real tag as a follow-up once one
  exists (EM)
- `pkg/rtp/duplex.go` (new) — `DuplexSession`: composes two ClearStream
  `rtp.Session` instances (caller leg, agent leg) bridged to a
  `*langstream.Session`: `CleanAudio()` → `asr.AudioFrame` →
  `Push{Caller,Agent}Audio`, and `{Agent,Caller}HearsAudio()` →
  `InjectBotAudio` (already-existing ClearStream API, no conversion
  needed — same 16-bit LE PCM byte layout both sides already use).
  `NewDuplexSession`/`Start`/`Stop` lifecycle, 4 bridging goroutines, PCM
  int16↔bytes conversion helpers. This is the actual Week 2 "Extend
  ClearStream's pkg/rtp session for bidirectional media" roadmap item,
  the single highest-risk item on the whole roadmap, done (Tech)
- `pkg/rtp/duplex_test.go` (new, Tech) — PCM conversion unit tests plus
  `TestDuplexSession_EndToEndLoopback`: real loopback UDP RTP packets sent
  into the caller leg, real langstream.Session with mock ASR/MT/TTS,
  confirms real synthesized RTP comes out the agent leg's forward socket.
  (Tech's own agent run hit a stream-timeout mid-task with this test left
  as a placeholder `t.Fatal("unreachable...")` — the EM finished it,
  adding a `newLoopbackPort` helper to get a concrete UDP port ClearStream
  doesn't otherwise expose externally, matching the port-discovery
  approach the interrupted agent had already started reasoning through in
  its own comments)
- `pkg/rtp/duplex_bidirectional_test.go`,
  `pkg/rtp/duplex_backpressure_test.go`,
  `pkg/rtp/duplex_shutdown_test.go`, `pkg/rtp/duplex_construct_test.go`
  (all new) — QA's independent integration testing: concurrent
  bidirectional traffic (both legs active at once), backpressure/drop-
  oldest under flood with a goroutine-leak check, shutdown-ordering edge
  cases, and the `NewDuplexSession` agent-leg-construction-failure path
  (confirms the caller leg's socket really is released). (QA)

### Bugs found/fixed
**Real bug: `Start()`/`Stop()` data race, found by QA, fixed by EM same
day.** QA's `TestDuplexSession_StopConcurrentWithStart` (calling `Start()`
and `Stop()` concurrently from separate goroutines, simulating a caller
racing its own startup against a near-simultaneous shutdown signal, e.g. a
SIP BYE) caught a genuine, `go test -race`-confirmed data race: `Start()`'s
`d.wg.Add(4)` and `Stop()`'s internal `d.wg.Wait()` goroutine ran under two
*independent* `sync.Once` guards (`startOnce`, `stopOnce`) with no
happens-before edge between them — if `Stop()` reached `wg.Wait()` before a
concurrent `Start()` reached `wg.Add(4)`, that's exactly the "Add with a
positive delta concurrent with Wait while the counter may still be zero"
pattern `sync.WaitGroup`'s own doc comment calls out as a data race.
Reproduced ~2/5 to 1/6 runs. Per QA's charter, QA reported this precisely
(exact mechanism, reproduction rate, stack trace) without touching
`duplex.go`. The EM fixed it: replaced the independent `atomic.Bool` +
`startOnce`/`stopOnce` pair with a single `lifecycleMu` mutex that `Start()`
holds for its *entire* body (including `wg.Add(4)` and starting both
ClearStream sessions) and that `Stop()`'s single (`stopOnce`-guarded) body
uses to atomically read `startedFlag`/set `stoppedFlag` before proceeding —
guaranteeing `Start()`'s `wg.Add(4)`, if it happens at all, always
happens-before any concurrent `Stop()`'s `wg.Wait()`. Verified fixed at
`go test -race -run TestDuplexSession_StopConcurrentWithStart -count=30`
(0 failures, was previously failing intermittently) and the full suite at
`-race -count=5` (0 failures). This is the third instance of this exact
class of bug in this codebase's history (Session.Close() Day-1 ordering
bug; the `.gitignore` fresh-clone bug; now this) — all three were caught
by deliberately adversarial verification (integration tests, fresh clones,
concurrent-call stress) rather than "it compiles and the happy path
passes."

No other bugs found. Bidirectional, backpressure, and construction-failure
tests all passed against Tech's code as written.

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=5` — all 10 packages pass, no flakes,
  including the race-fix regression test at an additional isolated
  `-count=30`
- `gofmt -l .` — clean
- `git add -A -n` — all 8 new/changed files correctly trackable
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked / follow-ups
- `DuplexSession` is not yet wired into `cmd/langstream`'s CLI or
  `examples/vsip_example`'s real SIP/socket address plumbing — that's
  real network/config work, deliberately scoped out of this run to keep
  the bridge itself (already the highest-risk item) reviewable on its own.
  Next concrete unblocked step.
- `pkg/rtp/jitter.go`'s groundwork is not yet wired into `DuplexSession`
  (which currently relies on ClearStream's own per-leg `JitterDepth`
  instead) — worth a decision on whether LangStream's own jitter buffer
  is still needed on top of ClearStream's, or was superseded by it.
- ClearStream has no tag past `v0.1.0` covering the `CleanAudio()` commit;
  `go.mod`'s pseudo-version pin should move to a real tag once one exists.
- `NewDuplexSession`'s agent-leg-construction-failure path depends on a
  ClearStream API gap (`Session.Stop()` before `Session.Start()` hangs;
  worked around via Start-then-Stop) — worth raising with ClearStream as
  a small upstream ask (e.g. a `Close()`-without-`Start()`, or deferring
  the UDP bind to `Start()`), not urgent.

### Tomorrow
1. Wire `DuplexSession` into `cmd/langstream`'s CLI and/or
   `examples/vsip_example`'s real socket plumbing — the concrete next
   step now that the bridge itself is proven
2. Decide whether `pkg/rtp/jitter.go` still has a role once real traffic
   exists, or whether ClearStream's own per-leg jitter buffer supersedes it
3. Continue the previously-planned WER corpus / jitter-stress-test
   tightening from today's earlier Sprint 5 entry

## 2026-07-13 (Sprint 7: vSIP end-to-end + TTS pacing + QA hardening) — scheduled run

### Agents run
Tech, QA (in parallel). PE/SRE not needed — today's scope (Week 3's two
remaining items) didn't touch `pkg/asr`/`pkg/translate`/`pkg/tts` or
CI/Docker/observability files.

### Repo health at start
Clean: `go build ./...`, `go vet ./...`, `go test ./... -race` (all 10
packages), `gofmt -l .` all clean before any changes. ClearStream checked
(`git ls-remote --tags` + GitHub API): still tagged only `v0.1.0`, latest
commit unchanged since 2026-07-12 (a docs-only DEVLOG entry) — no new
ClearStream work relevant today, no `VERSIONING.md` pin change needed.

### Changes

**Tech — vSIP example + CLI wired end-to-end (Week 3's last blocked item)**
- `cmd/langstream/duplex.go` (new): `langstream duplex` subcommand builds
  a real `rtp.DuplexSession` from CLI flags (both legs' listen/forward UDP
  addresses, payload type, jitter depth, suppressor backend), mounts the
  observability dashboard, graceful SIGINT/SIGTERM shutdown. Wired into
  `main()`/`usage()`.
- `examples/vsip_example/real_rtp.go` (new): `runRealRTPDemo` runs a real
  `rtp.DuplexSession` over real loopback UDP sockets (mock ASR/MT/TTS per
  the Week 2 decision — no vendor keys yet), called from `main()` after
  the existing shape-only `VSIPCallAdapter` demo.
- `cmd/langstream/duplex_test.go`, `examples/vsip_example/real_rtp_test.go`
  (new) — flag validation, construction-failure paths, real loopback
  end-to-end tests, dashboard on/off variants.

**Tech — jitter buffer repurposed as outbound TTS pacing (2026-07-12's
"does jitter.go still have a role" question, resolved)**
- Decision (EM, going into today's agent brief): ClearStream now
  jitter-buffers each leg's *inbound* audio internally before `CleanAudio()`
  hands off already-clean PCM, making `jitter.go`'s original inbound use
  case redundant. Repurposed the same `JitterBuffer` type as an *outbound*
  pacing/smoothing stage on the TTS→`InjectBotAudio` path instead — TTS
  synthesis is bursty, so pacing synthesized chunks before injection avoids
  choppy playback.
- `pkg/rtp/duplex.go`: `feedTTSPacer` (producer, tags each `tts.AudioChunk`
  with an incrementing `SeqNum`) + `runTTSPacer` (consumer, ticks at
  `DefaultTTSPacingInterval`=20ms, releases at most one chunk per tick).
  `Start()` now runs 6 bridging goroutines (was 4).
- `pkg/rtp/jitter.go`: new `ttsPacer` wrapper around `*JitterBuffer` with
  an atomic `pushed` counter, bounding `runTTSPacer` so it stops once all
  fed chunks are drained instead of ticking forever and manufacturing
  phantom "lost" chunks past end-of-stream — a real bug Tech's own test
  caught before landing (see Bugs below).
- `pkg/rtp/tts_pacing_test.go` (new, Tech): in-order/unmodified delivery,
  real time-spread pacing, delivery of a buffered chunk after the feed
  channel closes but before ctx cancellation.

**QA — jitter-buffer packet-accounting invariant tightened (QA's own
2026-07-12 follow-up)**
- `pkg/rtp/jitter_test.go`: added an `n int` parameter to the shared
  `driveJitterBufferSimulation` helper so it stops the instant
  `len(played)+lostCount == n` and hard-fails if that sum never reaches
  `n`. New `assertPacketAccounting` helper asserts **exact** equality
  (`played+lost == n`), with a comment on why exact (not fuzzy-tolerance)
  equality is correct: `Pull` always resolves exactly one sequence number
  per call, so a tolerance window would risk masking the exact silent-drop
  regression this check exists to catch.

**QA — Hinglish WER corpus expanded (6 → 15 entries)**
- `pkg/qa/corpus.go`, `pkg/qa/corpus_test.go`, `wer_measurement_test.go`:
  9 new hand-verified cases — mid-sentence code-switching, English
  loanwords in Hindi grammar, English digits/Hindi dates mixed, filler
  words, first insertion-only case, first 2-substitution case, a clean
  (WER 0.0) technical-jargon baseline. All wired into
  `TestFixedCorpus_PrecomputedWERMatches` and the fake-Sarvam-backed
  `TestWERMeasurement_FixedCorpusAgainstFakeASRBackedPipeline`.

### Bugs found/fixed

**Two real bugs, both found and fixed by Tech via Tech's own tests, before
landing (not flagged as cross-workstream issues — contained within Tech's
own owned files):**
1. First `runTTSPacer` draft drained *all* ready packets per tick instead
   of one — `JitterBuffer.Pull` returns immediately for an
   already-buffered packet regardless of deadline, so pacing only comes
   from calling `Pull` at most once per tick, not from `Pull` itself.
2. Unbounded `Pull`-forever bug: without a way to know "no more packets
   are coming," `runTTSPacer` would tick forever after the last real
   chunk, manufacturing phantom "lost" chunks for sequence numbers that
   never existed, for the rest of the process's life. Fixed with the
   `ttsPacer.pushed` atomic counter bound.
3. Shutdown-ordering bug (also Tech, own-files): `buildDuplexSession` was
   constructing `langstream.Session` against the same context that
   SIGINT/SIGTERM cancels, so the instant shutdown began, the Session's
   internal translate/synthesize goroutines abandoned the final-utterance
   flush before `Close()` could deliver it — defeating graceful shutdown
   silently. Fixed: `Session` now constructed against
   `context.Background()`; shutdown reordered to `sess.Close()` →
   `duplexFinalDrainGrace` (250ms) → `duplex.Stop()`. Same fix applied in
   `examples/vsip_example/real_rtp.go`.

**One measurement-harness finding, QA:** the pre-existing stress-test
helper drove `Pull` for extra "slack" ticks past the real end of each
simulated stream. Since `JitterBuffer` has no end-of-stream concept, those
extra ticks manufactured phantom `PullLost` events tied purely to each
test's arbitrary tick padding — e.g. `TestJitterBufferBurstyMultiPosition
Reordering` reported `Lost=10` even though its simulator never drops a
single packet (all 10 phantom, matching that test's `+10` padding exactly).
After the fix: harsh-loss `lost` 87→75, bursty-reorder 10→0, jitter-spike
52→15 (now exactly matching its `late` count). Not a `jitter.go` bug — a
property of driving `Pull` past the data — but it was corrupting what
"Lost" meant in these tests. Fixed as part of Task A above.

No cross-workstream bugs — Tech and QA's file sets didn't overlap
(confirmed via `git status`/`git diff` before integrating), and both
agents independently verified the other's in-flight build breakage during
the parallel run resolved once each finished (noted in their own reports).

### Verified
- Full repo: `go build ./...`, `go vet ./...` clean
- `go test ./... -race -count=3` — all 10 packages pass, no flakes
- `gofmt -l .` — clean
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces — needs
  live/pilot call traffic, which doesn't exist until Week 4. Unchanged.
- `runServe`'s pre-existing shutdown path shares the same ctx-sharing
  pattern the duplex path had before today's fix, but has never been
  exercised against a real close-during-shutdown flush in its own tests —
  untested exposure, not a proven bug. Worth a look next time `serve` is
  touched.
- TTS-pacing defaults (`DefaultTTSPacingTargetDelay`=40ms,
  `DefaultTTSPacingInterval`=20ms) are reasoned starting points, not
  measured against real vendor TTS latency distributions — same
  "tune later" framing as `jitter.go`'s original defaults.
- Docker-build verification, legal review of `docs/compliance.md` — both
  unchanged, still need a human.

### Tomorrow
1. Week 3 is now 5 of 6 done — the only remaining item (real-PSTN jitter
   tuning) is blocked on live traffic, not on more agent work. Week 4
   (pilot launch) can't meaningfully start until Saurabh decides on anchor
   customers / live traffic, so absent that decision, focus on hardening:
   look at `runServe`'s shutdown-ordering exposure flagged above, and/or
   tune TTS-pacing defaults if any real vendor latency data exists yet.
2. Continue strengthening the WER corpus and jitter stress tests
   opportunistically — both are cheap, high-value, and don't block on
   anything.
3. If Saurabh has a go/no-go or anchor-customer decision for Week 4,
   that supersedes both of the above.

## 2026-07-14 (Sprint 8: runServe shutdown fix, WER/jitter hardening, two flaky-test races found+fixed) — scheduled run

### Agents run
Tech, QA (in parallel). PE/SRE not needed — Week 3 is 5 of 6 done (the
only remaining item, real-PSTN jitter tuning, needs live traffic that
doesn't exist yet) and Week 4 (live pilot, real WER/CSAT, go/no-go) is
entirely gated on a live-traffic/anchor-customer decision that hasn't been
made — neither is buildable by agent automation today. Per DEVLOG's own
2026-07-13 "Tomorrow" list, today's scope was hardening/follow-up work
instead of new roadmap checkboxes: (1) the untested `runServe` shutdown-
ordering exposure flagged that day, (2) continued WER corpus / jitter
stress-test strengthening.

### Repo health at start
Clean: `go build ./...`, `go vet ./...`, `go test ./... -race` (all 10
packages), `gofmt -l .` clean before any changes (after working around a
transient sandbox-disk-full condition with `go clean -cache` — see
"Sandbox note" below). ClearStream checked (`git ls-remote --tags` + GitHub
API): still tagged only `v0.1.0`, latest commit (`b76bfa9`, 2026-07-13) is
past the pinned commit but no new tag exists, so no `VERSIONING.md` pin
change needed or made.

### Changes

**Tech — `runServe` shutdown-ordering fix (`cmd/langstream/main.go`)**
- Same bug class as `pkg/rtp/duplex.go`'s 2026-07-13 fix: `runServe`
  constructed its `langstream.Session` against the SIGINT/SIGTERM-
  cancelling `ctx`, so the instant a signal arrived, the Session's internal
  translate/synthesize goroutines abandoned any in-flight final-utterance
  flush before the deferred `sess.Close()` ever ran.
- Split `runServe` into `buildServeSession` (constructs the Session against
  `context.Background()` instead) + `runServeWithContext` (waits for
  `ctx.Done()`, then explicitly calls `sess.Close()` — which synchronously
  drains the final flush, bounded by a 3s `finalFlushTimeout` — concurrently
  with the dashboard's own pre-existing 5s-bounded shutdown). Deliberately
  did not copy `duplex.go`'s fixed `duplexFinalDrainGrace` sleep: `serve`
  has no RTP legs draining audio after `Close()` returns, and `Close()`
  already blocks synchronously on the flush itself, so a fixed sleep would
  only add dead time.
- `cmd/langstream/serve_shutdown_test.go` (new): drives a real Session via
  `buildServeSession`, pushes one buffered caller-audio frame, cancels
  context mid-utterance, and asserts the flush actually reaches
  `AgentHearsAudio()` instead of being dropped (the test that would have
  caught the original bug) — plus construction-failure and no-activity-
  shutdown coverage.

**QA — WER corpus expanded 15 → 25 entries (`pkg/qa/corpus.go`)**
10 new hand-verified entries covering categories the corpus didn't
previously exercise well: contiguous multi-word deletion, brand-name and
person-name substitution, number-word-vs-digit mismatches (both
substitution and digit-sequence-deletion shapes), two long (18-25 word)
utterances (previous entries topped out ~13 words), a content-word (not
filler-word) deletion, a hallucinated-word insertion, and the first
English-dominant-with-embedded-Hindi-courtesy-phrase entry (every prior
entry was Hindi-dominant with embedded English — this is the reverse
direction). Wired into `TestFixedCorpus_PrecomputedWERMatches` and
`TestWERMeasurement_FixedCorpusAgainstFakeASRBackedPipeline` (15→25).

**QA — jitter stress-test hardening (`pkg/rtp/jitter_test.go`)**
Added `TestJitterBufferSimultaneousHighLossAndSevereReordering`: severe
window-based reordering (up to 9 positions, windowSize=10) combined with
independent 15% loss *simultaneously* — the three existing harsh scenarios
each tested loss, reordering, or jitter-spikes in isolation, never
together. Uses the existing harness/`assertPacketAccounting` unchanged,
same exact `played+lost==n` equality. n=350, lost=58 (~16.6%), within
bounds. No bug found in `pkg/rtp/jitter.go` — passed clean against
unmodified production code.

### Bugs found/fixed

**Two real, pre-existing test races found during EM integration
verification (not introduced by today's Tech/QA changes — both tests
predate today, from the 2026-07-12 latency-instrumentation sprint), fixed
by EM (`pkg/langstream/latency_test.go`):**

`go test ./... -race -count=3` intermittently (roughly 1 run in 3) failed
`TestSessionPassthroughSkipsUnattemptedStagesButRecordsTotal` and (in a
separate run) `TestSessionRecordsRealLatencyMetrics`, both with
`Count("total") = 0, want > 0`. Root cause: `session.go` records the
"total" glass-to-glass latency sample *after* the final audio chunk has
already been forwarded to `AgentHearsAudio()` (correct — you can't measure
total latency until the send actually completes), but both tests read
`sess.Metrics()` immediately upon receiving that final chunk on their own
goroutine, racing the sending goroutine's return-then-record path. Not a
production bug (the recording order is correct for what "total" is
supposed to measure) — a test-synchronization bug: asserting something
inherently near-but-not-strictly-synchronous as if it were instantaneous.
Fixed by polling `m.Count("total")` for up to 1s (2ms interval) instead of
checking once; verified fixed with `-count=20` isolated reruns of each
test (20/20 clean) and `-race -count=3` on the full package and full repo.

**Sandbox note (environment, not a code bug):** the shared sandbox disk
(`/sessions`, ~9.8G) was at 96-99% capacity for most of this run from other
concurrent sessions' usage, occasionally causing `go build`/`go test` to
fail mid-compile with "no space left on device" on essentially random
packages each time (confirmed not a real regression — retried clean each
time). Worked around by `go clean -cache` when it happened, and for the
final full verification pass, by pointing `GOCACHE`/`GOPATH`/`GOTMPDIR` at
`/var/tmp` (the `/` mount, which had 3+ GB free vs. `/sessions`'s <200MB)
instead of the default `$HOME` location on the cramped `/sessions` mount.
Not something to "fix" in the repo — noting here in case a future run hits
the same thing and wants the faster workaround.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` — all 10 packages pass, no flakes (after
  the two latency-test fixes above; confirmed clean on repeat runs)
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces — still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) cannot start
  without Saurabh's decision on anchor customer(s) / live traffic — this
  is a business decision, not an engineering task, and no amount of agent
  automation closes it. Flagging plainly rather than inventing scope.
- Docker-build verification, legal review of `docs/compliance.md` — both
  unchanged, still need a human.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that's the top priority and supersedes everything below.
2. Absent that: `runServe`'s shutdown path is now fixed and tested; next
   hardening candidate is auditing whether any other `*_test.go` in the
   repo has the same "assert immediately after channel receive" race
   pattern found today (only `pkg/langstream/latency_test.go`'s two tests
   were confirmed affected, but the pattern could exist elsewhere and just
   not have been caught yet by `-count=3`).
3. Continue strengthening the WER corpus and jitter stress tests
   opportunistically if no higher-priority item exists — still cheap,
   high-value, and don't block on anything.

## 2026-07-14 (interactive session, Saurabh) — Sarvam wire-format bug: live-verified and fixed

Saurabh asked to test locally with real OpenAI + Sarvam keys, then to fix
whatever the testing found. `api.openai.com` is blocked from this sandbox
at the network level (Cisco Secure Access gateway — confirmed via a direct
`curl`, not something to route around), so GPT-4o/MT stayed untested here.
Sarvam is reachable, and testing it live surfaced a real bug.

**Bug (confirmed live, not simulated):** `pkg/asr/sarvam.go`'s "assumption
(1)" — that the per-message `encoding` field should be `"pcm_s16le"` to
match the connection-level `input_audio_codec` param, with headerless raw
PCM as `data` — is wrong. A raw WebSocket session against the real
`wss://api.sarvam.ai/speech-to-text/ws` endpoint with a real key returned:
`{"type":"error","data":{"message":"...audio.encoding\n  Input should be
'audio/wav' [type=enum, input_value='pcm_s16le', ...]"}}`. The real
contract: `encoding` must always be `"audio/wav"`, and `data` must be a
real, self-contained WAV file (RIFF/WAVE header + PCM), not headerless
PCM. Verified two ways: (1) one message containing a whole ~5.6s Hindi
utterance as a single WAV, and (2) the real streaming shape — many small
(~400ms) WAV-wrapped chunks sent in sequence, matching how `PushAudio` is
actually called in production. Both correctly transcribed real Hindi
speech (synthesized via Google TTS as a stand-in, since OpenAI TTS was
also blocked): `"मुझे कल शाम को अपना ऑर्डर वापस चाहिए, कृपया जल्दी मदद
करें"` → `"मुझे कल शाम को अपना order वापस चाहिए कृपया जल्दी मदद करें"`
(correct, natural code-switch handling on "order").

**Fix:** `pkg/asr/sarvam.go` — new `pcm16MonoToWAV(pcm []byte, sampleRate
int) []byte` helper wraps each frame in a minimal 44-byte WAV header
before base64-encoding; `PushAudio` now sets `Encoding: "audio/wav"` and
sends the wrapped bytes instead of raw PCM. Doc comment's assumption (1)
rewritten to state the verified (not guessed) contract. Re-verified after
the code fix by running the real `SarvamRecognizer` client (not just the
raw protocol probe) against the live endpoint with the same Hindi audio —
transcript matched exactly.

**Tests:** `pkg/asr/sarvam_test.go`'s existing
`TestSarvamRecognizer_SendsAudioAndParsesTranscript` updated to assert the
fake server receives `Encoding == "audio/wav"` and a real WAV-wrapped
payload (decoded and compared against `pcm16MonoToWAV`'s own output, not
just magic bytes). Two new tests added: `TestPCM16MonoToWAV` (header
field-by-field correctness) and `TestPCM16MonoToWAV_EmptyPCM` (zero-length
frame doesn't panic or produce a malformed header).

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l .` clean;
`go test ./pkg/asr/... -race -count=3` clean, all tests pass twice over.

**Not touched:** GPT-4o/OpenAI path — untestable from this sandbox
(network block), no code changes made there. Saurabh is continuing
locally where OpenAI is reachable.

**Next (Saurabh's ask, in progress separately):** a local WebRTC test
harness — browser mic in, live ASR→MT→TTS, browser audio out — for both a
single-user-talks-to-bot mode and a real two-browser two-user duplex
relay. Scoping questions asked before starting (mode, TTS backend since no
Cartesia/ElevenLabs key is wired into LangStream yet, and whether this
becomes a committed repo feature or a local-only script) — see the
conversation, not yet in this DEVLOG since scope wasn't settled as of this
entry.

## 2026-07-14 (interactive session, continued) — real WebRTC live-translation harness + ElevenLabs TTS backend

Continuing from this same day's earlier Sarvam wire-format entry (above):
Saurabh asked to fix the Sarvam bug in the repo (done, see above) and then
to be able to test live, real-time translation over an actual browser
call -- either one person talking to a translating bot, or two real
people each speaking their own language, both hearing the other
translated. After scoping questions (mode: two-user relay; TTS backend:
ElevenLabs, since that's the vendor key available; scope: a real,
committed repo feature, not a throwaway script), built both.

### Shipped

**`pkg/tts/elevenlabs.go` + `elevenlabs_voices.go` (new, PE-owned files)** —
a real ElevenLabs TTS backend, verified live against the real API
(`POST /v1/text-to-speech/{voice_id}/stream?output_format=pcm_8000`,
`xi-api-key` header, raw headerless PCM16@8kHz streamed response -- no
WAV/JSON/base64 framing, unlike Cartesia's WebSocket protocol). Two real,
confirmed voice IDs (via `GET /v1/voices` against the actual account):
George (`JBFqnCBsd6RMkjVDRZzb`) for English, Sarah (`EXAVITQu4vr4xnSDxMaL`)
for Hindi. Registered as `--backend elevenlabs` /
`LANGSTREAM_TTS_BACKEND=elevenlabs` in `cmd/langstream/main.go`. Full test
suite against an `httptest.Server` fake, plus a real live smoke-test
against the actual API (33 chunks, ~3.85s of real synthesized audio,
`IsFinal` correctly set on the last chunk) before trusting it further.

**`pkg/webrtcgw` (new package) + `cmd/langstream/webrtc.go` (new
subcommand)** — a real, two-user, browser-facing WebRTC test harness. Two
people each open a served page (`pkg/webrtcgw/static/index.html`,
embedded via `go:embed`), join the same room with opposite roles
("caller"/"agent") over a WebSocket signaling protocol
(`pkg/webrtcgw/signaling.go`), grant mic access, and talk to each other
live through a real `langstream.Session` (the same duplex orchestrator
`pkg/rtp.DuplexSession` bridges for ClearStream's telephony legs) -- no
telephony/RTP infrastructure needed.

**Design decision: G.711 (PCMA), not Opus.** Browsers' WebRTC audio is
normally Opus, which needs a codec library (cgo/libopus) to decode/encode
in Go -- real added complexity this repo doesn't need. PCMA/PCMU are
*mandatory-to-implement* codecs for every WebRTC-compliant browser (RFC
7874, specifically so browsers can interoperate with legacy telephony
gateways) -- confirmed via research, not assumed. `pkg/webrtcgw/peer.go`'s
`newMediaEngine` registers *only* PCMA for audio; since this gateway
always answers (never offers), restricting our side to PCMA forces
negotiation onto it with zero special handling needed on the browser
side (no `setCodecPreferences`, no SDP munging). G.711 companding is
simple 8-bit math (`pkg/webrtcgw/alaw.go`), not a real codec library --
this is what keeps the whole gateway cgo-free. Verified live: a raw
`pion/webrtc` offer with only PCMA/PCMU registered produces a clean
`m=audio ... 8 0` SDP line with no Opus at all.

**Real bug found and fixed live, via full end-to-end testing with real
Sarvam ASR + real ElevenLabs TTS through the actual gateway (not just
mocks):** the first working version pushed every individual 20ms
RTP-derived audio frame straight into `Session.Push{Caller,Agent}Audio`.
This worked for `langstream demo` (which explicitly `Close()`s the ASR
session at the end, and Sarvam responds to the resulting best-effort
flush signal -- see this same day's earlier Sarvam entry) but *silently
never finalized a single utterance* in a real, ongoing, never-closed
room: real Hindi speech went in, real RTP packets were confirmed arriving
server-side (283 packets, ~90KB, matching the source audio exactly), but
zero transcripts ever came back, and the test just hung waiting.

Root-caused methodically, not guessed: isolated chunk size as the only
variable between a working and a silently-broken run against the *live*
Sarvam endpoint (bypassing the whole gateway, driving `SarvamRecognizer`
directly with the identical audio content): 400ms chunks with no explicit
close/flush **did** autonomously finalize via Sarvam's own server-side
VAD; the same content in 20ms chunks with no close/flush **never** did,
even waiting 20+ seconds. Conclusion: Sarvam's VAD needs each individual
message's audio to span a large-enough window to detect a speech/silence
transition within -- 20ms (one RTP packet) is too short a window for that
detection to ever trigger, so a session that's never explicitly closed
(the normal case for a live, ongoing two-user call) just sits there
forever with nothing to signal "utterance over."

**Fix:** `pkg/webrtcgw/inbound_buffer.go`'s new `inboundBuffer` type:
accumulates ~400ms of decoded PCM across many small RTP packets before
calling `Session.Push{Caller,Agent}Audio`, with an explicit `flush()` that
still delivers whatever's buffered (even if under 400ms) when a track
ends -- so a real hangup mid-utterance doesn't silently drop the last
words either. Directly unit-tested (`inbound_buffer_test.go`, no live
pion/WebRTC transport needed) for: accumulation-not-immediate-forwarding,
correct reset after a flush, forced partial delivery on `flush()`, and
flush-on-empty being a safe no-op. Re-verified against the live Sarvam +
ElevenLabs stack after the fix: real transcript arrived
(`"मुझे कल शाम को अपना order वापस चाहिए कृपया जल्दी मदद करें"`), real
mock-translated text, real ElevenLabs audio (79.5KB, ~4.97s) delivered to
the other peer's WebRTC track.

**A second, smaller bug found via `go test -race`:** the *test harness
itself* (not gateway code) wrote to its WebSocket connection from two
goroutines without a mutex (the `OnICECandidate` callback racing the main
goroutine's join/offer sends) -- caught immediately by `-race` on the
very first real run, fixed by mirroring the same `writeMu`-guarded
`writeJSON` pattern the real `SignalingHandler` already used correctly.

**A third bug, flakiness under `-count=N`:** the end-to-end test reused a
literal room-ID string (`"room-1"`) across repeated runs within the same
test binary invocation; a room's cleanup (`Manager.leave`, triggered
asynchronously off `OnConnectionStateChange` once both peers disconnect)
isn't guaranteed to finish before a later iteration reuses the same ID,
so a repeated run could race stale room state and silently never connect
a fresh session. Fixed with a package-level atomic counter minting a
unique room ID per test call; verified stable across repeated
`-count=3`/`-count=4` runs after the fix (was reproducibly flaky before).

### Verified
- `pkg/asr`, `pkg/tts`, `pkg/webrtcgw`: `go build ./...`, `go vet ./...`,
  `gofmt -l .` clean; `go test ./... -race -count=2` clean across all 11
  packages (10 existing + the new `pkg/webrtcgw`), no flakes across
  multiple repeated runs after the room-ID fix above.
- Full live stack (real Sarvam ASR + real ElevenLabs TTS, mock
  translation) driven through the actual gateway via two independent,
  real `pion/webrtc` clients (headless stand-ins for real browsers --
  everything about the protocol/media path is real; only actual browser
  JS engines and microphone hardware are out of scope for this sandbox):
  real ICE/DTLS/SRTP negotiation, real G.711 RTP both directions, real
  vendor API calls, real translated audio delivered end to end.

### Blocked / not done here
- GPT-4o/OpenAI-backed translation untested through the WebRTC gateway
  from this sandbox specifically -- `api.openai.com` is blocked at this
  sandbox's network egress level (confirmed via direct HTTP request
  redirecting to a Cisco Secure Access block page), unrelated to any code
  in this repo. The GPT-4o client itself is unchanged from Week 2 and
  already tested then; Saurabh is continuing this specific test on his
  own machine where that domain is reachable.
- No TURN server configured (only a public STUN default) -- fine for
  same-host/same-LAN testing (the intended use case here), would need a
  TURN server added via `--stun` for participants behind restrictive/
  symmetric NATs.
- This feature is explicitly out-of-band from the daily six-agent
  automation's normal roadmap execution (see ROADMAP.md's new section) --
  not tied to a Week 3/4 checklist item, requested directly in this
  interactive session.

### Tomorrow (for the next scheduled daily run)
This work happened in an interactive session, not the scheduled
automation -- the next scheduled run should read this entry, note the new
`pkg/webrtcgw`/`pkg/tts/elevenlabs.go` files now exist (Tech and PE's
file-ownership map in `references/workstreams.md` naturally covers them:
`pkg/webrtcgw` falls under Tech's `pkg/langstream/*.go, pkg/rtp/*.go,
cmd/langstream/*.go, examples/` charter in spirit even though the literal
glob doesn't list it yet -- worth a small workstreams.md update next
scheduled run to add `pkg/webrtcgw/*.go` explicitly), and continue normal
Week 3/4 assessment unaffected by this addition.
