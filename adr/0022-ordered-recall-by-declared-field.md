# ADR 0022 — Ordered recall by a declared field (recency / ordinal / count)

**Status:** Proposed (2026-09-01)
**Builds on:** ADR 0020 (structured faceted recall — `kb_enumerate` / `kb_dimensions`, the complete-set primitives). Preserves the grounding contract of ADR 0008/0011.

## Context

Similarity retrieval answers *"what is relevant"*, never *"what is newest / the most / how many"*. When a user asks for the latest item in a collection, the model reads the highest-ordered value that happens to appear in the similarity sample, which can be lower than the true maximum: the sample is a partial view that silently omits the very documents that decide the answer. A recency / ordinal / count question is a **whole-collection** question.

ADR 0020 already gives the whole-collection primitive for *facet-match* questions: `kb_enumerate(dimension, value)` returns the complete matching set, and the prompt forces its use for "which / list all / how many X match" phrasings. What is missing is **ordering**: enumerate filters, it does not sort, and a document's ordinal frontmatter (a sequence number, a date) is not indexed as an orderable field. So "the latest one", "the most recent time X came up", "which is the newest" have no correct path and fall through to similarity.

## Decision

Three parts, each small and building on ADR 0020.

**1. Declare orderable fields.** Alongside `store-dimensions` (faceted values), add a config list of **orderable fields** — frontmatter keys with a total order: numeric or date. The store indexes each declared field's value per document. A field may be both a dimension and orderable; most orderables are ordinals, not vocabularies. The list is operator-supplied and may be empty (a deployment with no ordinal fields simply has no ordered recall).

**2. An ordered-recall primitive + tool.** Add `Store.Ordered(ctx, field, direction, limit) []DocRef` — the declared field's documents sorted asc/desc, optionally capped. Expose it as a tool, either `kb_ordered` or a `sort`+`limit` extension of `kb_enumerate` so ordering composes with facet filters (e.g. "the latest document that also matches facet X"). "How many" needs no new primitive: it is `len(enumerate(...))`, already available — the gap there is only routing (below).

**3. Route recency / ordinal / count questions to it.** Widen the prompt's structured-tool trigger (which today forces `kb_enumerate` for "which / list all") to also cover **"latest / most recent / newest / oldest / first / last / highest-numbered / how many / which is the Nth"**. Similarity stays the default for "tell me about X"; the ordered/enumerate path is mandatory for order- and count-bearing questions, because those are unanswerable from a sample by construction.

## Invariant (acceptance)

- "What is the latest item" returns the true maximum of the declared field over the whole indexed set, never the maximum within a similarity sample.
- Ordered recall composes with facets: "the most recent item matching facet X" sorts the enumerated match set, not the global one.
- A recency / count question never answers from the similarity CONTEXT alone (mirrors ADR 0020's completeness invariant); a test asserts the structured tool is invoked for these phrasings.
- Grounding (ADR 0008/0011) is preserved: ordered recall returns real indexed documents and the answer is still grounded in them.

## Consequences

- Recency / ordinal / count questions become correct instead of silently sampling.
- One new store method + one tool (or a `sort`/`limit` extension of the existing one) + a prompt-trigger widening. The field values are already in frontmatter, so indexing is an ingest-time capture, not new source data.
- The operator decides which fields are orderable, so a deployment without a declared field has no ordered recall — no default, no surprise (spirit of ADR 0021 Part B's required-knob discipline, though here an empty list is a valid "none").

## Open for maintainer

- **A dedicated `kb_ordered` tool, or `sort`+`limit` args on `kb_enumerate`.** Lean: extend `kb_enumerate`, so ordering and facet-filtering compose in one call and the model keeps a single structured-recall tool rather than two.
- **The direction default and whether a bare "latest" implies `limit 1`.** Lean: descending + `limit 1` for "the latest / the newest"; no limit for "list them newest-first".

## Deferred

- Range / bound queries (documents whose field is after a given value) — a natural follow-on once fields are ordered, not needed for recency/max.
- Aggregations beyond count (sum / avg) — no current question needs them.
