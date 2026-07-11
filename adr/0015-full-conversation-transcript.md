# ADR 0015 — Full role-tagged conversation transcript

**Status:** Accepted (2026-07-11)
**Amends:** ADR 0004 (quarantine log — human-only, curation corpus), ADR 0008/0011 (answer composition — bot output now also persisted, separately), ADR 0012 (consent gates logging; withdrawal posture), ADR 0014 (conversation buffer — contrast the withdrawal posture)

## Context

Two stores already persist message content, and neither is a conversation record:

- The **quarantine log** (ADR 0004) is a curation corpus. It holds **only human community messages**, is **write-only** (the bot never reads it), and exists so a later admin-in-the-loop pass can propose vault edits. It deliberately does not record what the bot said.
- The **conversation buffer** (ADR 0014) is transient, in-memory, bounded, and **read back into prompts** for follow-up continuity. It is not durable and is not a record.

Neither answers "what was actually said in this group, by whom, including the bot?" — a full, durable, role-tagged transcript. That record is wanted for operator review, debugging the bot's own answers, and future evaluation of answer quality over real conversations. It is a **fourth concern**, distinct from grounding corpus, prompt context, and spend accounting.

## Decision

Add an optional **full conversation transcript**: a durable, append-only, **role-tagged** log of a group conversation — both **human** turns and the **bot's own answers** — written to a store **separate from the quarantine log**, and **never read by the answering or curation path**.

- **Separate store — one file per chat.** `--transcript-log` names a **directory**; each group chat gets its own append-only `<chat-id>.jsonl` inside it, not a single combined file. This keeps the store separate from the quarantine Entry (whose value is precisely being human-only community content; mixing bot turns in would corrupt the curation corpus) and keeps each conversation self-contained. The chat id is sanitized to a filesystem-safe filename (chat ids can be negative, e.g. `-5527987187`, so a leading `-` and any path separators are neutralized — no traversal).
- **Role-tagged.** Each entry carries a role (`human` / `bot` / `system`), the speaker (for human turns), timestamp, and message id — enough to reconstruct the conversation in order. The `system` role annotates operational outcomes so silences are explained (see below).
- **Off by default.** A `--transcript-log <dir>` flag; unset/empty means no transcript is written. When set, the directory is created and validated as writable at startup, failing loud if not (matching the startup-fatal posture of the other configured stores). Instances that don't want a second durable copy of content pay nothing.
- **Group-only.** DMs are not transcribed. The DM surface is consent management (non-admin) or a private 1:1 (admin); neither is community conversation. This matches the ADR 0012 rule that DM content is not corpus-logged.

## The anti-drift boundary (why the bot still never reads it)

The transcript records the bot's answers, but the bot **never reads the transcript** — not when answering, not when curating. This is the same structural isolation as ADR 0004's quarantine log, and it matters more here: if curation could read the bot's past answers, the vault could drift toward *what the bot has said* rather than *what the community knows*. Grounding stays anchored to the human-curated vault. The transcript is an outward-facing record (operator/eval), not an inward-facing source. The package has no read path the engine or curation could use — isolation is structural, not conventional.

## What is a bot-role entry, and the operational marker

A **bot-role entry records the engine's substantive response to a query on the serve path — an answer OR a refusal.** A refusal ("that's outside what I can answer from my curated sources") is the bot's real, negative response to what was asked, and the bot *did* send that text to the user, so a faithful record includes it. Both refusal paths — the no-model early-refuse and a model-composed persona-shaped decline (ADR 0016) — are bot turns; the transcript need not distinguish them.

**This deliberately does NOT mirror the ADR 0014 buffer.** The buffer excludes refusals (`!reply.Refused`) because a canned decline is useless *as injected prompt context* — that is the buffer's whole purpose. The transcript's purpose is different: a faithful *audit record*. For audit, "the bot declined, with this text" is exactly what should be recorded, so the buffer's exclusion rationale does not transfer. Same word, `Refused`; opposite correct handling, because the two stores read (or never read) their contents for opposite reasons.

**Not bot turns — operational outputs:** the rate-limit notice, the consent nudge, and consent-flow replies are operational, not the engine's response to the query. They generate no bot entry.

**The operational marker (`role: system`).** One operational case still leaves a gap an operator would misread: a **rate-limited** directed message logs a human turn but no bot turn — "user spoke, bot went silent" reads like a bug in an audit log, which is exactly what the transcript must not do. So the rate-limited path emits a **`role: system` marker** (`event: rate_limited`) that explains the gap in place. This is the only marker case: refusals already carry their own bot turn; an **ambient** (log-only) message is not directed at the bot, so no answer is expected and its human-only entry is not a gap; **unconsented** users never enter the transcript at all. Rate-limited is the sole human-turn-without-a-bot-turn gap, and the marker makes it self-explaining.

The human turn that prompted the exchange is logged regardless of the bot-side outcome (see below) — only the *bot* side is outcome-dependent.

## Human turns follow the consent/logging gate

A human turn enters the transcript on the **same consent gate** that governs quarantine logging (ADR 0012): a **consented** member's group message is transcribed; an unconsented member's is not. This holds across outcomes — a consented human turn is transcribed whether it was **served**, **log-only** (ambient), or **rate-limited**. The gate is consent, not outcome: the record should show what a consenting participant said even when the bot could not answer. Only the *bot* side is outcome-dependent — a serve-path answer or refusal is a bot turn; a rate-limited outcome is a `system` marker instead (see above).

## Withdrawal posture — prospective, and disclosed

On `/consent remove`, the transcript posture is **prospective**: the bot **stops writing new transcript entries** for that user, but **past entries remain**. This is deliberately **different from the ADR 0014 buffer**, which purges immediately — and the difference is principled:

- The **buffer is actively read into prompts**, so a withdrawn user's turns must stop shaping answers *at once*; immediate purge is required there.
- The **transcript is never read** — not by answering, not by curation. A retained past entry shapes nothing. So there is no answer-integrity reason to purge it, and prospective (stop-writing) fully honours the withdrawal going forward.
- Retroactive purge of an **append-only** store would require tombstoning or rewriting lines — the same delete-in-place machinery a redaction tool needs. That is real surface area, and pulling it into the Required set to scrub a store nothing reads is not warranted. Append-only + prospective keeps the store simple and its audit trail intact.

This matches ADR 0004/0012's prospective rule for the append-only quarantine corpus. The transcript and the quarantine log withdraw the same way; only the actively-read buffer purges.

**Disclosed at consent.** Because the posture is prospective, the informed-consent surface (ADR 0012 unit-b disclosure) must **state that transcript entries persist historically** — a later withdrawal stops new entries but does not erase past ones. This makes "prospective + disclosed" coherent: the user opts in already knowing the record is durable, so retention after withdrawal is not a surprise. The disclosure line is added only when `--transcript-log` is active (no false promise when nothing is transcribed).

## Configuration

- `--transcript-log <dir>` — directory holding one append-only `<chat-id>.jsonl` per group chat; **unset/empty means off** (no transcript written, no disclosure line). When set, the directory is created and validated as writable at startup (fail-loud on error). The chat id is sanitized to a filesystem-safe filename before use.

## Deferred

- Transcript-backed evaluation / answer-quality scoring tooling (the transcript is the substrate; the analysis is separate).
- Redaction / retroactive scrub tooling (prospective withdrawal is the v1 posture; an append-only store keeps this out of Required).
- DM transcription (out of scope by design).
- Rotation / retention limits on the file (operator-side file management for v1).
