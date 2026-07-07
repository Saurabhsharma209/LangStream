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
