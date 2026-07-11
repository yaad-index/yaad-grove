# ADR 0017 — Embedding-based multilingual semantic retrieval

**Status:** Proposed
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
  - host-local → **`multilingual-e5-large`** via an Ollama endpoint.

The default embedder should be multilingual; both recommendations are.

## Vector store

**In-memory** for v1, period — brute-force cosine similarity over the embedded chunks, rebuilt at startup. Vaults are small, so startup re-embed is fine and there is no invalidation complexity. A persistent embedding store (SQLite, keyed by chunk content-hash to skip unchanged chunks) is named as **explicit future work**, not v1.

## Indexing and chunking

- One-time embed of all vault chunks at startup.
- The semantic retriever **reuses the existing vault chunking** — query-embedding and chunk-embedding must be over the same units, and the shared chunker keeps `[]Chunk`/Source identical.
- Reindex-on-change is future.

## Failure modes (two distinct)

- **Startup index build, endpoint unreachable → fatal.** Semantic was opted into but can't index — a deployment error, fail loud (same posture as `--persona-file`).
- **Query-time embedding failure (mid-session) → fall back to keyword for that query + `slog.Warn`.** Keyword is the resilience net; a transient endpoint blip degrades to language-blind retrieval, never total failure.

## Similarity threshold and top-k

- Reuse the current top-k (8).
- **`--similarity-threshold` is configurable with a conservative default** (not hardcoded). It is load-bearing: too tight → empty retrieval → the grounding short-circuit fires when it shouldn't; too loose → noise fed to the model. Conservative default, operator-tunable.

## Grounding guarantee — preserved (load-bearing)

Retrieval returns top-k chunks **above the similarity threshold**; if nothing clears it, the existing refuse-without-a-model-call short-circuit fires unchanged. The threshold makes "empty result" mean *genuinely nothing relevant*, not "keyword whiffed" — embeddings make the block fire only when there is truly no match. The block stays exactly as is.

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
