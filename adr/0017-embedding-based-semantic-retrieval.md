# ADR 0017 — Embedding-based multilingual semantic retrieval

**Status:** Accepted (2026-07-11)
**Amends:** ADR 0001 (Retriever interface), ADR 0008/0011 (grounding). Relates to ADR 0006 (spend ceiling).

## Context

Keyword retrieval is language-blind. An on-topic Persian question ("what does yaad-grove do?") retrieved 0 chunks and refused, though the answer is in the (English) vault. The root cause is **recall**, not the grounding block — the block correctly refuses when retrieval is empty; the bug is that keyword search whiffed on a cross-language match. Embeddings fix recall generally; a **multilingual** embedder fixes cross-language (the Persian query embeds near the English chunk) even from a single-language KB.

## Decision

Add a **semantic Retriever** doing vector-similarity search behind the existing `core.Retriever` interface. Keyword search stays as the zero-dependency fallback. The contract (`Retrieve(ctx, query) ([]Chunk, error)`, each Chunk carrying its Source) is unchanged, so the engine is unaffected.

## Retriever modes (implicit detection — no `--retriever` flag)

The mode is inferred from config, eliminating a redundant flag and the "set the model but forgot the flag" footgun:

- **No embedding configured → pure keyword.** Today's behavior, the **zero-config default**, unchanged.
- **Embedding configured → embedding primary, keyword as the per-query runtime fallback.**

So there are exactly two modes: keyword-only (default), or embedding-with-keyword-fallback. Semantic is opt-in by setting the embedding endpoint.

## Embedder interface (testable by construction)

The embedding client sits behind a one-method interface, so tests inject a deterministic fake — no live `/embeddings` in CI:

```go
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

It is batch by design: the retriever embeds all vault chunks at index time (one or few calls) and the single query at serve time. The engine/retriever depend only on this interface; the concrete OpenAI-compatible client lives in its own package, like the chat Model, and CI uses an injected fake.

## Embedding endpoint and config

An OpenAI-compatible `/embeddings` endpoint, mirroring the chat-model config — `--embedding-base-url` + `--embedding-model` set together as a pair (the operator picks the provider; grove does not bake a provider-locking default, same as the chat model).

- **Key:** `YAADGROVE_EMBEDDING_API_KEY`, **falling back to `YAADGROVE_MODEL_API_KEY`** when unset. Same-provider (chat + embeddings) needs zero extra config; split-provider (e.g. local Ollama chat + hosted embeddings) is supported.
- **`--embedding-base-url` and `--embedding-model` are set together as a pair.** Both present → semantic mode (the opt-in). Both absent → keyword (default). An **incomplete pair** (one without the other) → **startup-fatal** ("both required together"), the same posture as the chat-model pair. There is **no default embedding model and none is needed** — you are in semantic mode precisely because you named the model, so there is never a blank-field-blocks-startup surprise.
- **Recommended pairs (documented, copy-paste starting points — not defaults):**
  - OpenAI-compatible endpoint → **`text-embedding-3-large`** (strong multilingual, same key path).
  - host-local → **`bge-m3`** via an Ollama `/v1` endpoint — multilingual (100+ languages incl. Persian), 1024-dim, and the embedder used for the empirical calibration below (validated end-to-end against the real failing query). A local Ollama endpoint needs no auth, so the key env can be any placeholder (or the model-key fallback). Note: `nomic-embed-text` is English-only — unsuitable for the multilingual goal.

The recommended embedder should be multilingual; both above are.

## Vector store

**In-memory** for v1, period — brute-force cosine similarity over the embedded chunks, rebuilt at startup. Vaults are small, so startup re-embed is fine and there is no invalidation complexity. A persistent embedding store (SQLite, keyed by chunk content-hash to skip unchanged chunks) is named as **explicit future work**, not v1.

## Indexing and chunking

- One-time embed of all vault chunks at startup.
- The semantic retriever **reuses the existing vault chunking** — query-embedding and chunk-embedding must be over the same units, and the shared chunker keeps `[]Chunk`/Source identical.
- Reindex-on-change is future.

## Failure modes (two distinct)

- **Startup index build, endpoint unreachable → fatal.** Semantic was opted into but can't index — a deployment error, fail loud (same posture as `--persona-file`).
- **Query-time embedding failure (mid-session) → fall back to keyword for that query + `slog.Warn`.** Keyword is the resilience net; a transient endpoint blip degrades to language-blind retrieval, never total failure.

## Ranking and the threshold — the block-early / brain-judge choice IS `--similarity-threshold`

Top-k is reused (k=8). The similarity **threshold is not merely a recall knob — it is what lets the semantic retriever ever return "empty," and thus what keeps the pre-model grounding block alive.** A top-k retriever with *no* floor never returns empty for a non-empty vault: it always hands back k chunks, so the refuse-**without**-a-model-call short-circuit (the load-bearing block of ADR 0008/0011, the "dumb bot blocks before the brain" principle) does not fire — every off-topic query reaches the model and grounding falls to the model's scope-refusal (`%%OUT_OF_SCOPE%%`). This is the "block early vs. let the brain judge" choice, and rather than hard-code a philosophy, grove exposes it as **`--similarity-threshold`**:

- **A floor set (default `0.30`) → block-early.** A best match below the floor → empty → the hard block fires *before* the model. This is the default: it preserves the pre-model block, and the empirical numbers below make over-refusal a non-issue.
- **`--similarity-threshold=0` → brain-judges.** No floor; every query reaches the model and grounding is the model's scope-refusal only. Costs a model call per off-topic query and a weaker, more injection-exposed block — but it is the operator's choice, not grove's.

The operator picks the philosophy via one knob; grove ships block-early (0.30) out of the box.

### Empirical calibration (decision-grade)

Measured with `bge-m3` (a multilingual embedder) on the real Persian query vs. real vault content, cosine similarity:

| pair | cosine |
|---|---|
| Persian query ↔ on-topic English doc | **0.43** |
| English query ↔ on-topic English doc | 0.70 |
| Persian query ↔ unrelated decoy | **0.23** |
| Persian query ↔ English query (same meaning) | 0.54 |

Cross-language valid matches (~0.43) sit well below same-language (~0.70) but clearly above noise (~0.23). A floor tuned for same-language (0.5+) would re-introduce over-refusal for the exact Persian case this ADR fixes. **A ~0.30 floor is the safe value** — above noise, below cross-language valid — which is why the default is 0.30, not 0.5.

## Grounding guarantee — preserved (mechanism depends on the threshold setting)

The guarantee holds either way; the *mechanism* is what the threshold selects:

- **Floor set (the default, 0.30):** retrieval returns top-k chunks above the floor; nothing above the floor → empty → the existing refuse-without-a-model-call short-circuit fires unchanged, so the block is genuinely pre-model.
- **`--similarity-threshold=0`:** semantic retrieval is never empty for a non-empty vault, so the pre-model short-circuit is effectively bypassed and the model's in-prompt scope-refusal (ADR 0008 sentinel) carries the whole guarantee.

Either way a query embed failure that falls back to keyword can still yield empty → refuse. The default (0.30) keeps the pre-model block load-bearing.

## Budget (ADR 0006)

- **Startup bulk-index embed is exempt** from the per-user serving ceiling — a one-time boot operation. It is **logged prominently** (the startup embedding token count is surfaced in the boot line) so its cost is visible.
- **Per-query embeddings are metered** against the spend ceiling (serving cost).
- **Note for operators:** embedding calls are ~1–2 orders of magnitude cheaper than completions, but they *do* count. An operator who set the ceiling only for completion costs should re-check it — the meter now also consumes (a small amount) per query for the query embedding.

### Crash-loop caveat (the trigger for the embedding cache)

The startup exemption plus **no embedding cache** has a sharp edge: under `restart: unless-stopped`, a **crash-loop re-embeds the entire vault on every boot** — an unbounded cost that is *exempt from the very ceiling meant to bound runaway spend*. For the test bot (17 tiny docs) this is pennies, so deferring the cache is fine there. But this makes the deferral a **conscious** one, not a hidden footgun: the content-hash embedding cache (see Deferred) is **required before any large-vault or production deployment**, not an optional fast-follow. The prominent startup token-count log is the tripwire that makes a runaway visible.

## Multi-PR plan

Not a one-shot: (1) this ADR → (2) the `Embedder` client (`/embeddings`) behind the interface → (3) the semantic retriever + in-memory index + cosine/threshold → (4) config wiring + startup indexing + keyword fallback → (5) tests throughout (fake embedder, similarity/threshold, fallback).

## Deferred

Persistent embedding store (SQLite, content-hash keyed); reindex-on-change; hybrid keyword+semantic ranking; chunking-strategy tuning; per-language routing.
