# ADR 0025: A withdrawal is a decline, and a decline draws no nudge

**Status:** Proposed (2026-10-06)

**Amends:** ADR 0002 ("ignore" and "decline" collapse to the same state) and ADR 0012 (the "Directed + not consented" row of the group decisions), for a user who withdrew.

## Context

ADR 0002 treated a user who never answered the consent ask and a user who said no as one state: keep showing the reminder until they say yes. ADR 0012 moved consent to the DM and gave every user `/consent remove` to withdraw. Withdrawing sets the user back to unconsented, the same state as a user who was never asked. The store has a "declined" value, but nothing sets it.

So a user who withdrew is nudged again whenever they direct a message at the bot in a group, at most once per cooldown (ADR 0024), as if they had never been asked. They said no explicitly, and the bot keeps asking (#6).

## Decision

- **A withdrawal records a decline.** `/consent remove` sets the user's consent to declined instead of unknown. Everything else it does is unchanged: the buffered turns are purged (ADR 0014) and long-term memory is erased (ADR 0023).
- **A declined user draws no nudge.** In a group, every message from a declined user, directed or ambient, gets the gate's "reply nothing" decision. As for any unconsented user, it is not answered and not logged, and nothing they said is recorded (ADR 0002). The nudge cooldown (ADR 0024) does not apply, since there is no nudge to space out.
- **The way back is the DM, and it stays open.** The gate decides group messages only; the DM is the consent flow, and a decline does not change it. A DM from a declined user shows the disclosure and the opt-in button, as for anyone not opted in, and `/consent` or the button grants consent. The reply to a withdrawal that went through already says how to opt back in.
- **A user who was never asked is unchanged:** a directed message draws a nudge, once per cooldown.
- **No admin reset.** Returning a declined user to unknown would only bring the nudges back to someone who said no; the user's own way back is always open. ADR 0012's admin removal of another user's consent is not built and is not part of this decision.
- **No decline before a first opt-in.** The DM offers only the opt-in. A user who has never opted in is nudged at most once per cooldown and can ignore it; withdrawal is the one explicit no.

## Consequences

- A user's no holds in the group until they reverse it themselves.
- A declined user who directs a message at the bot gets no reply at all, and may read the bot as broken. They withdrew themselves, and the reply to the withdrawal told them how to come back.
- Users who withdrew before this change are recorded as unknown, the same as a user never asked, so they are still nudged. Nothing can tell the two apart, so nothing is migrated. Sending `/consent remove` again records the decline.
- The stored row does not grow. A user who withdrew already has a row; its consent flag now says declined instead of unknown. The flag is the consent state ADR 0002 already keeps, not anything the user said.
- `memory erase --unconsented` still erases declined users, since it covers every user whose consent is not granted.
