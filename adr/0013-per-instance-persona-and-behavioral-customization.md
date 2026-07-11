# ADR 0013 — Per-instance persona and behavioral customization (PERSONA.md)

**Status:** Accepted (2026-07-11)
**Amends:** ADR 0008 (system prompt construction)

## Context

The bot is perceived as hostile: it refuses greetings with a terse domain-decline, and meta-requests ("tldr", "what about X") are answered without character because there is no per-instance personality. The terse refusal is a prompt problem — the model composes the in-scope-but-declined response and the current system prompt gives it no guidance on tone.

There is no per-instance behavioral layer. Every deployment is identical in personality; operators have no lever to express who this bot is, what norms it upholds, or how it treats its community.

## Decision

Add an optional per-instance behavioral file — `PERSONA.md` by convention, path configurable via `--persona-file` — that is injected into the system prompt as a persona layer. The file is operator-authored plain Markdown, loaded at startup, and injected before the grounding instructions.

## Trust boundary

`PERSONA.md` is **operator-authored trusted configuration**, loaded at startup alongside scope and vault configuration. It is not user input and carries no sanitization concern — the trust boundary is the operator who deploys the bot, the same as for all other deployment config.

## What PERSONA.md governs

- **Name and personality.** Who the bot is, how it speaks. Friendly, terse, formal — the operator's call.
- **Social handling, bounded by community rules.** The bot makes small social gestures to put people at ease (greetings, acknowledgments) *within the limits set by the community rules*. If a community rule prohibits promotional content, the bot declines promotional messages in-character — friendliness does not override the rules. Social latitude is bounded, not freestanding.
- **Community rules.** What norms this specific community upholds. These are soft behavioral constraints the bot expresses in its answers and declines — distinct from the hard scope/retrieval boundary (vault config). The community rules bound the bot's social latitude.
- **Refusal tone.** How the model phrases an out-of-scope or in-scope-but-declined response — on the **answer path** where the model call already happens (the model parses the query to decline it, so persona-shaped wording is free, no extra call). The *decision* to decline stays deterministic (ADR 0008/0011); only the *wording* is persona-shaped.
- **DM answers.** Admin DM answers reach the engine and are fully persona-shaped — the persona layer applies in DM as well as group.

## What PERSONA.md does NOT govern

- **Transport-layer copy.** Static handler strings (consent nudge, rate-limit, at-capacity, callback toasts) fire without a model call and must be deterministic. These stay as config flags; i18n territory (#25).
- **Scope.** Cannot expand or contract retrieval scope. Scope is vault and MCP tool configuration.
- **Grounding.** Factual claims still require retrieval grounding (ADR 0008/0011). `PERSONA.md` cannot instruct the model to assert without a source. Social/meta responses carry no factual claims and do not require retrieval.
- **Knowledge.** Behavioral guidance only; facts go in the vault.
- **Consent or ACL.** Gate, rate limit, and ACL decisions are unaffected.

## Grounding guarantee — explicit preservation

System prompt ordering: persona section → scope section → grounding instruction → tool definitions. The grounding instruction appears after the persona section and explicitly overrides any persona guidance that would relax the factual-claims requirement. An operator cannot use `PERSONA.md` to bypass grounding.

## Illustrative example

```markdown
# Name
Grove

# Persona
Friendly and concise. You help members of the community learn and share.
Acknowledge greetings warmly and briefly.

# Community rules
This is a technical community. Stay on topic.
Don't recommend anything you haven't verified applies to this community's context.
Promotional or off-topic content is not welcome — decline it warmly but clearly.
```

This is non-normative; the file is free-form and the operator structures it as they see fit.

## Loading and configuration

- **Config flag:** `--persona-file <path>`. Default: `PERSONA.md` (resolved in the working directory, alongside `config.yaml`). A `PERSONA.md` present in the working directory is picked up automatically; the flag overrides with any name or path.
- **Default absent → graceful:** if the flag is at its default and `PERSONA.md` does not exist, the bot starts with no persona layer — current behavior, zero change for existing deployments.
- **Flag explicitly set but unreadable → startup-fatal:** if the operator explicitly points `--persona-file` at a path that is missing or unreadable, that is a deployment error and the bot refuses to start. Operator intent was to use a persona file.
- **File format:** Plain Markdown. No schema. Free-form behavioral guidance.
- **Not hot-reloaded.** Restart to pick up changes. Behavioral config, not operational data.

## Implementation notes (for the build ADR)

- New `PersonaFile string` field on `ServeCmd`, populated by `--persona-file`, defaulting to `PERSONA.md`.
- Loaded in `Run()`, passed to engine's system prompt construction. The load distinguishes the default-and-absent case (skip silently) from the explicitly-set-and-unreadable case (startup-fatal).
- `PERSONA.md` content is prepended as a clearly-delimited "persona" section before scope and grounding instructions.
- Absent or empty = no section added (backwards-compatible).

## Deferred

- Hot-reload.
- Validation/linting of `PERSONA.md` content.
- Interaction with ADR 0014 (conversation memory): complementary but independent.
