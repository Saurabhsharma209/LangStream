# LangStream Dev Log

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
