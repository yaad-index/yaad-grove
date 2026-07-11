# ADR 0016 — Externalized grounding prompt template

**Status:** Accepted (2026-07-11)
**Amends:** ADR 0013 (per-instance persona / system-prompt construction), ADR 0014 (conversation injection); preserves the grounding contract of ADR 0008/0011.

## Context

The system prompt is assembled by a hardcoded `groundedSystemPrompt` in the engine: persona → scope → grounding contract (with the sentinel-first refusal instruction) → RECENT CONVERSATION → CONTEXT. An operator can shape the persona (ADR 0013, `PERSONA.md`) but not the surrounding scaffolding — the grounding wording, the refusal phrasing, the block framing. Tuning any of that needs a code change and a release.

Externalizing the prompt into an operator-editable template makes the whole scaffolding tunable per instance, the same way `PERSONA.md` made the persona tunable — without touching code.

## Decision

Extract `groundedSystemPrompt` into a Go `text/template` with placeholders `{{.Persona}}`, `{{.Scope}}`, `{{.History}}`, `{{.Context}}`, `{{.Query}}`. Embed the CURRENT prompt verbatim as the default template. Add `--prompt-template <path>` (mirroring `--persona-file`): absent → the embedded default; an explicitly-set unreadable path → startup-fatal.

`{{.History}}` and `{{.Context}}` are the already-rendered conversation and retrieval blocks (their internal formatting is unchanged), so the template assembles pre-built sections in order rather than re-implementing them. `{{.Query}}` is exposed in the template data for operator use, but the default template does not place it in the system message — the query remains the separate user turn, per the model contract.

## Invariant (the acceptance test)

With no `--prompt-template`, the engine renders the embedded default template to the **same output as the pre-refactor `groundedSystemPrompt`, byte-for-byte**, for the same inputs. Externalization is a pure refactor at the default — **zero behavioral change**. This is a stated invariant, not a test footnote.

The acceptance test golden-compares the default-template render against the pre-refactor prompt across fixtures: with/without persona, with/without history, with/without chunks, and the refusal path. If the golden test passes, the refactor is safe by construction.

## Preserved

- **Prompt order (ADR 0013):** persona → scope → grounding contract → RECENT CONVERSATION → CONTEXT → query.
- **The RefusalToken sentinel-first contract (ADR 0008/0013):** the default template keeps "decline: `%%OUT_OF_SCOPE%%` first, then a brief in-persona note." The engine's sentinel parsing is unchanged.
- **The grounding guarantee (ADR 0008/0011):** the default template's grounding instruction is unchanged. An operator template CAN weaken it — that is the operator's trusted responsibility (see Trust boundary) — but the default is safe, and the scope/refusal machinery around it is unchanged.

## Trust boundary

The prompt template is operator-authored **trusted configuration**, like `PERSONA.md` and the scope statement — loaded at startup, not user input, no sanitization concern. The trust boundary is the operator who deploys the bot.

## Loading and configuration

- `--prompt-template <path>`. No default path (the template is embedded in the binary). Absent → the embedded default (current behavior; zero change for existing deployments).
- **Parse at startup.** A specified-but-unreadable or unparseable template is fatal — a misconfigured prompt is a deployment error, not a degraded-mode condition. Missing placeholders that the engine requires are a load-time error, not a silent empty render.
- **Not hot-reloaded.** Restart to pick up changes.

## Consequences

- Operators tune the grounding scaffolding per instance without a code change or release.
- The default embedded template becomes the single source of the prompt; `groundedSystemPrompt` becomes its renderer.
- A malformed operator template fails startup loudly rather than degrading.

## Deferred

- Template validation/linting beyond parse + required-placeholder checks.
- Hot-reload.
- Per-surface templates.
