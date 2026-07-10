# ADR 0010: Proposed actions and informed consent

**Status:** Proposed (2026-07-10)

## Context

ADR 0009 built the interactive-action control plane: a user taps a button, the
click is re-authorized against the user's current tier, and the verb executes.
There, the human **authored the intent** — they chose the action; the button was
just convenience, and re-authorization (are they *allowed*?) is the whole
security story.

T4 adds a **proposer** (overseer): a component that *suggests* actions — "set
this user to trusted?" — surfaced as one-tap approve / dismiss buttons. That
shifts something ADR 0009 never had to model. A proposed action's **intent is the
proposer's, not the approver's**. Re-authorization still checks the approver is
allowed, but it does **not** check that the approver *understood or intended* this
specific effect. An admin reflexively tapping *approve* on a proposed
`set_tier(X, admin)` executes it with full authority even if the proposal was
wrong or adversarial. Authorization ≠ intent-verification.

## Decision

### Two axes: authority is unchanged; intent-authorship is new

- **Authority axis (unchanged from ADR 0009):** provenance confers no authority. A
  proposed action is a UI hint like any other; the boundary is re-authorization at
  click, against the approver's current tier. `approve` routes into the existing
  execute path verbatim — no new authorization surface.
- **Intent axis (new):** because the proposer, not the approver, authored the
  intent, the approver's **informed consent** is required for a privileged
  proposal — and it is *enforced*, not merely encouraged.

### The label is the consent surface, and it is registry-derived

The approve button's label is what the approver reads before tapping, so it is the
consent surface. It is **derived from the verb, not supplied by the proposer**: a
`Verb.Describe(params)` renders the concrete effect ("Set @target → trusted").
Because the label comes from the same registry entry (and the same params) that
`Execute` will use, it **cannot diverge from the effect that will run** — a
proposer cannot render "→ trusted" over a `set_tier(X, admin)`. The consent
surface is canonical, not an assertion by the proposer.

Fail-closed: a **privileged** verb (MinTier admin, per ADR 0009's rule that
privileged verbs sit at admin) with **no `Describe` cannot be proposed** — the
proposal is refused at the render boundary, never shown with a bare "Approve".

### Render order is a correctness guarantee

The render path runs **validate params → describe → mint token → render**.
`Describe` receives only *validated* params — the same data `Execute` will get —
so a malformed-param proposal is refused *before* any label is generated. There is
no describing an invalid action.

### Privileged proposals are DM-only

A privileged proposal is offered only in a direct message. A group has no single
recipient to tier-check, and offering a privileged approve button in a group would
leak an admin affordance to non-admins. Restricting privileged proposals to a
private surface makes the recipient check well-defined and matches the use case —
you would not change someone's tier via a public group button. The recipient's
authority is checked before offering (defense-in-depth; re-authorization at click
is still the boundary). Group-surface privileged proposals need a
"who-in-the-surface-can-approve" model and are deferred. Unprivileged proposals
may render anywhere.

### The proposer seam

`Proposal{Prompt, Action}` is a suggestion; `Proposer.Propose` emits them. The
runtime's `RenderProposal` turns a proposal into a reply offering
`approve` (the proposal's real action, effect-labelled) and `dismiss` (an
unprivileged verb that drops it and edits the message to say so). T4 ships one
worked proposer; event-driven overseers come later. `adjust` (re-offer with
altered params) is deferred to a follow-up — its keyboard-re-render and
superseded-token lifecycle deserve a focused pass and carry no new security
property.

## Consequences

- The control plane closes its loop: an overseer can propose, a human approves
  with one informed tap, and the T3 spine executes — without the proposer ever
  holding authority.
- Informed consent is structural, not a convention: the consent label is derived
  from the authoritative registry, and a privileged verb that can't describe
  itself simply can't be proposed. Future verb authors find the rule here and on
  the `Verb` type.
- The security boundary is unchanged — re-authorization at click (ADR 0009). The
  new machinery (Describe, DM-only, recipient check) lives entirely at the
  *offering* boundary and never in the auth path.
- A gap remains open by design: informed consent reduces rubber-stamping but does
  not verify that a proposal *should* happen. Whether proposals themselves need
  provenance/attestation (a trusted-proposer model) is left to a later decision if
  overseers ever propose beyond an operator's own instance.
