# Combined Roadmap — ClearStream + LangStream

Two independent repos, two independent daily agent teams, one call
pipeline. This document is the single place that shows both timelines
side by side and calls out the points where they actually touch, so
"hand in hand" means something concrete instead of just two projects
existing near each other. See `VERSIONING.md` for how version
compatibility between them is tracked.

Each repo keeps its own detailed roadmap/log as the source of truth for
its own work — [`ClearStream/DEVLOG.md`](https://github.com/Saurabhsharma209/ClearStream/blob/main/DEVLOG.md)
and this repo's [`ROADMAP.md`](ROADMAP.md) / [`DEVLOG.md`](DEVLOG.md). This
file doesn't duplicate that detail; it's the map of how they relate.

## Where they stand today (2026-07-07)

| | ClearStream | LangStream |
|---|---|---|
| Maturity | Mature, actively hardened (188+ commits, six-agent daily QA/coverage cycle) | Week 1 of a 1-month pilot |
| Version | `v0.1.0` (tagged) | `v0.1.0-alpha` (tagged) |
| Cadence | Continuous hardening — no fixed "roadmap end," runs indefinitely | Fixed 4-week pilot scope (`ROADMAP.md`), explicitly not GA |
| Automation | 6 workstream agents (Audio Pipeline, AI Model, RTP/SIP, Post-processing, API Layer, QA/Testing) | 6 role agents (PM, EM, PE, Tech, SRE, QA) |
| Owns | Noise suppression, AGC, RTP/SIP media handling, codec support | ASR/MT/TTS orchestration, duplex session logic, voice personas |

## How they fit together in production

```
Caller/Agent RTP ──► ClearStream (denoise, AGC) ──► LangStream (ASR→MT→TTS) ──► other party
```

ClearStream's job ends at "clean audio out." LangStream's job starts at
"clean audio in" and has no noise-suppression logic of its own. Running
ClearStream first measurably improves LangStream's ASR accuracy — this is
why they're designed to run together even though neither imports the
other's product surface today.

## The one real coupling point: duplex RTP (LangStream Week 2)

LangStream's `pkg/rtp` is currently a doc-only skeleton (see `pkg/rtp/doc.go`).
Week 2 of LangStream's roadmap extends ClearStream's `pkg/rtp.Session`
model for two-leg (duplex) media instead of reimplementing RTP handling
from scratch. This is the one place the two projects actually need to
coordinate, not just coexist:

1. **Before starting:** LangStream's daily automation checks ClearStream's
   latest tagged release (currently `v0.1.0`) and reads `pkg/rtp` there to
   confirm what's reusable as-is via a normal Go module import
   (`go.mod require github.com/exotel/clearstream`).
2. **If ClearStream's `pkg/rtp` already exposes what's needed:** LangStream
   imports it, pins the version, and updates `VERSIONING.md`'s
   compatibility table in the same commit. No ClearStream-side change.
3. **If it doesn't** (e.g. something needed for duplex use is currently
   unexported or structured single-leg-only): this is a coordination
   checkpoint, not something LangStream's automation resolves unilaterally.
   Standing agreement (see both repos' README "Related Projects" /
   DEVLOG cross-link entries from 2026-07-07): any actual ClearStream code
   change gets proposed as its own separately-reviewed PR against the
   ClearStream repo, with its own description of what changed and why —
   never a silent side effect of a LangStream commit. Saurabh gets a
   flagged report instead of an autonomous cross-repo edit.

## Timeline view

```
ClearStream  ──────────────────────────────────────────────────────►  (continuous hardening, no end date)
                         │
                         │ v0.1.0 tagged 2026-07-07 — pinning point established
                         ▼
LangStream   Week 1 ──► Week 2 ──────► Week 3 ──────────► Week 4
             (done)     (real ASR/MT/TTS +     (hardening,     (pilot launch,
                          ⚠ duplex RTP           compliance,     go/no-go)
                          coordination            observability)
                          checkpoint here)
```

## What "hand in hand" means in practice, concretely

- Both repos are tagged and versioned (SemVer, see `VERSIONING.md`) instead
  of floating on `main` with no pinning point.
- Both READMEs and DEVLOGs cross-reference each other so anyone landing in
  either repo finds the other and understands the relationship.
- The one real dependency (duplex RTP) is called out explicitly with a
  process for handling it, instead of assuming either automation will
  "figure it out" by quietly editing the other repo.
- Each project keeps its own independent roadmap, agents, and pace — this
  document coordinates them, it doesn't merge them.
