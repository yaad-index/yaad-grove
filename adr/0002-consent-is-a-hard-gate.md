# ADR 0002: Consent is a hard gate on the whole interaction

**Status:** Accepted (2026-07-09)

## Context
The engine logs community messages to feed a curation loop that enriches the
vault over time (ADR 0004). That logging needs consent. The question was what
consent gates, and what happens to a user who has not consented.

A weaker design — answer the user but do not log until they consent — was
considered and rejected. It still processes and could still incidentally retain
content from people who never opted in.

## Decision
Consent gates the **whole interaction**, per user, on a first-interaction basis:

- With **no consent**, the bot's only response is a consent prompt. It does not
  answer the user's actual query, and it records **nothing the user said** —
  not even ephemerally. Message content is never persisted without consent.
- The first message a user sends is answered only with the consent ask; it is
  not stored. Logging and answering both begin only **after** the user opts in.
- "Ignore" and "decline" collapse to the same state: keep showing the consent
  reminder (throttled so repeat messages do not spam), nothing else, until they
  say yes. Not-yet-answered is treated as no consent, never as an implied yes.

The only state kept for a non-consented user is a minimal ACL row — platform
user-id, consent flag, rate counter — so the bot can remember their state and
throttle the reminder. That is operational metadata, not content.

## Consequences
- The privacy promise is simple and total: no consent, no record, no answer.
- Declining does not make the bot misbehave; it just stays silent-but-for-the-
  ask, which is the honest behavior.
- The store schema splits cleanly into "content" (consent-gated) and "minimal
  ACL metadata" (always kept). See ADR 0003 for the ACL model.
