# ADR 0024: A cooldown on the consent nudge

**Status:** Proposed (2026-10-05)

**Supersedes:** the "Directed + not consented" row of ADR 0012's group decisions, in part: a nudge is no longer drawn by every directed message.

## Context

ADR 0012 has the bot nudge an unconsented user each time they direct a message at it in a group (a reply to the bot or a mention), and says the nudge "cannot flood" because only directed messages draw one. That holds for ambient chatter, but not for one user who keeps directing messages at the bot. Each of those messages draws a nudge, and in message mode each nudge is a message in the group. Before ADR 0012, ADR 0007's prompt throttle bounded this: a repeat inside the window drew nothing. ADR 0012 removed that throttle along with the in-group consent flow it belonged to (#47).

## Decision

- **One nudge per user per window.** After nudging a user, the gate stays silent for that user's directed messages until a cooldown has passed, then nudges again on the next one. The gate returns its existing "reply nothing" decision for a message inside the window, as it does for ambient chatter.
- **Per user, across chats.** The window belongs to the user, not to a group: a nudge in one group starts the window everywhere. The nudge says the same thing wherever it is shown.
- **Both nudge modes.** The cooldown applies to a reaction as to a message. One rule covers both, and repeated reactions are noise too.
- **Held in memory, in the gate.** The gate keeps the time of each user's last nudge in memory and writes nothing to its store, so nudging an unconsented user persists nothing about them, as today. A restart forgets the windows, which costs at most one extra nudge per user. An entry is dropped once its window has passed, so the map holds only users nudged within one window.
- **Ten minutes by default, set by the operator.** A flag sets the window; zero turns the cooldown off, which is ADR 0012's behaviour.
- Nothing else changes: a consented user is never affected, ambient chatter still draws nothing, and nothing the user said is recorded (ADR 0002).

## Consequences

- One user can draw at most one nudge per window, however many messages they direct at the bot. Message mode can no longer be used to make the bot noisy in a group.
- A user who misses the nudge, or asks again inside the window, gets no reply until it passes. The opt-in instruction is still reachable by the DM flow at any time (ADR 0012).
- The windows are per process. An instance runs one process: its access-control store is a single file that one process holds open.
- The windows are not durable: after a restart, the next directed message from a user nudges them even if they were nudged just before.
