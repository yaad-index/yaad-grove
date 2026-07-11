# ADR 0014 — Bounded per-conversation memory (shared conversation buffer)

**Status:** Accepted (2026-07-11)
**Amends:** ADR 0004 (logging boundary), ADR 0008/0011 (answer composition + grounding), ADR 0012 (consent gates logging)

## Context

Context-dependent follow-ups fail: "tldr", "shorter", "what about X" have nothing to refer to because the bot keeps no recent-turn context. Each message is answered in isolation. This is an architecture gap, not a persona/tone problem (ADR 0013) — no amount of friendly wording gives the model a last answer to summarize.

## Decision

Add a **bounded, per-conversation short-term memory buffer**, keyed by conversation (chat id), injected into the answer prompt as recent conversation context. It is **speaker-attributed** (the bot knows who said what and can say "as Bob asked…") and **consent-gated on entry**. It is conversational context, never a knowledge source.

## What it governs

- **Follow-up and meta continuity.** "tldr"/"shorter" summarize the bot's own last answer; "what about X" continues the topic with referential context.
- **Speaker attribution.** Turns carry the speaker, so the bot can reference who said what.
- **One buffer per conversation.** A group chat has one shared buffer for all its members; a DM is naturally 1:1 and uses the same chat-keyed mechanism.

## Turn structure

Each buffered turn is `{speaker, text, timestamp, reply-to-ref}`. The buffer preserves conversation **structure**, not just a flat list of lines:

- **Chronological order.** Turns carry a timestamp and are injected in true time order, so the model reads the conversation as it actually unfolded.
- **Reply-to threading.** A turn records which message it is replying to, so the model sees "X replied to Y" rather than a flat sequence — threading that matters in a busy group where several topics interleave.

A reply-to reference whose target is **not in the buffer** — e.g. a reply to an unconsented user's turn, gated out on entry — is rendered as "reply to a message not shown," never a dangling reference. This is the reply-to counterpart of the speaker-gap disclosure below: an absent target means not-shown / not-consented, not a broken reference.

## What it does NOT govern (the hard boundary)

- **Grounding.** Buffered turns are referential context, NOT facts. The grounding guarantee (ADR 0008/0011) is unchanged: any NEW factual claim still requires retrieval. "tldr" re-summarizes the bot's own already-grounded last answer (no new claim); "what about X" is a fresh grounded query on X. The buffer can never become a scope-bypass that lets the model assert prior-turn content as fact without re-grounding.
- **Scope, knowledge, consent, ACL.** Unchanged. The buffer is not the vault; facts go in the vault. Gate, rate limit, and ACL decisions are unaffected.

## Consent gate on entry, and the speaker-gap disclosure

Only a **consented** member's turns (plus the bot's own answers) enter the group buffer; an unconsented member's messages stay out — consistent with the ADR 0004/0012 rule that consent gates logging.

**Only engine answers are buffered on the bot side.** A `dmConsentFlow` reply (opt-in prompt, status, nudge) never reaches the engine and is operational, not conversational — it generates no buffer entry. The buffer holds consented human turns plus the model's own answers, nothing else.

Because entry is consent-gated, the buffer is a **partial transcript** with speaker gaps (unconsented turns absent). The injected context must **explicitly tell the model**: this is a partial record — only consented participants appear; a gap means not-shown / not-consented, NOT that no one spoke. Otherwise the model misreads absence as silence.

## Grounding preservation (prompt ordering)

persona (ADR 0013) → scope → grounding instruction → **conversation buffer (labelled: recent context, partial, not a fact source)** → retrieved context → query. The grounding instruction precedes and governs the buffer: factual claims still resolve from retrieval, not from recalled turns.

## Interaction with the quarantine log (ADR 0004)

Distinct stores, distinct lifecycles: the quarantine log is **write-only, append-only** (curation corpus, never read by the bot); the buffer is **read-into-prompt, transient, bounded**. A consented group turn enters both. The **bot's own answers enter the buffer** (so "tldr" works) but NOT the quarantine — the corpus is community content, not bot output.

## DM

Admin DM answers reach the engine (ADR 0012), so an admin DM gets a 1:1 buffer (admin turns + bot answers) for follow-up continuity — **private, not corpus-logged** (matching the DM-not-logged rule). Non-admin DMs are consent-management only (no engine, no answers), so they generate no answerable turns.

## Injection model

**Remembering and injecting are decoupled.** The buffer *retains* many turns, but only a small, relevant slice *enters* any given prompt — so the buffer can be generous without every answer paying to carry all of it:

- `--memory-turns` — how many structured turns the buffer **retains** (default 100).
- `--memory-inject` — how many turns actually **enter** a prompt (small default, ~15).

Per query, the injected slice is selected:

1. **Recency floor.** Always include the last few turns verbatim, for immediate continuity.
2. **Relevance.** Retrieve the most relevant retained turns for the query through the same retrieval path as the vault (full-text in v1, embeddings later) — so all retained turns stay available, but only the relevant slice is injected.
3. **Follow-up gate.** Skip history injection entirely for a standalone question; pull it only when the message references prior context — a reply to the bot, pronouns, or a meta request ("tldr", "more", "why"). A cheap heuristic for v1: a false negative degrades to today's isolated-answer behavior, a false positive costs only up to `--memory-inject` tokens, so no separate knob is needed — the gate is binary and improvable later.

Injected turns keep their structure (speaker, timestamp, reply-to) and are ordered chronologically. Grounding is unchanged: retrieved turns are conversational context, never facts.

**Reply-to, two uses (both from the incoming update, no new bookkeeping):** the follow-up gate's reply-to-bot signal already exists — unit-a directed-detection (ADR 0012) reads `reply_to_message.from.id == botID` — so the gate reuses it, no sent-message id store. The buffer's `reply-to-ref` is separate: turns are keyed by `message_id`, so a reply-to can point at a prior turn (or render "not shown" when the target isn't buffered). Both derive from the update the adapter already receives.

## Storage and lifecycle (decided)

- **In-memory.** The buffer lives in process memory, not a persistent store. Conversation context is transient and recent by nature; a restart clearing it degrades gracefully (recent follow-up context is lost, nothing else), and it avoids a second persistent copy of message content beyond the append-only corpus. A second bbolt store for ephemeral chat state is not worth the lifecycle cost. (Persistence is deferred.)
- **Purge on withdrawal.** On `/consent remove`, the withdrawing user's turns are purged from the live buffer immediately. This is a deliberate departure from ADR 0012's prospective rule for the append-only corpus: the buffer is small, transient, and **actively read into prompts**, so a withdrawn user's turns must stop shaping answers at once — categorically different from a corpus the bot never reads.
- **Bounds — count-primary.** The headline retain bound is a message count: the buffer holds the **last 100 messages** by default (`--memory-turns`). This is a deliberate chat-context choice — chat is dense and recent context is what makes follow-ups work. A TTL is a secondary, optional knob (a conversation can also go stale by time), but the count is the primary retain bound. What actually enters a prompt is the smaller `--memory-inject` slice (see Injection model).

## Cost note

Retaining 100 turns is cheap (in-memory); the per-call token cost is set by **injection**, not retention. Only up to `--memory-inject` turns (~15) ever enter a prompt, and a standalone question injects **none** (the follow-up gate). So a large buffer does not tax every answer: the cost is bounded by `--memory-inject`, tunable down (or the buffer disabled), and the global spend ceiling (ADR 0006) caps total spend regardless.

## Configuration

- `--memory-turns` — how many structured turns the buffer retains; **default 100**; `0` disables the buffer entirely.
- `--memory-inject` — how many retained turns enter a prompt; small default (~15).
- `--memory-ttl` — optional secondary staleness cap on retention; unset means count-only.

On by default (the failing "tldr" is the motivating bug), and fully disable-able.

## Deferred

- Cross-conversation / long-term memory.
- Persistence across restarts.
- Embeddings-based relevance for injection selection (v1 uses the full-text retrieval path).
- Richer follow-up detection (v1 is a binary heuristic bounded by `--memory-inject`).
- Summarization / compaction of aged turns.
- Entity extraction.

Complementary to ADR 0013 (persona) but independent.
