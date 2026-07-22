# LangStream Dev Log

## 2026-07-22 (interactive session, Saurabh) — voice-distortion root cause, MT/TTS timeout bug, Gemini backend, Deepgram Hindi

Saurabh reported live-pilot issues with translation quality, voice
distortion, and clarity, and pushed a rough, untested set of ideas to a
`suggestions` branch (commit `e8b83ac`: a Gemini translator draft,
Deepgram Hindi support via plain `nova-2`, a blind `TranslateTimeout`/
`SynthesizeTimeout` bump from 2s/3s to a flat 10s, and inline stderr MT
debug logging) as a signal, not a finished fix. Asked to investigate and
fix properly rather than merge that branch as-is. Tech and PE agents ran
in parallel on `main`, followed by QA's independent verification pass.

### Investigation findings (before any code was written)

- **Ruled out one plausible theory first:** checked whether ClearStream's
  outbound G.711 codec (A-law vs µ-law) could be silently mismatched from
  the RTP leg's actual negotiated `PayloadType` (a classic cause of
  garbled telephony audio). Confirmed via ClearStream's own
  `resolvePayloadType()` (`pkg/rtp/session.go`) that `Codec` is correctly
  auto-derived from `PayloadType` when left unset -- this was already
  correct, not a bug, so no change was made here.
- **Real root cause #1 (voice distortion): a chunk-boundary bug in
  `pkg/rtp/duplex.go`'s TTS pacer.** ClearStream's `InjectBotAudio` accepts
  raw PCM16 but silence-pads any trailing partial 160-sample (20ms@8kHz)
  frame on *every single call*. `feedTTSPacer` was handing each
  arbitrarily-sized real TTS chunk straight through -- since Cartesia/
  ElevenLabs streaming chunk sizes don't naturally land on clean 320-byte
  boundaries, nearly every chunk boundary was getting silently
  padded/truncated at a non-sample-aligned point, which is exactly the
  kind of thing that sounds like clicking/choppy distortion on a real call
  but wouldn't show up against this repo's round-number fake-server test
  fixtures.
- **Real root cause #2 (translation being skipped, not mistranslated):**
  `TranslateTimeout`/`SynthesizeTimeout` (2s/3s) wrap each vendor call's
  *entire* retry sequence (3 attempts, up to ~1.8s of cumulative backoff
  alone, on top of real request latency) -- but this budget was only ever
  validated against fast local fakes per ROADMAP.md's Week 2 decision;
  real vendor latency was untested until Saurabh's own live run. A tight
  2s budget could plausibly expire on one real GPT-4o/Cartesia attempt,
  triggering fallback-to-original-audio (with a warning tone) far more
  often than intended -- which would read as "translation/clarity" issues
  to a caller even though the translation itself was never wrong, just
  frequently skipped.
- **Real root cause #3, found only while fixing #2, not hypothesized in
  advance:** `SynthesizeTimeout`'s `ttsCtx` was built with
  `context.WithCancel` (no deadline at all), not `context.WithTimeout` --
  so TTS's own internal connect-retry logic (up to 3 attempts x up to 10s
  dial timeout = up to 30s) ran completely unbounded by
  `SynthesizeTimeout`; only the post-connect "wait for first chunk" phase
  was ever actually timed.
- **Deepgram Hindi:** confirmed via Deepgram's own docs
  (`developers.deepgram.com/docs/multilingual-code-switching`,
  `.../docs/models-languages-overview`) that Nova-2's real-time
  code-switching is English-Spanish only -- it does not support
  Hindi-English code-switching. Naively adding `language=hi` on `nova-2`
  (Saurabh's draft) would transcribe pure Hindi reasonably but likely
  produce noticeably worse results than Sarvam on exactly this product's
  primary real-world pattern: Hinglish, mid-sentence code-switched
  speech.

### Changes

**Tech -- chunk-boundary fix (`pkg/rtp/duplex.go`, `pkg/rtp/tts_pacing_test.go`)**
`feedTTSPacer` now accumulates PCM into clean 320-byte (160-sample)
frames, carrying any remainder forward to the next chunk, and only
flushing a genuine partial tail at `IsFinal` or stream close. New
`TestTTSPacer_AccumulatesPartialFramesAcrossChunkBoundaries`; existing
tests updated to use frame-aligned fixtures where the old ones
accidentally masked this.

**Tech -- timeout recalibration + fix (`pkg/langstream/fallback.go`,
`pkg/langstream/session.go`, `pkg/langstream/fallback_test.go`)**
`TranslateTimeout` 2s -> **6s** (covers ~2 real attempts at a realistic
p95 latency + backoff), `SynthesizeTimeout` 3s -> **4s** (same reasoning,
TTS's per-attempt cost is lower) -- both derived from the retry math
above and documented in `fallback.go`'s doc comments, not the draft's
blind flat 10s (which would mean up to 10s of dead air on a genuinely-down
vendor -- a worse real-time-call experience than a well-reasoned tighter
number). `ttsCtx` now built with `context.WithTimeout(s.ctx,
s.fallback.SynthesizeTimeout)` so the timeout actually bounds the whole
`SynthesizeStream` call including connect-retries, fixing the
previously-unbounded-connect-phase bug found above. Added a documented,
non-enforced `maxSaneFallbackTimeout` (8s) reference ceiling.

**Tech -- structured MT/TTS failure logging (`pkg/langstream/session.go`,
`cmd/langstream/main.go`)**
New optional `SessionConfig.Logger *zap.Logger` (defaults to
`zap.NewNop()`), replacing the draft's raw `fmt.Fprintf(os.Stderr, ...)`
idea with real structured logging consistent with `pkg/rtp`'s existing
logger pattern -- covers both MT and TTS failure/stall sites (the draft
only logged MT), wired into the CLI's real session construction in
`cmd/langstream/main.go`.

**PE -- Gemini 2.0 Flash translator backend (`pkg/translate/gemini.go`,
`gemini_test.go`, new)**
Built out from the draft's API shape (which was structurally sound) but
fixed a real gap: the draft never recorded cost at all. Added real
`RecordCost` wiring using Gemini's per-token pricing ($0.10/M input,
$0.40/M output), parsing `usageMetadata` for exact token counts with a
char-count fallback matching `gpt4o.go`'s existing convention. 28 new
tests (success, retry/breaker/cooldown/probe, safety-block handling,
context cancellation, the 4 cost-recording invariants this repo already
established for other vendors). Registered as `--backend gemini` /
`LANGSTREAM_TRANSLATOR_BACKEND=gemini`.

**PE -- Deepgram Hindi via Nova-3 code-switching, not Nova-2
(`pkg/asr/deepgram.go`, `deepgram_test.go`)**
Hindi now routes to `model=nova-3` + `language=multi` (Deepgram's
real-time multilingual code-switching mode, confirmed to include Hindi),
not plain `nova-2`/`language=hi` -- English unchanged on `nova-2`/
`language=en`. Added `endpointing=100` for code-switching sessions per
Deepgram's own recommendation, a Hindi-specific cost rate, and a
`WithDeepgramHindiModel` override option. 6 new tests including
cross-language isolation on a shared recognizer instance.

**QA -- independent verification (`chunk_boundary_integration_test.go`
new; `pkg/langstream/session_test.go`, `pkg/asr/deepgram_test.go`
appended)**
Verified the chunk-boundary fix by driving real audio through a real
`*langstream.Session` + `*rtp.DuplexSession` with a fake TTS producing
deliberately non-aligned chunk sizes (137/501/322 bytes), decoding the
actual G.711 RTP output sample-for-sample against an independently
reimplemented reference mu-law codec -- and confirmed the test actually
catches the bug by temporarily reverting the fix and watching it fail
with exactly the expected corruption pattern before restoring it.
Verified the `ttsCtx` timeout fix the same way (a fake TTS backend that
blocks past `SynthesizeTimeout` before ever connecting). Filled two
coverage gaps (Deepgram English/Hindi cross-language isolation on a
shared recognizer; `SessionConfig.Logger`'s nil-defaults-to-`zap.NewNop()`
path). Gemini's 28 tests audited for the repo's standard
assert-after-async race pattern -- clean, no fix needed.

### Bugs found/fixed
Three, all described above: the TTS chunk-boundary silence-padding bug
(the most likely actual cause of "voice distortion"), the unbounded
TTS-connect-retry timeout bug (found while fixing the timeout budget, not
hypothesized going in), and Deepgram Hindi's code-switching gap on
Nova-2 (caught before it shipped, via research, not via a live failure).

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` -- all 12 buildable packages pass, run in
  chunks (`.`/`pkg/langstream`/`pkg/rtp`; `pkg/asr`/`pkg/translate`;
  `pkg/tts`/`pkg/qa`/`pkg/observability`; `pkg/webrtcgw`/`cmd/langstream`/
  `examples`)
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Not done / needs a real key or live call to confirm further
- Gemini and Deepgram-Nova-3-Hindi are both implemented and unit/
  integration tested against fake servers, per this repo's established
  Week 2 pattern -- neither has been live-tested against the real vendor
  API from this session (no keys available here). Recommend a live smoke
  test (same shape as the 2026-07-14 Sarvam/ElevenLabs live verification)
  before relying on either in a real pilot call.
- The chunk-boundary and timeout fixes are structural/logic fixes verified
  by decoding real RTP output in-test, but a live PSTN call is still the
  real confirmation that the audible "distortion" complaint is resolved --
  recommend Saurabh re-test the same scenario that surfaced the original
  complaint.

### Tomorrow
1. If Saurabh can live-test with real Gemini/Deepgram keys, that's the
   highest-value next step to confirm today's fixes actually resolve what
   he heard.
2. Otherwise, continue the normal scheduled-automation cadence -- Week 4
   still blocked on the anchor-customer/live-traffic decision, unchanged.


## 2026-07-21 (Sprint 14: dead-leg audio drain, vendor cost-recording audit, WER corpus growth) — scheduled run

### Agents run
Tech, PE, QA — three agents in parallel (Tech/PE), then QA sequenced after
so it could test against Tech's and PE's real, finished code rather than
guessing at their shape in advance. SRE not needed (no scoped SRE-owned
gap found during planning; CI/Docker/observability infra unchanged from
prior audits). Week 3's one open item (real-PSTN jitter tuning) and all of
Week 4 remain gated on Saurabh's anchor-customer/live-traffic decision,
unchanged since Sprint 8 — today again did opportunistic hardening rather
than inventing roadmap scope.

### Repo health at start
Clean, on the very first try: `go build ./...`, `go vet ./...`,
`go test ./... -race` all passed clean for the entire repo (all 12
buildable packages; `examples/backend_selection` and
`tools/latency_benchmark` have no test files). `gofmt -l .` clean.
ClearStream checked (`git ls-remote --tags`): still only `v0.1.0`, no
`VERSIONING.md` action needed — today's scope never touched `pkg/rtp`'s
ClearStream import either way.

### Sandbox note
`$HOME` (`/sessions/...`) was again at 100% full, 0 bytes free, for the
entire run — same pattern as every sprint since Sprint 9, Sprint 12's
disk-exhaustion incident being the one exception that briefly looked
clean. Worked around exactly as Sprint 13 did: cloned directly into a
fresh, this-run-owned directory (`/tmp/lswork/LangStream`), reused the
pre-existing extracted go1.22.5 toolchain at `/tmp/gotools` (read+execute
accessible even though owned by a stale UID from a past run), and used a
fresh `TMPDIR` (`/tmp/lswork/gotmp`) rather than the sandbox's default
`$TMPDIR`, which points at the full `/sessions` filesystem and fails Go's
own scratch-dir creation immediately even when `/tmp` itself has room.
Root filesystem held 2.6→1.1GB free across the run (three
parallel/sequenced agents each compiling and testing) — tighter than
Sprint 13's 2.7-3.4GB but never hit the wall. Confirms Sprint 13's
tentative read: the disk situation is sandbox-instance-specific, not a
monotonically-worsening shared-state problem — this is now two clean (or
workable) runs out of the last three, with only Sprint 12 hitting a hard
wall. Still worth the next run checking early and reporting the delta
either way, per Sprint 13's own note.

### Changes

**Tech — dead-leg audio no longer silently dropped forever
(`pkg/langstream/session.go`, `pkg/langstream/fallback.go`)**
Closed the gap Sprint 13 documented as intentional and left open: after
an ASR stream permanently dies mid-call, `runLeg` did one last
drain-and-forward of buffered audio and returned — nothing was left to
drain any audio pushed afterward until `Session.Close()` finally ran, so
a caller who kept talking to an already-dead leg had that audio
silently disappear. New `drainDeadLeg` goroutine (spawned via the same
`s.wg` the leg goroutines already use, so `Close()`'s existing
`wg.Wait()` covers it with no new synchronization primitive) polls
`leg.audio` every `FallbackConfig.DeadLegDrainInterval` (new field,
300ms default) and forwards whatever's buffered as passthrough, tagged
with a new, distinct reason `reasonASRStreamClosedPassthrough` (separate
from the original one-time `reasonASRStreamClosed`) so the dashboard can
tell "the leg just died" from "the leg's been dead and audio kept
arriving."

**Real bug found and fixed during this same task:** the first cut of
`drainDeadLeg` only exited on `s.ctx.Done()`. Since `Close()` doesn't
call `s.cancel()` until after its own `wg.Wait()` returns (or a 3s
`finalFlushTimeout` backstop fires), and the drainer is tracked by that
same `wg`, every `Close()` on a session with a dead leg would have
deadlock-waited for the full 3-second backstop before `s.cancel()` could
even run — caught because
`TestASRPermanentFailure_RealSessionDegradesCallerLegAndForwardsBufferedAudio`
suddenly took exactly 3.00s instead of near-instant. Fixed by having
`drainDeadLeg` also check the existing `s.closing` atomic flag each tick
(reusing the existing primitive, not adding one) so it exits within one
poll interval of `Close()` starting. This is the third instance of this
exact shutdown-ordering bug class in this repo (see DEVLOG.md's
2026-07-12 and 2026-07-14 entries) — worth remembering as a standing
review point any time a new goroutine is added to `Session`.

**PE — vendor cost-recording audit across all 5 vendor clients
(`pkg/asr/deepgram_test.go`, `pkg/asr/sarvam_test.go`,
`pkg/translate/gpt4o_test.go`, `pkg/tts/cartesia_test.go`,
`pkg/tts/elevenlabs_test.go`)**
Audited Deepgram, Sarvam, GPT-4o, Cartesia, and ElevenLabs against four
specific cost-correctness invariants: no double-counting `RecordCost` on
retry-then-succeed; no cost recorded for undelivered work on full-retry
exhaustion; ASR mid-stream reconnects don't double-bill; circuit-open
rejections never record cost. **Clean audit — no bugs found.** All four
invariants already held in every vendor client (each success path has
exactly one cost call site, reached only after the retry loop resolves;
Cartesia/ElevenLabs bill per-character at request-acceptance time by
design, matching real vendor invoicing, so a later mid-stream drop
correctly doesn't erase or double that cost; Deepgram/Sarvam compute
cost fresh from the current `AudioFrame` per `PushAudio` call, never from
reconnect-resettable cumulative state; all five breakers reject before
any billing logic runs). Per this repo's established pattern for a real
negative result (see Sprint 11's race-pattern audit), added 12 new
pinning regression tests across the five files above rather than
manufacturing a fix for a non-bug, so a future regression in any of these
four invariants is caught loudly instead of silently.

**QA — integration coverage for Tech's fix, WER corpus growth (47 → 52),
race-pattern audit continuation**
New root-level `dead_leg_drain_integration_test.go`: drives a real
`*langstream.Session` (not `runLeg` in isolation) through a permanent ASR
failure followed by *three* separate subsequent audio pushes over time
(Tech's own unit tests covered one follow-up push; this closes the gap
to "keeps forwarding indefinitely, not just once more"), for both caller
and agent legs, plus a 15-iteration goroutine-leak check pushing 3
frames per cycle. WER corpus grew 47 → 52 with 5 new, hand-verified,
non-overlapping error shapes: total-deletion-to-empty-hypothesis
(silence timeout, WER 1.0 via genuine D=N, distinct from the existing
S=N total-substitution case), deletion+insertion with zero substitutions,
a contiguous 3-word phrase-repeat insertion (previous insertions were all
single-word), the same word mis-heard identically at two occurrences
("hai"/"hain" verb agreement, vs. existing multi-substitution entries
using unrelated word pairs), and a trailing 3-word contiguous deletion
block (call-cutoff/truncation shape, distinct from the existing 2-word
mid-sentence deletion and the 3-non-adjacent-deletions entry).
`corpus_test.go` and `wer_measurement_test.go` updated in sync (one
entry — the empty-hypothesis case — deliberately excluded from the
Sarvam-backed pipeline test, since `sarvam.go`'s `handleMessage` silently
drops empty-transcript messages; documented, not a bug). Race-pattern
audit found one structurally-relevant-but-safe case (PE's two
mid-stream-reconnect cost tests use a fixed 150ms sleep to let a
goroutine observe a dropped connection, matching a pattern already
shipped in Sprint 13 and run clean 25x under `-race` — flagged for
visibility, not fixed, per Sprint 11's precedent for this exact kind of
negative-but-notable finding) and nothing else.

### Bugs found/fixed
One, described above under Tech: the dead-leg drainer's original
`s.ctx.Done()`-only exit condition would have forced every `Close()` on a
session with a dead leg to eat the 3s `finalFlushTimeout` backstop. Fixed
same-day, before landing. No bugs found in PE's or QA's own work, and QA
found none in Tech's or PE's finished code either.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` — all 12 buildable packages pass, no
  flakes, run in chunks to stay under this sandbox's per-command time
  limit (`.`/`pkg/langstream`/`pkg/asr`; `pkg/translate`/`pkg/tts`/
  `pkg/qa`; `pkg/rtp`/`pkg/webrtcgw`/`pkg/observability`/`cmd/langstream`;
  `examples/...`)
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces — still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) still cannot start
  without Saurabh's decision on anchor customer(s) / live traffic —
  unchanged.
- Docker-build verification, legal review of `docs/compliance.md` — both
  unchanged, still need a human.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that's the top priority and supersedes everything below.
2. Absent that: continue opportunistic hardening (WER corpus, race-
   pattern audits) if no higher-priority item exists — still cheap,
   high-value, and don't block on anything.
3. Keep checking whether the sandbox disk situation holds — two workable
   runs out of the last three (Sprint 12 the exception), but that's still
   too small a sample to call the pattern resolved.


## 2026-07-20 (Sprint 13: ASR circuit breakers + permanent-ASR-failure leg visibility, WER corpus growth) — scheduled run

### Agents run
PE, Tech, QA -- three agents in parallel. SRE not needed (no scoped
SRE-owned gap found during planning; CI/Docker/observability infra
unchanged from prior audits). Week 3's one open item (real-PSTN jitter
tuning) and all of Week 4 remain gated on Saurabh's anchor-customer/
live-traffic decision, unchanged since Sprint 8 -- today again did
opportunistic hardening rather than inventing roadmap scope.

### Repo health at start
Clean, on the very first try: `go build ./...`, `go vet ./...`,
`go test ./...` (single run and `-race -count=3`), `gofmt -l .` all
passed clean for the *entire* repo including `pkg/webrtcgw`/
`cmd/langstream` (the packages Sprint 12 could not verify at all).
ClearStream not checked this run (today's scope never touched
`pkg/rtp`/duplex-RTP; VERSIONING.md's pin is unchanged from 2026-07-12).

### Sandbox note: Sprint 12's disk-exhaustion blocker is resolved
`$HOME` (`/sessions/...`) was still at 100% full, 0 bytes free, for this
entire run -- same as every prior sprint -- but this time it didn't
matter: all work was done from the start in `/tmp/LangStream` (cloned
directly there, never attempted under `$HOME`) using a freshly
downloaded go1.22.5 toolchain extracted to `/tmp/gotools` (no leftover
cruft found in `/tmp` this run, unlike Sprints 11-12's 60+ stale
directories from past sessions). One real environment gotcha found and
worked around: this sandbox's `$TMPDIR` env var defaults to
`/sessions/.../tmp` (the full filesystem), which made `go build`/`go
test` fail immediately with "no space left on device" (creating its
scratch workdir) even though `/tmp` itself (the actual filesystem `go
build` should use) had 3+GB free the whole run -- fixed by exporting
`TMPDIR=/tmp/mytmp` before every Go command. Root filesystem (`/`) held
steady at 2.7-3.4GB free throughout the entire run, including three
parallel subagents each compiling/testing concurrently -- no repeat of
Sprints 8-12's worsening disk-pressure pattern today. Worth noting for
whoever owns this automation's sandbox environment: today's clean run
suggests the pattern may be sandbox-instance-specific (a fresh instance
each run) rather than a truly persistent, ever-worsening shared-state
problem as Sprints 8-12 assumed -- but that's an inference from one good
day, not a confirmed fix; the next run hitting disk pressure again would
be the real signal either way.

### Changes

**PE -- circuit breakers for `pkg/asr` (Deepgram, Sarvam)
(`pkg/asr/circuitbreaker.go`, new; `pkg/asr/deepgram.go`,
`pkg/asr/sarvam.go`)**
`pkg/translate` (GPT-4o) and `pkg/tts` (Cartesia, ElevenLabs) each got a
circuit breaker in Sprint 10; `pkg/asr` never did, despite having the
same "sustained vendor outage means every new call pays a full
dial-and-backoff cost" problem those breakers solve. Ported the same
design (5 consecutive-failure threshold, 10s cooldown, single-probe
recovery) to Deepgram/Sarvam, scoped correctly for ASR's very different,
long-lived-streaming-connection shape: the breaker gates `StartStream`
(fails fast with a wrapped `ErrCircuitOpen`, zero dial attempts, when
open) and is settled -- success, failure, or abort -- only by a
session's very *first* connect attempt; `pkg/asr/backoff.go`'s existing
mid-stream `reconnectBackoff` reconnects (a session that connected fine
once, then drops and reconnects later) are completely untouched and
never interact with the breaker, by design (verified by a dedicated
test: a mid-stream reconnect failure does not move `breaker.open`/
`consecutiveFails` at all). `WithCircuitBreaker`/`WithSarvamCircuitBreaker`
functional options added, matching each package's existing
naming convention; circuit-open rejections tagged via the existing
`RecordErrorReason("asr_connect", vendor, "circuit_open")` API, reusing
each recognizer's existing `WithMetrics` recorder (no second metrics
field added). 10 new tests across `pkg/asr/circuitbreaker_test.go`
(breaker unit tests, including a stuck-probe regression mirroring
`pkg/translate`'s) and `deepgram_test.go`/`sarvam_test.go` (5 scenarios
each: trips after N consecutive initial-connect failures and fails fast
with zero new dial attempts; dashboard tagging; probe-after-cooldown
recovery; mid-stream reconnects don't touch the breaker; default breaker
active without the option).

**Tech -- permanent ASR failure no longer silently kills a leg
(`pkg/langstream/session.go`, `pkg/langstream/fallback.go`)**
`fallback.go`'s own doc comment carved out the ASR socket as "not this
file's problem... today's ASR backends already reconnect/retry
internally" -- true for transient blips, but not for the case an ASR
`StreamSession`'s `Transcripts()` channel closes *permanently* (e.g.
Deepgram's `failAndClose` after exhausting `maxReconnectAttempts`).
`runLeg`'s `case tr, ok := <-transcripts: if !ok { return }` branch
handled that identically to a deliberate `Session.Close()` shutdown --
silently returning, with `CallerLegDegraded()`/`AgentLegDegraded()`
never reflecting it and whatever raw audio was mid-utterance in
`leg.audio` dropped, unlike every *other* fallback trigger in this same
function (low confidence, MT error/timeout, TTS error/timeout, repeated
MT/TTS failure). Fixed with a new `Session.closing atomic.Bool`, set as
the first step of `Close()` before either ASR stream is closed, so
`runLeg` can tell "the whole Session is shutting down on purpose"
(`closing == true`: unchanged, just return) apart from "the backend
itself gave up mid-call" (`closing == false`: now marks the leg
permanently degraded via the existing `legState.recordFailure`
machinery, records a new `"asr_stream_closed"` reason via a
`recordFallbackReason` helper mirroring `recordFallbackErr`'s existing
`"circuit_open"` pattern, and forwards whatever was buffered in
`leg.audio` as one final passthrough chunk before returning).
**Deliberately not attempted:** keeping the leg "alive" for audio pushed
*after* this point -- `runLeg` is the only consumer that ever drains
`leg.audio`, so once it returns, future `Push{Caller,Agent}Audio` calls
still succeed (buffering into the ring buffer per existing behavior) but
nothing forwards that audio again until `Session.Close()`. Documented
explicitly as a known, honest gap for a future sprint rather than
silently left unaddressed. 2 new tests in `session_test.go` (permanent
closure degrades the leg, records the reason, and forwards buffered
audio; a normal `Close()` does *not* trigger any of that).

**QA -- integration coverage for both fixes, one expected test update,
WER corpus growth (41 -> 47)**
Two new root-level integration test files:
`asr_circuit_breaker_integration_test.go` (drives PE's real
`DeepgramRecognizer`/`SarvamRecognizer` against a dead fake WebSocket
server to prime the breaker open, then confirms `langstream.NewSession`
fails in under 200ms with an error wrapping `asr.ErrCircuitOpen` for
both vendors -- proving the fast-fail path, not just a unit-level
check) and `asr_permanent_failure_integration_test.go` (drives a real
`*langstream.Session` with a local fake `asr.StreamSession` whose
`Transcripts()` channel is closed directly; confirms both legs degrade,
buffered audio is forwarded, and `-race -count=3` plus a 20-iteration
goroutine-leak check come back clean). Both were tested against PE's
and Tech's *real*, finished implementations, not fallback local fakes --
their work had landed on disk by the time QA reached those steps.

One **expected** test breakage, found and fixed (QA-owned file, not a
new bug): `integration_vendor_test.go`'s pre-existing
`TestVendorRoundTrip_ASRFatalErrorDoesNotHangSession` asserted "no audio
ever reaches the listening party" after a fatal ASR error -- exactly the
behavior Tech's fix corrects. Updated to assert the new, correct
contract (leg degrades, buffered audio arrives as passthrough, `Close()`
still doesn't hang).

WER corpus grew 41 -> 47 with 6 new, hand-verified error shapes not
previously represented: punctuation-only mismatch, a single entry mixing
all three edit types together, a currency-symbol-vs-spelled-out-words
mismatch, a pure-substitution WER==1.0 case (distinct from the existing
WER>1.0 hallucination case), a short common-word homophone ("to"/"too"),
and three non-adjacent deletions in one long sentence (previous max was
two). `corpus_test.go` and `wer_measurement_test.go` updated in sync.

`-race -count=10` flakiness audit on the root package, `pkg/qa`,
`pkg/asr`, and `pkg/langstream`: clean, no flakes found.

### Bugs found/fixed
One, described above under QA: `integration_vendor_test.go` asserted the
exact old (now-fixed) behavior Tech's change replaces. Not a new defect
-- an expected test update following an intentional, correct behavior
change. No other bugs found this run.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` -- all 11 buildable packages pass
  (`examples/backend_selection`, `tools/latency_benchmark` have no test
  files), no flakes, including `pkg/webrtcgw`/`cmd/langstream` (verified
  today for the first time since Sprint 11 -- Sprint 12 could not reach
  them at all)
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces -- still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) still cannot start
  without Saurabh's decision on anchor customer(s) / live traffic --
  unchanged.
- Docker-build verification, legal review of `docs/compliance.md` -- both
  unchanged, still need a human.
- The intentional, documented gap this run's Tech fix leaves behind
  (audio pushed to a permanently-dead leg after the fact still buffers
  but is never forwarded) -- not urgent, flagged for a future sprint, not
  a regression.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that supersedes everything below.
2. Absent that: consider whether the leg-death gap Tech's fix
   intentionally left open (audio pushed after a leg permanently dies
   still isn't forwarded) is worth closing -- would need a redesign
   (a replacement consumer for `leg.audio` once `runLeg` exits), not
   urgent today.
3. Continue opportunistic hardening (WER corpus, race-pattern audits) if
   no higher-priority item exists.
4. Keep an eye on whether today's clean disk situation holds on the next
   run or whether Sprints 8-12's pressure pattern resumes -- one clean
   run doesn't yet prove the underlying cause is gone.


## 2026-07-18 (Sprint 12: sandbox disk exhaustion — partial health check only, no code shipped) — scheduled run

### Agents run
None. No PE/Tech/SRE/QA workstream agents were spawned this run — see
below for why spawning them would have been irresponsible today.

### Repo health at start
Partial, and that partial result is itself the finding. ClearStream
checked (`git ls-remote --tags`): still only `v0.1.0`, no coordination
action needed (today's scope never touched `pkg/rtp`/duplex-RTP either
way).

**What was verified clean:** `pkg/asr`, `pkg/translate`, `pkg/tts`,
`pkg/rtp`, `pkg/observability`, `pkg/qa`, `pkg/langstream` — all seven
build, `go vet` clean, `gofmt -l` clean, and `go test` passes for each
(no `-race`, see below on why not).

**What could not be verified at all this run:** `pkg/webrtcgw`,
`cmd/langstream`, and the root-level `package langstream_test`
integration suite (`fallback_integration_test.go`,
`integration_vendor_test.go`, and by extension anything importing
`cmd/langstream`) — these pull in the `pion/webrtc` dependency tree
(dtls, ice, sctp, srtp, turn, mdns, etc.), and this run's sandbox ran out
of disk space partway through compiling that tree, twice, even after
every space-saving measure below. This is an infrastructure ceiling, not
a code problem — nothing in this entry implies those packages are broken,
only that this sandbox could not build them today to check.

### Sandbox disk crisis — now a hard blocker, not just a warning
Sprints 8 through 11 each flagged worsening disk pressure in this same
sandbox and routed around it. Today the workarounds stopped being enough:

- `$HOME` (`/sessions/...`) was at **100% full, 0 bytes free**, for the
  entire run, exactly as in Sprints 9-11 — could not write a single byte
  there.
- The **root filesystem itself started at 97% full (359MB free)** and
  dropped to as low as **85MB free** partway through just building the
  seven packages listed above as healthy — this is worse than every prior
  sprint's recorded low (Sprint 11: ~387-434MB).
- `/tmp` is confirmed (again) to be a persistent, multi-tenant scratch
  area that survives across scheduled runs, not just within one — found
  60+ leftover directories from past sprints and apparently other
  unrelated sessions entirely (e.g. `/tmp/qa15` contained `slide-*.jpg`
  files, nothing to do with this repo), almost all owned by `nobody`/
  other UIDs this run's user cannot delete (`rm -rf` fails with
  `Permission denied` file-by-file; `sudo` itself refuses to run in this
  container — confirmed, same as prior sprints).
- The Cowork outputs-mounted folder (`bindfs`) has ~4.4GB free but was
  confirmed **unusable even as scratch space**, separate from the
  documented git-index-lock issue: files written there cannot be deleted
  at all (`rm` returns `Operation not permitted` on a file this run's own
  user just created seconds earlier). One small, harmless test file/dir
  (`.lsbuild_test/`, a few bytes) was accidentally left behind there as a
  result and could not be cleaned up — flagging it here rather than
  hiding it; it is trivial and can be deleted by a human with the right
  permissions, or ignored.
- Workaround applied: reused a pre-existing, already-extracted Go 1.22.5
  toolchain sitting read+execute-only at `/tmp/gohome/go` (saved ~250MB
  versus extracting a fresh copy from `/tmp/go.tar.gz`, same trick as
  Sprint 11) and did all work in a fresh, uniquely-named,
  this-run-owned directory (`/tmp/lsbuild`) rather than fighting for
  space in already-occupied shared paths.
- Even with both of those savings, `go build ./...` across the *entire*
  repo (i.e. including `pkg/webrtcgw`/`cmd/langstream`'s pion/webrtc
  dependency tree) twice ran the root filesystem down to under 100MB free
  mid-compile and failed with `no space left on device` on dozens of
  packages simultaneously. Only after narrowing the build to the seven
  non-webrtcgw packages did a clean `build`/`vet`/`gofmt`/`test` pass
  become possible at all.

**Decision:** rather than force a full-repo build/test by further
shrinking scope (e.g. deleting the module cache the moment it's used,
package by package — fragile and still not guaranteed to fit), or push
any change without being able to run the full verification Step 5/6 of
this automation's own process requires (`go build ./...`, full
`go test ./... -race`, fresh-clone-from-GitHub rebuild), this run did
**not** spawn workstream agents and did **not** commit or push any code
change. Shipping unverified changes to compensate for a broken sandbox
would be a worse outcome than skipping a sprint. This DEVLOG entry itself
is the only change this run makes, and it needs no build to be safe to
push.

### Bugs found/fixed
None — no code was touched this run.

### Verified
- `pkg/asr`, `pkg/translate`, `pkg/tts`, `pkg/rtp`, `pkg/observability`,
  `pkg/qa`, `pkg/langstream`: `go build`, `go vet`, `gofmt -l` clean;
  `go test` passes (single run, no `-race` — the repeated/`-race` runs
  Step 5 normally requires were skipped to conserve the little disk
  headroom left for even this partial check).
- `pkg/webrtcgw`, `cmd/langstream`, root-level `langstream_test`
  integration suite: **not verified this run** (disk exhaustion, see
  above). No reason to believe they're broken — nothing touched them —
  but that's an inference from Sprint 11's clean state, not a
  measurement taken today.
- ClearStream: still tagged only `v0.1.0`, unchanged; `VERSIONING.md`'s
  pin is still accurate, no update needed.

### Blocked
- Everything Sprint 11 already listed as blocked (real-condition
  jitter-buffer tuning, Week 4, Docker-build verification, legal review
  of `docs/compliance.md`) — all unchanged, still need either live
  traffic or a human decision.
- **The sandbox disk-pressure pattern has now crossed from "annoying but
  routable-around" to "blocks a full verification pass outright."** Five
  consecutive scheduled runs (Sprints 8-12) have hit this, worsening each
  time. This is the top priority for whoever owns this automation's
  sandbox environment — not something the next scheduled run can keep
  absorbing by finding a cleverer workaround.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that supersedes everything below (unchanged from Sprint 11).
2. **Before any more feature work:** the sandbox needs either a genuinely
   clean disk per run, or a privileged cleanup of the accumulated
   `/tmp`/`/sessions` cruft from past runs/sessions between runs. Until
   that happens, any scheduled run touching `pkg/webrtcgw`/
   `cmd/langstream` (or anything importing them) risks the same partial
   verification this run hit — worth confirming fixed before trusting a
   future "full repo green" report at face value.
3. Absent an infra fix, the next run should retry the full-repo build
   early (before spending its time budget on anything else) to see
   whether the situation has improved, stayed the same, or worsened
   further, and report that delta explicitly.

## 2026-07-17 (Sprint 11: circuit-open reason propagated to orchestrator, race-pattern audit, WER corpus growth) — scheduled run

### Agents run
PE, Tech (coordinated pair), QA (three agents, in parallel). SRE not
needed today -- no scoped SRE-owned gap was found during planning (CI/
Docker/observability all clean since Sprint 10's audit). Week 3 remains 5
of 6 done (real-PSTN jitter tuning still needs live traffic) and Week 4 is
still entirely gated on Saurabh's anchor-customer/live-traffic decision --
unchanged since Sprint 8, neither closeable by agent automation today. Per
Sprint 10's own "Tomorrow" list, today closed out item #2 exactly
(tagging circuit-open failures with `RecordErrorReason(..., "circuit_open")`
at the `pkg/langstream` orchestrator level, not just the vendor-client
level Sprint 10 already covered), plus continued the standing
race-pattern-audit and WER-corpus-growth follow-ups from Sprint 8/9/10.

### Repo health at start
Clean: `go build ./...`, `go vet ./...`, `go test ./... -race` (all 12
packages), `gofmt -l .` clean, before any changes. ClearStream checked
(`git ls-remote --tags`): still only `v0.1.0` tagged, no coordination
action needed (today's scope never touched `pkg/rtp`/duplex-RTP).

### Sandbox note (environment, not a code bug)
Same recurring class of issue as Sprints 8-10, worse this run: `/sessions`
(where `$HOME` lives) was at 100% (0 bytes free) for the *entire* run --
literally could not write a single byte to `$HOME`, not even before
starting. Additionally, unlike prior runs, `/tmp` itself is a persistent,
multi-tenant scratch area that carries over **across scheduled runs, not
just within a session** -- found 60+ leftover directories/files from prior
runs (some from this repo's own past sprints, e.g. `/tmp/langstream_run`,
`/tmp/ls-em-work`, `/tmp/mywork`, `/tmp/mygo`, several hundred MB each),
almost all sticky-bit/other-UID protected so this run's user could not
delete them even with `rm -rf` (no `sudo` available either -- confirmed,
`sudo` itself refuses to run in this container). Root filesystem (`/`) was
at 96-97% full (record low: 387-434MB free) for this entire run purely
from that accumulated cruft, not from this run's own usage. Workaround:
same pattern as Sprint 9/10 -- did the entire run's work in a fresh,
uniquely-named own-user-owned directory (`/tmp/ls-work/LangStream`) rather
than fighting for space in already-occupied shared paths, and reused a
pre-existing extracted go1.22.5 toolchain already sitting (read-only, but
executable) at `/tmp/gohome/go` instead of extracting a fresh copy from
`/tmp/go.tar.gz` (saves ~250-300MB of the very tight budget). This makes
4 consecutive scheduled runs (Sprint 8, 9, 10, 11) hitting variations of
this same disk-pressure problem, and it is visibly getting worse each
time (Sprint 8: 96-99% full; Sprint 9-10: `/sessions` fully out; Sprint
11: `/` itself now also down to <400MB free from accumulated `/tmp`
cruft this run's own user can't clean up). Flagging again, more urgently:
this now looks like it needs a fix outside this repo (e.g. the
automation's sandbox should either start from a genuinely clean image
each run, or run a privileged/owner-level cleanup of stale `/tmp` state
between runs) rather than each run continuing to route around a shared
resource that never gets reclaimed.

### Changes

**PE -- exported circuit-breaker sentinel errors
(`pkg/translate/circuitbreaker.go`, `pkg/tts/circuitbreaker.go`)**
Previously-unexported `errCircuitOpen` in both packages renamed to
exported `ErrCircuitOpen` (identical message, zero behavior change) --
`pkg/translate/gpt4o.go`, `pkg/tts/cartesia.go`, `pkg/tts/elevenlabs.go`
updated to the new name. This is what makes a circuit-open rejection
distinguishable via `errors.Is` from any other package, which is exactly
what Tech's change (below) needed. 3 new tests
(`TestCircuitBreaker_OpenErrorIsErrCircuitOpen` and its Cartesia/
ElevenLabs counterparts) confirm `errors.Is(err, ErrCircuitOpen)` holds on
the real wrapped fail-fast error each vendor client returns.

**Tech -- circuit-open reason now tagged at the orchestrator level too
(`pkg/langstream/fallback.go`, `pkg/langstream/session.go`)**
Sprint 10 made circuit-open failures visible on the dashboard, but only
at the vendor-client layer (`pkg/tts/cartesia.go` etc. calling
`RecordErrorReason(..., "circuit_open")` directly) -- `pkg/langstream/
fallback.go`'s `recordFallback` helper, called from `session.go`'s
translate-error and TTS-synthesize-error paths, always recorded a plain,
reason-less `RecordError`, so the orchestrator layer itself couldn't tell
"my own circuit breaker just rejected this" from any other failure even
though the returned `err` carried that information. Added
`recordFallbackErr(rec, stage, vendor, err)` alongside the existing
`recordFallback`: checks `errors.Is(err, translate.ErrCircuitOpen) ||
errors.Is(err, tts.ErrCircuitOpen)` and tags `"circuit_open"` when true,
plain/empty reason otherwise -- byte-for-byte the same recording as
before for every non-circuit-open failure. Wired into the two `session.go`
call sites that actually have an `err` in scope (translate-error, TTS-
synthesize-error); the TTS-stall branch (no underlying `err`, a timeout
condition) and ASR-confidence/leg-degraded branches are unchanged, still
plain `recordFallback`. 8 new tests in `fallback_test.go` cover both
circuit-open cases, both ordinary-error regression cases (proving old
behavior is unchanged), plus nil-recorder/empty-vendor/nil-err edges.
Coordinated cleanly with PE in parallel -- confirmed via `git diff --stat`
that file sets never overlapped, and the PE-owned exported symbols were
already present (no wait/retry needed) by the time Tech's build ran.

**QA -- race-pattern audit (no fix needed, real negative result) + WER
corpus growth (35 -> 41)**
Per Sprint 8's outstanding "Tomorrow" item, audited every `*_test.go` in
the repo for the same test-synchronization race class found and fixed
that day (asserting state immediately after a background goroutine's
channel send, without accounting for that goroutine's own subsequent
"then update this other state" step). Examined ~20 candidate files;
everything else was already correctly synchronized (mutex/WaitGroup/
channel-close, or reads only the value actually delivered by the
send/callback itself, nothing "downstream" of it). One structurally
similar case was found -- `dashboard_latency_integration_test.go`'s two
tests, which read HTTP dashboard state immediately after
`drainUntilFinal` on the same `forwardAudio` goroutine the original bug
involved -- but 500+ `-race` runs (`-count=20/30/100`, varied `-cpu=1,2,4,8`)
came back clean every time, likely because the real httptest.Server
round-trip between the channel receive and the assertion adds enough real
wall-clock delay to close the original race window. Correctly not
"fixed" (nothing to fix), flagged here for visibility rather than left
silent. WER corpus grew 35 -> 41 with 6 new entries covering shapes not
previously represented: word-splitting ("helpline" -> "help line"),
word-merging ("up date" -> "update"), adjacent-word transposition,
case-sensitivity mismatch, and the corpus's first WER > 1.0 case (a
severe hallucination where the hypothesis is longer than the reference).
`corpus_test.go` and root `wer_measurement_test.go` updated in sync
(count guards, `want`/`wantWER` maps, fatal-message name lists).

### Bugs found/fixed
None this run -- all three workstreams' changes were additive/well-scoped
and verified clean on the first integration pass; the QA audit's one real
finding (above) was a negative result (test proven safe), not a bug.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` -- all 12 packages pass, no flakes
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces -- still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) still cannot start
  without Saurabh's decision on anchor customer(s) / live traffic --
  unchanged.
- Docker-build verification, legal review of `docs/compliance.md` -- both
  unchanged, still need a human.
- The recurring sandbox disk-pressure pattern (now 4 scheduled runs in a
  row, visibly worsening) -- see "Sandbox note" above. Flagging again,
  more urgently, as something that likely needs a fix outside this repo.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that supersedes everything below.
2. Absent that: `dashboard_latency_integration_test.go`'s two tests
   (flagged above) are a latent-but-unconfirmed race -- not urgent (500+
   clean runs), but worth an occasional re-check if the repo's real HTTP
   round-trip timing ever changes (e.g. if `httptest.Server` is ever
   swapped for something faster/in-process).
3. Continue opportunistic hardening (WER corpus, jitter stress tests) if
   no higher-priority item exists.
4. The sandbox disk-pressure pattern (see above) has now recurred and
   worsened across 4 consecutive runs -- worth raising directly with
   Saurabh rather than continuing to silently route around it each time.

## 2026-07-16 (Sprint 10: vendor circuit breakers, TURN credential support, cost-recording test + WER growth) — scheduled run

### Agents run
PE, Tech, QA, SRE (all four, in parallel). Week 3 remains 5 of 6 done
(real-PSTN jitter tuning still needs live traffic) and Week 4 is still
entirely gated on Saurabh's anchor-customer/live-traffic decision --
unchanged since Sprint 8. No message from Saurabh on that decision as of
this run, so today continued Sprint 9's "Tomorrow" list: (1) a
circuit-breaker/fail-fast layer on top of Sprint 9's retry/backoff for the
three request/response vendors (GPT-4o, Cartesia, ElevenLabs), since every
request during a sustained outage was still paying the full 3-attempt
retry budget; (2) the cost-recording integration test for GPT-4o/
Cartesia/ElevenLabs specifically, flagged as a gap in Sprint 9's report
(the existing test only covered Deepgram/Sarvam); (3) TURN server
credential support for `pkg/webrtcgw`/`cmd/langstream webrtc`, closing the
"no TURN server configured" gap flagged in the 2026-07-14 interactive
session's DEVLOG entry; (4) continued WER corpus growth.

### Repo health at start
Clean: `go build ./...`, `go vet ./...`, `go test ./... -race` (all 11
packages), `gofmt -l .` clean, before any changes. ClearStream checked
(`git ls-remote --tags`): still only `v0.1.0` tagged, no coordination
action needed (today's scope never touched `pkg/rtp`/duplex-RTP).

### Sandbox note (environment, not a code bug)
Same class of issue as Sprint 8/9's notes: the shared `/sessions` disk was
at 100% (0 bytes free) again for this entire run -- `$HOME` itself
couldn't be written to at all. Worked around exactly as Sprint 9 did: the
whole run's work happened in `/tmp/langstream_run/LangStream` (on `/`, not
`/sessions`), including the final push. `/tmp` itself was also nearly
full (~1.4G of leftover, mostly-undeletable multi-tenant scratch data from
unrelated sessions -- some sticky-bit/other-UID protected); worked around
the same way Sprint 9 did, with a uniquely-named own subdirectory rather
than fighting for space in shared `/tmp` paths. Disk stayed tight (usually
800M-1.5G free on `/`) for this run's entire duration -- worth flagging to
Saurabh if this becomes a recurring pattern, since it's now shown up in
3 consecutive scheduled runs (Sprint 8, 9, 10).

One workstream agent (Tech) hit a distinct infrastructure problem this
run: its sandboxed shell became unresponsive for most of its session
(`RPC error: process ... already running`, large file writes silently not
landing despite appearing to succeed in brief responsive windows). Tech
correctly detected this itself, verified via `wc -l` that no partial/
corrupted state was left behind, and reported back a fully-specified,
ready-to-apply design instead of guessing or forcing it through. The EM
(this session) then implemented that exact design directly during
integration (see Tech's item below) rather than re-spawning and burning
more time against a possibly-still-degraded shell.

### Changes

**PE -- circuit breaker / fail-fast on top of Sprint 9's retry/backoff
(`pkg/translate/gpt4o.go`, `pkg/tts/cartesia.go`, `pkg/tts/elevenlabs.go`,
new `pkg/translate/circuitbreaker.go` and `pkg/tts/circuitbreaker.go`)**
Every request during a sustained vendor outage was still paying the full
3-attempt retry budget (with backoff delays) before failing, burning
exactly the latency budget this product cares about most, right when a
vendor is already confirmed down. Added a small, thread-safe breaker per
client: opens after 5 consecutive *full-retry-exhaustion* failures
(permanent 4xx failures never count toward this, matching Sprint 9's
retry policy), fails fast with no attempt/backoff for a 10s cooldown, then
lets exactly one probe call through -- success closes the breaker, failure
reopens the cooldown. Configurable via `WithCircuitBreaker`/
`WithElevenLabsCircuitBreaker` functional options matching each package's
existing convention; on by default with no config needed. 19 new tests
against the existing fake HTTP/WS servers (plus new toggle-able fake
servers to flip vendor health mid-test) covering open/cooldown/probe/
close/reopen, permanent errors never tripping the breaker, and concurrent
access under `-race`.

**Tech -- TURN server credential support (`cmd/langstream/webrtc.go`,
`cmd/langstream/main.go`'s usage text, new `cmd/langstream/webrtc_test.go`)**
DEVLOG's 2026-07-14 entry flagged a real gap: `--stun` only ever built
anonymous `webrtc.ICEServer{URLs: [...]}` entries, so a real `turn:`/
`turns:` URL (which needs RFC 5766 long-term-credential auth, unlike
STUN) would just fail to authenticate. Added `--turn-username`/
`--turn-credential` flags; a new `iceServerForURL`/`buildICEServers` pair
attaches `Username`/`Credential` only to `turn:`/`turns:`-scheme entries
(case-insensitive prefix check, since STUN/TURN URLs per RFC 7064/7065
aren't hierarchical and `net/url.Parse` doesn't reliably yield the
scheme), and only when *both* flags are non-empty -- a half-supplied pair
is treated as not configured rather than sent partially. `stun:`/`stuns:`
entries, and the fully-default no-flags-set case, are byte-for-byte
unchanged from before. This workstream's own agent hit a sandbox
infrastructure failure mid-session (see the note above) and could not
land its own diff, but did fully specify the design and confirmed via
grep that no other file/test depends on `--stun`'s exact current shape --
the EM implemented that exact design directly during integration,
including the 12-case `webrtc_test.go` the agent had planned (covering
turn:/turns:/stun:/stuns: scheme handling, case-insensitivity, partial-
credential-pairs-are-ignored, whitespace/empty-entry handling, and the
unchanged-default-behavior case).

**QA -- GPT-4o/Cartesia/ElevenLabs cost-recording integration test +
WER corpus growth**
- New `gpt4o_tts_cost_integration_test.go` (repo root, matching
  `cost_tracking_integration_test.go`'s existing style): drives real
  `GPT4oTranslator`/`CartesiaSynthesizer`/`ElevenLabsSynthesizer` against
  fake servers sharing one `LatencyRecorder`, checking no double-counting
  (deliberately different call counts per vendor: 5/3/6), correct
  per-vendor attribution (exactly 3 correctly-keyed `CostSnapshot`
  entries), and correct units -- a fake GPT-4o server that echoes back
  real `prompt_tokens`/`completion_tokens` via `stream_options.
  include_usage` gives an *exact* 2.0x cost-doubling assertion when input
  length doubles, tighter than the ASR test's tolerance-banded check.
  Also confirms the same data surfaces through `BuildDashboardData`.
  Closes the exact gap flagged in Sprint 9's "Tomorrow" list.
- WER corpus grown 30 -> 35 entries (`pkg/qa/corpus.go`): an
  acronym/homophone case (EMI -> "emmy"), the corpus's first
  digit-insertion case, its first trailing-position insertion case, and
  two long-utterance cases (one mixing two different error types, one a
  long-utterance counterpart to an existing short two-insertion entry).
  `corpus_test.go`'s exhaustive `want` map and `wer_measurement_test.go`'s
  count guards updated in sync; every new WER matched hand computation on
  the first try.
- Cross-workstream check (`git diff --stat` + targeted greps, plus a
  `.gitignore` audit on every new file today): found two real issues in
  PE's circuit-breaker work (see Bug found/fixed below), confirmed Tech's
  workstream had landed nothing yet at check time (correctly reported,
  not assumed), and found one stray untracked file
  (`cmd/langstream/.write_test`, a leftover permission-probe artifact) --
  deleted by the EM before committing.

**SRE -- `RecordErrorReason`/`ReasonSnapshot` audit + addition
(`pkg/observability/metrics.go`, `dashboard.go`)**
Audited (per the SRE charter's "verify before proposing changes"
convention) whether a circuit-open fast-fail is distinguishable on the
dashboard from an ordinary single-request transient failure. Traced the
real call path (vendor client -> `pkg/langstream/fallback.go`'s
`recordFallback` -> `RecordError`) and confirmed a genuine gap: no error-
type inspection anywhere in that path, so a circuit-open storm (many
fast-fails/sec, near-zero latency, breaker protecting the pipeline)
renders identically to many genuinely-failed full-retry calls on the
dashboard -- exactly the ambiguity that would make the new breaker's
protection invisible during the one scenario it exists for. Added
`RecordErrorReason(stage, vendor, reason string)`/`ReasonSnapshot()` to
`pkg/observability/metrics.go` (`RecordError` now delegates to
`RecordErrorReason(..., "")`, byte-for-byte backward compatible --
verified with a dedicated test), a `..._error_reason_total{reason=...}`
Prometheus line, and a new dashboard table. 13 new tests (backward-compat,
reason isolation, concurrent-write race safety, HTML/JSON/`/metrics`
surfaces). Correctly did not wire real callers into `pkg/langstream/
fallback.go` (owned by another workstream) -- left that one-line call as
ready-to-use for whoever wires it. Also confirmed `go.mod`/`go.sum`
unchanged today, so skipped re-auditing the CI cache-key (no new deps to
audit against).

### Bug found/fixed

**Real bug, found by QA, fixed by EM during integration
(`pkg/translate/circuitbreaker.go`, `pkg/tts/circuitbreaker.go`):** if a
post-cooldown probe call's context was cancelled (or, found independently
during integration, if a *permanent* non-retryable error occurred) before
the call reached `recordSuccess`/`recordFailure`, `probeInFlight` was
never reset -- `allow()` then returned `false` forever, permanently
stuck open regardless of how much time passed (verified stuck even 24h
later in a test). Root cause: `allow()`'s in-flight-probe check ran
before checking whether that probe had ever actually resolved. Fixed with
a new `circuitBreaker.abort()` method (clears only `probeInFlight`, never
touches open/closed state or the failure count -- ctx cancellation and
already-excluded permanent errors aren't vendor-health signals) called
via `defer` immediately after every `allow()`-gated call in all three
vendor clients, guarded by a local `breakerSettled` bool so `abort()` is a
no-op whenever `recordSuccess`/`recordFailure` already ran. Two new
regression tests per package (`TestCircuitBreaker_AbortReleasesStuckProbe`,
`TestCircuitBreaker_AbortIsNoOpWhenClosed`) directly reproduce the stuck
scenario and confirm the fix. Also wired the now-real
`RecordErrorReason(stage, vendor, "circuit_open")` call into all three
vendor clients' circuit-open rejection path (SRE's new API had zero real
callers as landed -- a second, smaller issue QA flagged as "dead/
half-wired"), using the exact stage/vendor strings `pkg/langstream/
fallback.go` already uses (`"translate"`/`"tts"`, `Name()`) so the
dashboard groups them consistently with existing error-rate data.

**Housekeeping:** deleted a stray untracked `cmd/langstream/.write_test`
(leftover permission-probe artifact, not gitignored) before committing, so
it doesn't get swept in by `git add -A`.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` -- all 11 packages pass, no flakes
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces -- still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) still cannot start
  without Saurabh's decision on anchor customer(s) / live traffic --
  unchanged.
- Docker-build verification, legal review of `docs/compliance.md` -- both
  unchanged, still need a human.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that supersedes everything below.
2. Absent that: `pkg/langstream/fallback.go`'s `recordFallback` call
   sites could tag circuit-open errors with `RecordErrorReason(...,
   "circuit_open")` at the langstream-orchestrator level too (today's fix
   tags it at the vendor-client level, which already gets it onto the
   dashboard; a langstream-level tag would let fallback-triggering logic
   itself branch on "vendor confirmed down" vs. "one flaky call" if that
   ever becomes useful, but isn't needed for dashboard visibility, which
   is done) -- low priority, not a gap, just a possible enhancement.
3. The recurring `/sessions`-disk-full pattern (3 scheduled runs in a
   row now) may be worth a permanent fix outside this repo (e.g. always
   cloning to `/tmp/<unique>` by default instead of discovering the full
   disk each time) rather than continuing to route around it fresh every
   run.
4. Continue opportunistic hardening if no higher-priority item exists.

## 2026-07-15 (Sprint 9: vendor retry/backoff, per-vendor cost wiring, webrtcgw idle-room hardening) — scheduled run

### Agents run
PE, SRE, Tech, QA (all four, in parallel). Week 3 remains 5 of 6 done
(real-PSTN jitter tuning still needs live traffic) and Week 4 is still
entirely gated on Saurabh's anchor-customer/live-traffic decision — same
as Sprint 8, neither is closeable by agent automation today. Per Sprint
8's "Tomorrow" list, today's scope was hardening rather than new roadmap
checkboxes, this time across all four workstreams since there was real,
scoped work in each: (1) two real vendor clients (GPT-4o, Cartesia,
ElevenLabs) had zero retry/backoff on transient failures, unlike
Deepgram/Sarvam; (2) `observability.RecordCost` — the Week 3 "per-vendor
cost" dashboard primitive — had zero real callers anywhere in the vendor
clients despite the dashboard already being able to display it; (3)
`pkg/webrtcgw/room.go` (added 2026-07-14) had no idle-room timeout, a
resource leak for any room where a second peer never shows up.

### Repo health at start
Clean: `go build ./...`, `go vet ./...`, `go test ./... -race` (all 11
packages), `gofmt -l .` clean, before any changes. ClearStream checked
(`git ls-remote --tags`): still only `v0.1.0` tagged, no coordination
action needed (today's scope never touched `pkg/rtp`/duplex-RTP).

### Sandbox note (environment, not a code bug)
Same class of issue as Sprint 8's note, worse this run: the shared
`/sessions` disk was at 100% (0 bytes free) for the entire run, not just
96-99% — `$HOME` itself (where the task's own instructions say to clone)
could not even write a `.git/index.lock`. Pointing `GOCACHE`/`GOPATH`/
`TMPDIR` at `/` (which had ~2.5G free) alone wasn't sufficient this time,
because the git working tree at `$HOME/LangStream` itself lived on the
full `/sessions` mount. Workaround: re-cloned the repo into `/tmp/ls-em-
work/LangStream` (on `/`, not `/sessions`) and did the entire run's work
there instead, including the final push. `/tmp` itself turned out to be a
shared, multi-tenant scratch space with leftover multi-hundred-MB
directories from unrelated prior sessions/automations (some not
deletable — sticky-bit-protected, owned by other UIDs); worked around by
creating a uniquely-named own-user-owned subdirectory
(`/tmp/ls-em-work/`) rather than trying to clean up or reuse those. Not a
repo issue — noting here in case a future run hits the same thing and
wants the faster path directly to a `/tmp`-based clone instead of
diagnosing `/sessions` capacity first.

### Changes

**PE — retry/backoff for transient vendor errors (`pkg/translate/gpt4o.go`,
`pkg/tts/cartesia.go`, `pkg/tts/cartesia_ws.go`, `pkg/tts/elevenlabs.go`,
plus new `pkg/translate/backoff.go` and `pkg/tts/backoff.go`)**
Deepgram and Sarvam (ASR) already shared `pkg/asr/backoff.go`'s capped-
exponential-with-jitter `reconnectBackoff`; GPT-4o and both TTS vendors had
none — a transient 429/5xx/reset failed the whole request outright. Added
package-local retry helpers mirroring that same policy (3 attempts, 150ms/
1200ms base/max) to all three, retrying only on 429/5xx/connection-reset/
timeout — never on 4xx auth/bad-request errors, and never masking `ctx`
cancellation. Tested against fake HTTP/WS servers: retry-then-succeed,
retry-exhaustion, and fail-fast-on-4xx cases for each vendor.

**PE — `observability.RecordCost` wired into all five real vendor clients**
(`pkg/asr/deepgram.go`, `pkg/asr/sarvam.go`, `pkg/translate/gpt4o.go`,
`pkg/tts/cartesia.go`, `pkg/tts/elevenlabs.go`) via a new `WithMetrics`/
`WithSarvamMetrics`/`WithElevenLabsMetrics` functional option per package
(nil recorder = no-op, matching each package's existing options
convention). Approximate documented per-unit pricing: Deepgram $0.0059/min,
Sarvam $0.006/min (flagged as a thinner-docs assumption), GPT-4o
$2.50/$10.00 per 1M input/output tokens (real token counts via
`stream_options.include_usage=true` when the API returns them, ~4 chars/
token fallback otherwise), Cartesia $0.00004/char, ElevenLabs $0.00018/
char — every constant has an inline comment citing the assumption and
stating "pilot cost-visibility only, not billing-grade." This closes the
gap QA's cost-tracking integration test (below) verified end to end.

**SRE — audited (no code changes needed)**
Checked whether `pkg/observability/dashboard.go`'s HTML/JSON/`/metrics`
routes already surface `CostSnapshot()` — they do, built in Sprint 3
(2026-07-09), just with no real data flowing through it until PE's change
above. Also audited `.github/workflows/ci.yml`'s cache-key behavior against
the heavier `pion/webrtc` dependency tree added 2026-07-14 — `setup-go`'s
default `**/go.sum`-hash cache key already covers it correctly (single
root `go.mod`/`go.sum`, no nested modules, no per-branch/OS key
fragmentation). No changes made to either file; verified rather than
assumed in both cases.

**Tech — idle-room timeout + max-concurrent-rooms cap (`pkg/webrtcgw/room.go`,
new `pkg/webrtcgw/room_test.go`)**
A room where only one peer ever joins (dead shared link, browser crash
before joining, abandoned tab) previously lived forever — its Session,
goroutines, and buffers never freed. Added `DefaultRoomIdleTimeout` (2
minutes, configurable via new `WithIdleTimeout` `ManagerOption`,
`d<=0` disables it) and an optional `WithMaxRooms` cap on concurrent
rooms (checked only at new-room creation, never blocks joining an
existing room as its second peer). `NewManager`'s signature stays
backward compatible (`opts ...ManagerOption`). Handles the race between
`expireIncomplete` firing and a second peer's concurrent `Join` via a
`Room.closed` flag shared by both cleanup paths, with `roomFor` retrying
against a stale closed room. Tested: idle single-peer room is expired
and its `PeerConnection`/`Session` actually closed; a full two-peer room
is never touched by the timeout even at 5x its duration; new-room
creation is rejected past the `WithMaxRooms` cap while joining an
existing room isn't.

**QA — cross-cutting verification + WER corpus growth**
- New `cost_tracking_integration_test.go` (repo root): drives real
  `asr.DeepgramRecognizer`/`asr.SarvamRecognizer` against fake WS servers
  with a shared `LatencyRecorder`, checking exactly the three ways PE's
  cost wiring could have been subtly wrong in isolation — double-counting
  (`CostEventCount` matches exact call count), wrong-vendor attribution
  (`CostSnapshot` has exactly 2, correctly-keyed entries), wrong units
  (cost scales with audio duration, not call count) — and confirms the
  same data surfaces through `BuildDashboardData`.
- New `pkg/webrtcgw/idle_room_qa_test.go`: independently verified Tech's
  idle-room cleanup via `runtime.NumGoroutine()` before/after (both a
  single batch and repeated batches, to catch a slow cumulative leak),
  going beyond Tech's own room-count/PeerConnection-state assertions.
- WER corpus grown 25 → 30 entries (`pkg/qa/corpus.go`): five new
  Hindi-English code-switching cases (negation deletion that flips
  meaning, two acronym/homophone substitutions, a two-insertion case, a
  long-utterance two-deletion case), same style/verification convention
  as existing entries; `wer_measurement_test.go`'s hardcoded entry-count
  guards updated in sync.
- Fresh-clone-class `.gitignore` audit: `git check-ignore -v` against
  every file touched by all four agents today, plus hypothetical new
  filenames under each changed package — no matches, no repeat of the
  2026-07-07 whole-package-exclusion bug.

### Bug found/fixed

**Real bug, found by QA, fixed by EM during integration
(`pkg/webrtcgw/room.go`):** Tech's idle-timeout refactor of `Join`
silently dropped the pre-existing
`p.pc.OnConnectionStateChange(func(state) { ... m.leave(...) ... })`
registration that used to fire on `Failed`/`Closed`/`Disconnected` — QA
caught it via `git diff` + a repo-wide grep showing zero remaining
`OnConnectionStateChange` registrations outside stale comments. Effect:
a *full* two-peer room whose media path died at the ICE/DTLS level
(not a clean WebSocket close) would never have been cleaned up — the
exact bug class being fixed today, just on the opposite side (a full
room instead of an incomplete one). Restored the registration in `Join`
right after peer creation; `m.leave` was already idempotent against an
already-closed room (needed for the idle-timeout path anyway), so no
further changes were required to make it safe. Re-verified clean after
the fix: full repo `go build`/`go vet`/`gofmt -l .` clean, `go test
./... -race -count=3` all 11 packages pass, no flakes.

### Verified
- Full repo: `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./... -race -count=3` — all 11 packages pass, no flakes
- Fresh-clone-from-GitHub rebuild performed after push (see below)

### Blocked
- Real-condition jitter-buffer tuning against live PSTN traces — still
  needs live/pilot call traffic (Week 4). Unchanged.
- Week 4 (live pilot, real WER/latency/CSAT, go/no-go) still cannot start
  without Saurabh's decision on anchor customer(s) / live traffic —
  unchanged, flagging plainly rather than inventing scope.
- Docker-build verification, legal review of `docs/compliance.md` — both
  unchanged, still need a human.

### Tomorrow
1. If Saurabh has an anchor-customer/live-traffic decision for Week 4,
   that supersedes everything below.
2. Absent that: GPT-4o/Cartesia/ElevenLabs cost-wiring now exists but
   only Deepgram/Sarvam got a dedicated QA cross-check today (PE's own
   change landed cost-wiring for all five, but QA's integration test only
   covered the two ASR vendors since translate/tts hadn't landed yet when
   QA started that test) — worth a follow-up integration test covering
   GPT-4o/Cartesia/ElevenLabs cost-recording specifically.
3. Continue opportunistic hardening (WER corpus, jitter stress tests) if
   no higher-priority item exists.

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
