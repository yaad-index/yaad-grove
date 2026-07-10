# ADR 0009: Interactive actions and the callback security spine

**Status:** Proposed (2026-07-10)

## Context

The chat surface is growing from grounded Q&A + slash commands into the
front-end for an admin control plane. Q&A is one surface; authorized admin
operations are the other — "set this user's limit to X", or approving a
suggested action with one tap instead of retyping a command. That makes
interactive affordances — inline keyboards, callback queries, reactions, message
editing — first-class, not decoration.

Two forces shape the design. First, the grounding guarantee (ADR 0001) must stay
intact: a click is a control-plane event, not a model query, so the
retrieval/refusal path is untouched. Second, a button is exposed to every member
of a chat, so **its `callback_data` can be forged or replayed** — it can never be
trusted as authority on its own.

This ADR fixes the action model and the callback security spine. It is realized
in phases (T1–T4); T1 (the library swap, ADR-referenced there) and T2 (actions on
the wire) land first.

## Decision

### The feature is an ACL-gated Action layer; buttons are only its rendering

An `Action` is a typed operation the actor invokes with a tap. Its wire shape —
what a transport needs to render a button and round-trip a click — is minimal:

```
Action { Verb string; Params map[string]string; Label string }
```

`core.Reply` gains `Actions []Action` (empty = plain text, fully
backward-compatible) and `Notice string` (an ephemeral, actor-only
acknowledgement — a Telegram `answerCallbackQuery` toast, later a Slack/Discord
ephemeral reply). A transport renders actions as an inline keyboard where it can
(`CapButtons`) and degrades to an enumerated text list (`ActionsAsText`) where it
cannot — the graceful-degradation rule already stated for the transport boundary.

The authorizing/executing half — a minimum tier and an executor bound to each
`Verb`, held in an `ActionRegistry` — arrives with T3. A new admin capability is
then a registered verb, no transport change.

### A button is a UI hint, re-authorized at execution time

`callback_data` is never trusted beyond a token lookup. Every callback is
**re-authorized against the actor's ACL tier at execution time** (T3), composing
with the consent hard-gate (ADR 0002) and layered policy (ADR 0003 / 0007). The
click is the actor's *request* to run the verb, not permission to run it.

### Pending actions live server-side, keyed by a short token

`callback_data` is capped at 64 bytes, so the action itself is persisted
server-side (bbolt, `internal/pending`) keyed by a short random token; only the
token rides in the button. The store is transport-neutral (token → `core.Action`)
and shared: the transport mints a token when it renders a keyboard, and the
runtime resolves it when the click arrives.

The store yields two safety properties:

- **Expiry** — a stale button dies after a TTL (default 10m).
- **Single-use** — a resolved token never returns its action twice.

Single-use is implemented as a **tombstone** (a resolved record is marked
consumed, not deleted) rather than a delete. This is a *UX* choice, not a
security one: replay is rejected either way (delete → "not found"; tombstone →
"found + consumed"). The tombstone only buys a better toast — distinguishing
"already completed" from "expired". Resolution order makes that precise:

1. token not found → **expired**
2. found, consumed → **already completed** (even if the TTL has since elapsed —
   the actor already did it)
3. found, unconsumed, past TTL → **expired** (lazy GC)
4. found, unconsumed, within TTL → **resolve** (and tombstone)

Because the lazy path never re-touches a token nobody clicks again, the store
also runs a **periodic sweep** that drops every past-TTL record — unclicked
tokens and old tombstones alike — so the bucket stays bounded. The sweep is not
merely a method: the store **owns its sweeper lifecycle** — the durable store
starts it on open and stops it on close — so garbage collection is live the
moment the store exists, with nothing for the serve loop to remember to schedule.
The sweep interval is a deployment parameter, not a constant; callers pick it.

### The callback inbound carries its own authorization subject

An inbound click reuses the existing `Inbound` (its `User`, `Surface`, and
`ReplyTo` chat) and adds one field, `Callback`:

```
Callback { Token; QueryID; MessageID }
```

- `Token` → the pending-action lookup (the runtime resolves it).
- `QueryID` → answers the click within the platform's acknowledgement window
  (Telegram ~30s); it cannot be fetched afterward, so it must ride in the inbound.
- `MessageID` (with the chat in `ReplyTo`) → the handle to edit the message in
  place later.

The acting subject the runtime re-authorizes against — user id + surface — is the
inbound's own `User`/`Surface`, so T3's tier check is a clean insertion with no
fetch-back.

## Phasing

- **T1** — transport swapped to a maintained, full-coverage, dependency-free Bot
  API library at text parity (mechanical; de-risks the dependency).
- **T2** — actions on the wire: `Reply.Actions` + `Notice`, inline-keyboard
  rendering + `CapButtons` text fallback, the bbolt token store (expiry +
  single-use + sweep), and callback ingestion. A no-op **echo** action proves the
  send → click → resolve → respond loop end to end. **T2 owns the dead-token
  toasts** (expired vs already-completed); a clean resolve gets a generic "done".
- **T3** — the typed `ActionRegistry`, ACL-gated executors, re-authorization at
  execution, and the first real verbs. Security tests concentrate here:
  forged / replayed / expired / under-tier all rejected. T3 owns the
  post-execution toast and the keyboard → status-line edit.
- **T4 (later)** — suggested-action proposals rendered as approve / adjust
  buttons.

## Consequences

- The control plane is decoupled from any one platform: a new verb is a registry
  entry, a new platform is another adapter that renders actions its own way.
- The grounded-answer path (ADR 0001 / 0008) is untouched — a callback never
  reaches the engine.
- The security boundary is explicit and testable: trust stops at the token
  lookup, and authority is re-checked at execution against the tier that the
  inbound already carries.
- The store adds a periodic-sweep responsibility to the runtime; without it the
  pending bucket would grow unbounded.
