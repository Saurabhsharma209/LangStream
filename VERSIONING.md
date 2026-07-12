# Versioning

LangStream and [ClearStream](https://github.com/Saurabhsharma209/ClearStream)
are independent repos with independent release cadences — see the "Related
Projects" discussion in each README for why they aren't merged into one repo
or one set of branches. This document is what makes them work *together*
despite that: a shared versioning convention and an explicit compatibility
matrix, so a change in one never silently breaks the other.

## Scheme

Both repos follow [SemVer](https://semver.org) (`MAJOR.MINOR.PATCH`), with
the pre-1.0 convention applied honestly:

- **0.x.y** — anything may change between minor versions without notice.
  Both repos are at 0.x today. ClearStream's `0.1.0` reflects real
  production-hardening work (188+ commits, 94%+ test coverage on core
  packages) that simply was never git-tagged before; LangStream's
  `0.1.0-alpha` reflects Week 1 of a 1-month pilot — the `-alpha` suffix is
  there specifically so nobody mistakes it for the same maturity level as
  ClearStream's `0.1.0`.
- **1.0.0** — first version either repo commits to a stable public API.
  For LangStream that's not before the pilot (ROADMAP.md) proves out and a
  real GA scope is scoped and staffed.
- Tags are cut manually at meaningful milestones (end of a roadmap week,
  a significant fix), not automatically per commit — both repos run daily
  agent automation that commits frequently, and tagging every commit would
  make tags meaningless.

## Compatibility matrix

| LangStream version | Requires ClearStream | Why |
|---|---|---|
| `v0.1.0-alpha` (2026-07-07 to 2026-07-12) | `>= v0.1.0` (informational only — not yet an actual Go module dependency) | LangStream's `pkg/asr` interfaces assume reasonably clean input audio; no code-level dependency exists yet because LangStream doesn't import ClearStream's Go package today. |
| `v0.1.0-alpha` (current, 2026-07-12+) | ClearStream commit `4d5ea467888c97a61d501efe33ba271b039f3348` (pinned as pseudo-version `v0.0.0-20260712052406-4d5ea467888c`, no ClearStream semver tag exists past that commit yet) | **Real Go module dependency as of 2026-07-12.** ClearStream's own daily automation resolved the standing "OnCleanAudio" decision by adding `rtp.Session.CleanAudio() <-chan rtp.CleanAudioFrame` (opt-in via `Config.CleanAudioBufferSize`, see ClearStream's `ROADMAP.md` "Resolved Decisions" 2026-07-12 entry) — LangStream's duplex RTP session (`pkg/rtp/duplex.go`) now imports `github.com/exotel/clearstream/pkg/rtp` directly: `CleanAudio()` feeds the caller→ASR direction, the pre-existing `InjectBotAudio([]byte) bool` feeds the TTS→caller-audio-out direction. Because ClearStream hasn't cut a new semver tag since `v0.1.0`, `go.mod` pins the exact commit via a pseudo-version plus a `replace github.com/exotel/clearstream => github.com/Saurabhsharma209/ClearStream ...` directive (needed because ClearStream's own `go.mod` declares module path `github.com/exotel/clearstream`, which isn't the repo's actual GitHub location). **Action item:** once ClearStream cuts a real tag containing this commit, drop the pseudo-version pin (and ideally the `replace`, if ClearStream's module path is ever corrected to match its real import path) in favor of that tag. |

**Policy:** whenever LangStream's `go.mod` changes which ClearStream
version it requires, that same commit must update this table. If a
ClearStream release contains a breaking change relevant to `pkg/rtp`,
LangStream should stay pinned to the last compatible version until the
break is deliberately absorbed and tested — never float on ClearStream's
`main` branch in production code.

## Cross-repo coordination

Both projects run their own independent daily agent automation (ClearStream:
six workstream agents; LangStream: PM/EM/PE/Tech/SRE/QA — see
`references/workstreams.md`). Neither automation modifies the other repo's
*code* without it being called out explicitly and treated as its own
reviewed change — see `COMBINED_ROADMAP.md` for where their timelines
intersect, and the "Related Projects" note in each repo's README/DEVLOG for
the standing agreement on how that coordination happens.
