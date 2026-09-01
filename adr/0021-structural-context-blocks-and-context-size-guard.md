# ADR 0021 — Structural context blocks and a hard context-size guard

**Status:** Proposed (2026-09-01)
**Amends:** ADR 0016 (externalized grounding prompt template — the CONTEXT block rendering); preserves the grounding contract of ADR 0008/0011 and the injection ordering of ADR 0013.
**Issue:** #166

## Context

Two independent problems in how the CONTEXT section is built.

**1. Citation-shaped markers invite echoing.** `contextBlock` (`internal/core/prompt.go`) renders each retrieved chunk as prose preceded by a literal `\n[<source>]\n` marker. The grounding prompt then instructs the model to use those `[source]` tags but never surface them. This is a *negative* instruction against a marker that looks exactly like a citation the model is used to producing — its correctness depends entirely on the model obeying it. `deepseek-chat` obeyed. `gemini-2.5-flash` does not reliably: it leaks `[source]` into replies (observed 2026-09-01, e.g. `...episode 42... [source] host...` (a Persian reply)). The marker is prose-shaped, so the model treats it as something to reproduce.

**2. No absolute bound on assembled context.** Retrieval selects chunks by similarity threshold and an inject count, but nothing caps the *total size* of the rendered CONTEXT. A set of large chunks can grow the system prompt without limit, which is both a cost and a reliability risk (latency, truncation by the provider, degraded grounding).

## Decision

**A. Render context as structurally-delimited blocks, not prose with citation markers.**

Replace the `\n[<source>]\n` prefix with an unmistakably non-prose wrapper per chunk, e.g.:

```
<doc id="games/beyond-the-sun#setup">
…chunk text…
</doc>
```

The `id` carries the same internal grounding reference the `[source]` marker did (still never surfaced), but it now lives in an attribute inside a structural tag rather than as a bracketed token in the prose stream. Nothing in the injected material is shaped like a citation the model would emit, so there is nothing to parrot.

Because the marker is no longer citation-shaped, the grounding prompt's "use the `[source]` tags but do not surface them" clause is **no longer load-bearing** and is reduced to a short, positive framing ("facts come only from the documents below"). The anti-echo instruction is not the guard; the format is.

This mirrors how a well-built agent harness feeds tool results and retrieved documents: as typed, clearly-delimited blocks the model reads as data, not as prose it authored.

**B. A hard, configurable context-size guard.**

Add a configurable maximum on the assembled CONTEXT, enforced deterministically at render time.

- **Unit: tokens**, not characters. It matches the existing spend meter and the real model limit. (An approximate tokenizer is acceptable for the guard; the guard is a safety bound, not billing.)
- **Overflow behaviour: drop lowest-fused-score chunks first**, never truncate mid-chunk. Retrieval already ranks chunks by fused score (`internal/retrieval/planner.go`); the guard drops from the bottom until the assembled block fits. A half-a-document chunk is worse than one fewer document.
- **Observability: the drop is logged**, never silent — a `slog` line naming how many chunks and roughly how many tokens were dropped, alongside the existing `grounding sources` trace. A silent truncation reads as "the model had everything" when it did not.
- **Default: a conservative token ceiling** that is safe out of the box (in the spirit of `spend-ceiling`'s conservative default), overridable per deploy via a new knob next to `--similarity-threshold` / `--memory-inject`.

## Invariant (acceptance test)

1. **No citation string reaches a reply.** For both `deepseek-chat` and `gemini-2.5-flash`, no `[source]` / `[منبع]` / bracket-citation token appears in output, **without any output-scrubbing step** — the format alone prevents it. Golden/e2e test asserts absence across the leak fixtures.
2. **Context is provably bounded.** A synthetic oversized retrieval set renders a CONTEXT block whose token count is ≤ the configured cap; the dropped chunks are the lowest-scored ones; the drop is logged.
3. **Grounding is preserved.** The structural rewrite does not weaken the grounding guarantee (ADR 0008/0011): the same facts are answerable from the same retrieved set, minus anything the size guard deliberately dropped.

## Preserved

- **Injection order (ADR 0013):** persona → scope → grounding contract → RECENT CONVERSATION → CONTEXT → query. Unchanged.
- **Internal-only sources (ADR 0016):** the `id` is still an internal reference, never surfaced.
- **Externalized template (ADR 0016):** `{{.Context}}` remains the pre-rendered block; only its internal formatting changes. The default template's byte-for-byte invariant is **intentionally broken here** for the CONTEXT block only, and this ADR is the record of that deliberate change — existing operator templates that place `{{.Context}}` keep working (they receive the new block); templates that hardcoded the old `[source]` framing in their own prose do not exist by construction, since that framing lived in the engine, not the template.

## Consequences

- The leak is fixed at the cause; the anti-echo instruction stops being a correctness dependency.
- Context cost/latency gains a hard ceiling with a predictable, logged degradation mode.
- One new config knob; one changed block renderer; the grounding contract and refusal machinery are untouched.

## Deferred

- Exact tokenizer choice for the guard (provider-accurate vs approximate) — an approximate count is sufficient for a safety bound; revisit only if the cap needs to be tight against a hard model limit.
- Per-tier or per-dimension size caps — single global cap first.

## Open for maintainer

- **The token ceiling default.** Proposed conservative; the real number wants the maintainer's call against the deploy's model context window and cost target.
