# ADR 0007: Consent gate — a silent-throttle decision and consent-before-rate ordering

**Status:** Accepted (2026-07-09)

## Context

ADR 0002 (consent is a hard gate) and ADR 0003 (two-surface access, layered
policy) set the access **policy**. Implementing the gate's decision function
(`Gate.Check`) surfaces two points the scaffold's `Decision` set and documented
check order do not resolve:

1. **No "reply nothing" outcome.** ADR 0002 requires that an unconsented user who
   keeps messaging is re-prompted only on a **throttle** — "keep showing the
   consent reminder (throttled so repeat messages do not spam)". So a repeat
   message *within* the throttle window must produce **no reply at all**: not
   another prompt, not an answer. The scaffold `Decision` set — serve /
   ask-consent / refuse / rate-limited — has no state for that.
2. **Ordering puts fairness before consent.** The scaffold documents
   surface-reach → **rate** → **consent** → serve. But consent is the hard gate:
   an unconsented user should reach the consent path, and — critically — must not
   be *counted*.

## Decision

### `DecideSilent` — a "reply nothing" decision

Add one `Decision` value: `DecideSilent`. The gate returns it for an unconsented
user whose consent prompt is within the throttle window — the reply is nothing.

The gate holds the throttle state (`Record.LastPromptedAt`), so the gate must own
the throttle **decision**; a distinct outcome keeps the transport a dumb renderer
of decisions (ADR 0001) — "no reply" is just another rendering. The alternative,
reusing `DecideAskConsent` and having each transport decide whether to actually
send, would scatter one throttle policy across every transport.

### Order: surface-reach → consent → rate-limit → serve

Consent gates **before** the rate limit. The decisive reason is not ordering
hygiene — it is the **privacy guarantee**: an unconsented user is governed
entirely by the consent path (prompt once, then `DecideSilent` until the window
elapses) and therefore **never touches the per-user rate counter**. Not
incrementing the counter for an unconsented user means we do not *count* them at
all — which extends ADR 0002's "record nothing the user said" from message
content to their activity metadata. The rate limit is fairness among **served
(consented)** users only; the unconsented are bounded by the consent-prompt
throttle instead.

Putting rate before consent would both give an over-limit unconsented user a
"you're rate-limited" reply (leaking that we count them) and grow their stored
state beyond the minimal row.

### Fail-closed, minimal state

A store error at any step returns `DecideRefuse` — never serve on an unknown
state. A first-seen user is a zero `Record` (`ConsentUnknown`), treated as no
consent, never an implied yes. The only state ever kept for an unconsented user
is the minimal `Record`: id, consent flag, and the prompt-throttle timestamp.

## Consequences

- The gate's returned `Decision` fully determines the reply, with no throttle
  logic leaking into transports; the `Decision` set grows by one (`DecideSilent`,
  rendered as no reply).
- The per-user rate counter only ever advances for consented users, so
  unconsented state stays truly minimal — the "record nothing" guarantee holds on
  the rate path, not just for content.
- An unconsented user's only possible outputs are one throttled consent ask and
  silence; they can be neither answered nor rate-limit-messaged.
- The ordering change is a deliberate departure from the scaffold's documented
  surface → rate → consent, recorded here as the gate's contract.
