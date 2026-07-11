# ADR 0012: Explicit DM consent, surface-split answering, and directed-vs-ambient group handling (v1)

**Status:** Accepted (2026-07-11)

**Amends:** ADR 0002 (consent is a hard gate), ADR 0003 (two-surface access), ADR 0007 (consent gate ordering)

## Context

The first live deployment surfaced that the original consent model is not just buggy but conceptually wrong. ADR 0002/0007 made consent a hard gate and specified "keep chatting = opt in," inferred from a user's messages. Problems:

1. **The opt-in transition was never implemented** — the gate prompted once then went silent forever; nothing set `ConsentGranted`, so it was impassable.
2. **"Keep chatting = opt in" is wrong in a group.** A user's group messages are addressed to the community, not to the bot; there is nothing to infer bot-consent from.

This ADR replaces the inference model with explicit consent, splits answering by surface, and distinguishes bot-directed messages from ambient community chatter. A key consequence of only ever responding to *directed* messages is that an opt-in instruction cannot flood the group.

## Decision (v1)

### Surfaces

**Group (open to all members).** The bot handles a group message by whether it is *directed at the bot* (a reply to one of the bot's messages, or an @mention) or *ambient* (anything else):

- **Directed + consented** → the bot answers from the knowledge base.
- **Directed + not consented** → the bot delivers a **configurable consent nudge**: either a short **text reply** with the opt-in instruction (the default) or an **emoji reaction**, chosen per instance, with the message text / emoji configurable and a sensible default. Either way it does not answer, and it cannot flood — only directed messages ever draw any response; ambient chatter never does.
- **Ambient + consented** → the message is **silently logged** to the growth corpus (ADR 0004), with no reply.
- **Ambient + not consented** → ignored: no log, no reply.

**DM (admin-only for answers).**

- **Admin** → full answers from the bot/model.
- **Non-admin** → the DM is only for managing consent (see below); it never answers a query.

### Consent

- **Opt-in is explicit and private.** A user opts in by tapping an inline **opt-in button** in a DM, or with the `/consent` text command. Consent is never inferred from group messages.
- **Entry from the group:** since a bot cannot DM a user who has not started it, the opt-in instruction points the user to the bot's DM; opening it sends `/start`. **The bot must handle `/start` by presenting the opt-in button** (otherwise the DM opens blank).
- **Opt-out:** `/consent remove`. A user may remove their own at any time (withdrawal is always available). An admin may also remove any user's consent (moderation override); the admin power is additive and never removes the user's own ability.
- **Consent is the gate** for both being answered on a directed group message and having ambient chatter logged.

### Admins

- Admins are configured as a list of platform user ids (`admins: [...]`). A user is an admin iff their id is in the list. There is **no separate "allowed/elevated user" tier** in v1.

## Consequences

- **Supersedes** ADR 0002's in-group "keep prompting / continuing opts in" flow. Consent moves to the DM (button + commands); in the group, an unconsented user is answered only with the opt-in instruction, and only when they direct a message at the bot.
- **ADR 0003 (two-surface):** DM answering is admin-only; the DM also carries the consent UI for everyone. Group answering is open to consented members, but only on directed messages.
- **New behavior to build:**
  - **Directed-vs-ambient detection** in the transport: is a group message a reply-to-bot or an @mention (directed), or ambient?
  - **DM consent UI:** opt-in button, a `/start` handler that presents it, `/consent`, and `/consent remove` (self + admin-target).
  - **Admin config** + admin-gated DM answering.
  - **Gate/decision changes:** answering requires consent (group, directed) or admin (DM); consent is an explicit grant/revoke flag; ambient consented messages are logged without a reply; unconsented directed messages get the opt-in instruction.
- **Growth corpus (ADR 0004):** fed by consented ambient group messages (group-only already shipped in #38).
- **v1 scope boundary:** DM answering is admins-only; group answering is any consented member on a directed message. Broader answering is a future ADR.

## Implementation notes (not part of the decision)

- Suggested unit split: (a) directed-vs-ambient detection in the Telegram transport; (b) DM consent UI (opt-in button + `/start` handler + `/consent`) and callback wiring; (c) `/consent remove` (self + admin); (d) admin config + admin-gated DM answering + the group directed/ambient/consent decision logic in `acl`/`runtime`.
- **Unconsented-directed nudge is configurable:** `mode` (message | reaction; default message), the message `text` (default: a short opt-in instruction), and the reaction `emoji` (default 🤝). This fits the per-instance / localized strings planned in issue #25.
- Backlog-skip (#37) and group-only logging (#38) are already merged and independent.
