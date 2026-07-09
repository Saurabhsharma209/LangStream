# Compliance — DPDP Data-Residency Assessment & Call Consent (Draft, Week 3)

**Status:** Preliminary assessment for the pilot scope in `ROADMAP.md`. This
is not legal advice — before any anchor customer (especially BFSI) goes
live, this document needs sign-off from Exotel's legal/compliance team, not
just engineering judgment. Owner: PM. Last updated: 2026-07-09 (Sprint 3).

## 1. What data actually leaves India in the current pilot design

LangStream's pipeline for a single call leg is: caller/agent audio (PCM) →
ASR (transcript + partial confidence) → MT (translated text) → TTS
(synthesized audio) → other party. Today's Week 2 vendor wiring
(`pkg/asr`, `pkg/translate`, `pkg/tts`) is real client code tested against
fake servers (see `DEVLOG.md` 2026-07-08) — no live vendor traffic has
flowed yet, but the *code path* that will carry customer voice data once
keys exist is now written, so this is the right time to assess it, before
it's live rather than after.

| Vendor (pilot) | Function | Data sent | Known/likely hosting |
|---|---|---|---|
| Sarvam AI | Hindi ASR (code-switch aware) | Raw caller/agent audio | India-based vendor; India hosting is Sarvam's stated positioning, but the pilot integration (`pkg/asr/sarvam.go`) does not yet pin a specific data-residency contractual guarantee — **needs a signed DPA confirming in-region processing before live BFSI traffic**, not just a marketing claim. |
| Deepgram | English ASR | Raw caller/agent audio | US-headquartered; region selection depends on plan/contract. **Needs explicit region-pinning in the account/contract**, not assumed from the API alone. |
| OpenAI GPT-4o | Hindi↔English translation | Transcript text (no raw audio) | US-hosted by default; OpenAI's enterprise/API terms have historically not guaranteed India-region processing for the standard API tier. This is the pilot's clearest residency risk today. |
| Cartesia | TTS | Translated text, returns synthesized audio | US-based vendor; region guarantees not yet confirmed for this integration. |

**Bottom line:** as currently scoped, the pilot's translation step (GPT-4o)
and likely also ASR/TTS route call content through US-hosted
infrastructure by default. Whether that's *permitted* depends on what kind
of data it is and who the anchor customer is — see below.

## 2. What DPDP (Digital Personal Data Protection Act, 2023) actually requires here

Two things matter for this pilot, and they cut differently:

1. **DPDP itself does not impose a blanket India-data-residency
   requirement** for most personal data processors — cross-border transfer
   is allowed by default *except* to countries the Central Government
   notifies as restricted (a blocklist model, not an allowlist), and the
   relevant rules/notified list were still being finalized as of this
   assessment. **Action item:** confirm current status of the notified
   country list and the DPDP Rules before the pilot's first live BFSI call
   — this is a fast-moving regulatory area and this document will go stale
   quickly if not re-checked.
2. **Sector-specific rules can be stricter than DPDP.** This is the actual
   binding constraint for a BFSI anchor customer: RBI's data-localization
   directions (originally the 2018 payment-systems-data circular, and
   related RBI guidance since) require certain categories of financial
   transaction/customer data to be stored and processed only in India,
   independent of what DPDP itself allows. **If the anchor customer is a
   bank/NBFC/payments entity and the call content includes anything RBI
   would classify as payment or customer financial data, DPDP's more
   permissive cross-border stance does not override RBI's localization
   requirement.** This is the assessment's key finding: the binding
   constraint on this pilot is very likely RBI's rules, not DPDP's, and
   the two should not be conflated when scoping which anchor customers are
   safe to onboard first.

## 3. Recommendation for the pilot

- **Do not default to GPT-4o (or any US-hosted vendor) for a BFSI anchor
  customer's live calls until legal confirms, in writing, whether RBI
  localization applies to that customer's call content.** For a
  non-BFSI anchor customer (if one exists in the pilot's 1-2 slots), the
  DPDP cross-border position is more permissive and the bar is lower, but
  still needs a consent basis (see §4) and a data-processing agreement
  with each vendor.
- **Get vendor DPAs (data processing agreements) in place** confirming
  each vendor's actual processing region and retention policy — Sarvam's
  is the most likely to already be India-region; OpenAI's is the one most
  likely to require an enterprise-tier agreement to pin region, if that's
  even offered for the API tier in use.
- **If RBI localization applies:** the pragmatic Week 3/4 options are
  (a) hold the BFSI anchor customer's pilot for a self-hosted-model track
  explicitly marked out of scope for this month (see `ROADMAP.md`'s
  "explicitly out of scope" section — this would need to be revisited,
  not silently worked around), or (b) select the pilot's first anchor
  customer from a non-BFSI or DPDP-only-applicable segment instead, and
  treat BFSI as a Phase 2 decision once residency is actually solved.
  **This is a decision for Saurabh/product leadership, not something
  engineering should resolve by picking a customer unilaterally.**

## 4. Consent / disclosure language for calls routed through AI translation

Every call where LangStream is active must disclose that AI-assisted
real-time translation is in use, distinct from and in addition to any
existing call-recording disclosure. Two variants below — pick per
customer/regulatory context; legal should confirm final wording before
production use, this is a starting draft:

**Short IVR/voice-prompt version (played before or at start of the AI-translated segment):**

> "This call may use AI-assisted real-time translation. Voice and speech
> content may be processed by third-party translation services. If you'd
> prefer not to continue, you can request a human interpreter instead."

**Longer version (for written consent capture, e.g. app/web flow before a call is placed):**

> "During this call, your voice may be transcribed and translated in
> real time by an AI system to enable communication between [Hindi/English]
> speakers. This involves sending audio and/or transcribed text to
> third-party AI service providers for processing. [Company] does not use
> this data to train AI models beyond what's needed to deliver the
> translation for your call. You may opt out and request a human
> interpreter at any point during the call."

**Open items for legal, not resolved by this draft:**
- Whether disclosure must be affirmative opt-in (explicit consent before
  the call proceeds) or opt-out-capable disclosure (proceed unless the
  caller objects) — likely depends on the anchor customer's sector and
  existing call-recording consent posture.
- Retention: how long transcripts/audio are retained by each vendor and by
  Exotel, and whether the consent language needs to state a retention
  period explicitly (DPDP's purpose-limitation and storage-limitation
  principles suggest it should).
- Whether the "does not use this data to train AI models" claim in the
  long-form draft is actually true for every vendor in §1 — **this needs
  verification against each vendor's actual terms before this exact
  sentence is used with a real customer**, it's written here as a
  placeholder assumption, not a confirmed fact.

## 5. What this document is not

This is Week 3 groundwork, not a compliance sign-off. It exists so the
Week 4 go/no-go decision (`ROADMAP.md`) is made with the residency and
consent questions already surfaced, not discovered during a live pilot
call. Re-review before Week 4 launch, and immediately if the anchor
customer selection changes.
