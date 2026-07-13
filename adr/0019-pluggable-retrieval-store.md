# ADR 0019 — Pluggable retrieval store (persistent index + structured/graph lookup)

**Status:** Proposed (2026-07-13)
**Relates to:** ADR 0017 (embedding-based semantic retrieval), ADR 0011 (tool-call loop + grounding boundary), #65 (hybrid retrieval / pluggable store — this is Increment B and more)

## Context

Two problems with retrieval today, one operational and one fundamental.

**1. The index is fully in-memory and rebuilt on every boot.** `internal/retrieval/semantic.go` reads, chunks, and *re-embeds the entire vault into RAM at construction* on every start (the "N chunks indexed" startup line). There is no persistence. That is fine at 1.8k chunks; it does not scale, it makes restarts slow, and it ties us to one implicit "store."

**2. A whole class of queries is structurally unanswerable by top-k semantic RAG.** The dominant real query on the podcast bot is *"which episodes cover game X?"* — and it under-recalls (observed live: Irish Gauge is reviewed in ep47 *and* ep73, the note says so, yet the bot answered ep73 only). This is not a content gap; the data is correct. It is the **wrong retrieval primitive**: "enumerate every document whose `games` list contains X" is a *filter/traversal over structured metadata*, but semantic search returns the few most-similar chunks — never the complete set. The same shape recurs everywhere and generalizes past boardgames:

- podcast: "which episodes discuss game X", "what did host Z say about W", "which games did designer Y make"
- a Berlin city-guide instance: "which restaurants are in Kreuzberg", "which museums are free"

They are all **relationship traversals / attribute filters** over data the KB already carries in frontmatter (`games:`, `hosts:`, `designer:`, or `neighborhood:`, `category:`). Semantic-chunk RAG cannot do them reliably; a structured index (or a graph) answers them exactly and completely.

**The mechanism, confirmed live (2026-07-13).** The chunker already splits on markdown headings, so each game's section is its own chunk — that is not the problem. The problem is the **top-k cap**: a retrieval returns only a handful of chunks (≈8 by default). For "which episodes cover game X", the relevant chunks are spread across *several* documents (ep47, ep66, ep73, + the game's own note) and they compete for those few slots — the highest-ranked win, the rest are **cut before they ever reach the model**. The bot then, grounded honestly to the context it *did* get, **denies the game is in ep47/ep66 even when explicitly told to look there** — because that text is genuinely not in its context. No cap value fixes this: "the k most similar chunks" is structurally not "every document matching X." This is the concrete failure `Enumerate` removes.

**Operator constraint:** no storage lock-in. A small instance (the podcast) wants an *embedded* store (no separate server); a larger instance (Berlin) may want Postgres/pgvector or something else. The engine must not be welded to any one backend — if a chosen backend (e.g. a cgo one) becomes a liability, we swap it via config, not a rewrite.

## Decision

Introduce a **pluggable `Store` port**: the engine depends only on an interface; concrete backends are **selected by config**. This extends an existing seam — `core.Retriever` is already an interface — rather than fighting the architecture. The `Store` contract covers all three retrieval modes **plus structured enumerate/traverse**, so the failing query class is answered by construction, generically, for any instance's declared frontmatter fields.

Backends ship as adapters; **cgo backends are isolated behind Go build tags** so the default build stays a pure-static (`CGO_ENABLED=0`) distroless binary and no instance pays for a backend it does not use.

### The `Store` interface (sketch, for review)

```go
// Store is the retrieval backend port. The engine holds only this; backends are
// config-selected adapters. All methods are read-mostly after Index().
type Store interface {
    // Index (re)builds the store from the vault: chunk text + embeddings +
    // declared structured dimensions. Persistent backends skip re-embedding
    // unchanged content; the memory backend rebuilds in full.
    Index(ctx context.Context, docs []Doc) error

    // Semantic returns the top-k chunks by vector similarity.
    Semantic(ctx context.Context, queryEmbedding []float32, k int) ([]Chunk, error)

    // Keyword returns the top-k chunks by lexical/full-text match.
    Keyword(ctx context.Context, query string, k int) ([]Chunk, error)

    // Enumerate returns EVERY document matching a structured predicate over a
    // declared dimension — the complete authoritative set, not top-k. This is
    // the primitive that fixes "which episodes cover game X".
    //   Enumerate(ctx, "games", "Irish Gauge") -> [ep47, ep73]
    // Matching is over a NORMALIZED key (see normalization contract below), so
    // casing/formatting drift in frontmatter can't silently drop docs.
    Enumerate(ctx context.Context, dimension, value string) ([]DocRef, error)

    Close() error
}

// Doc is a source note: its chunks + the frontmatter dimensions the instance
// declared queryable (e.g. games, hosts, designer | neighborhood, category).
type Doc struct {
    Ref        DocRef
    Chunks     []Chunk
    Dimensions map[string][]string // field -> values, from frontmatter
}
```

Graph-capable backends (LadybugDB) implement `Enumerate` as a one-hop edge traversal; SQL backends as a `WHERE` over an indexed column; the memory backend as a map lookup. Same contract, native implementation.

### Backends (adapters)

| Backend    | Kind                 | Modes                              | cgo | Use |
|------------|----------------------|------------------------------------|-----|-----|
| `memory`   | in-process, volatile | semantic + keyword + enumerate(map)| no  | **default**, zero-config, current behavior |
| `sqlite`   | embedded, single-file| sqlite-vec + FTS5 + SQL enumerate  | yes | small persistent instance |
| `ladybug`  | embedded graph       | vector index + FTS + Cypher traverse | yes | **flagship embedded** — models the relationships the queries are about |
| `postgres` | client-server        | pgvector + FTS + SQL enumerate     | no (pgx) | scale / Berlin |

**Why LadybugDB is a strong default-embedded choice:** the KB *is* a graph — episodes↔games↔hosts↔designers↔publishers are edges — and every failing query is a traversal over those edges. Ladybug (ex-Kuzu, MIT, embedded/in-process, Go bindings, vector + FTS + Cypher) models that natively while staying serverless. It is young (v0.18), which is exactly why it lives behind the interface.

### cgo isolation (the no-lock-in guarantee)

- Engine core + `memory` + `postgres` backends are **pure Go**; the default build is `CGO_ENABLED=0`, static, distroless — unchanged from today.
- `sqlite` and `ladybug` adapters compile **only under their build tag** (`-tags sqlite` / `-tags ladybug`); an image that wants them opts in and accepts the C toolchain + dynamic libs. Nothing else in the tree references them.
- Swapping backends is a **config change + a reindex**, never an engine change. If cgo/Ladybug ever becomes a problem, drop to `postgres` (pure-Go) or `memory` with one config line.

### Query routing — how the model reaches Enumerate

Expose a **structured-lookup tool** the model can call, alongside the existing grounding retrieval (fits ADR 0011's tool loop): e.g. `kb_enumerate(dimension, value)`. The model calls it for "which/what/list … " questions and gets the complete set; free-text questions still use semantic+keyword grounding. Generic — the instance declares its dimensions; the tool description is generated from them. (Alternative considered: engine-side intent detection. Rejected as less general and more brittle than letting the model choose the tool.)

### Config shape

```yaml
serve:
  store:
    backend: ladybug            # memory | sqlite | ladybug | postgres
    path: /data/index.ldb       # embedded backends
    # dsn: postgres://…         # postgres
    dimensions: [games, hosts, designer, publisher]   # frontmatter fields → queryable
```

`backend: memory` with no `store:` block is the zero-config default — nothing changes for an instance that doesn't opt in.

## Consequences

**Good:** persistent index (no re-embed on boot, faster restarts, scales); the enumerate/traversal query class answered correctly and completely; genuinely generic (driven by each instance's declared frontmatter — podcast, Berlin, anything); no storage lock-in; graph modeling available where it fits.

**Cost / risk:** cgo complexity for embedded backends (mitigated: build-tag isolation, pure-Go default + pure-Go scale path); LadybugDB maturity (mitigated: behind the interface, swappable); an interface broad enough to fit vector + FTS + graph needs care not to leak backend specifics (keep `Enumerate` value-based, not Cypher-shaped).

## Rollout (increments, each shippable)

1. **Define `Store` + refactor current retrieval behind it** — `memory` backend reproduces today's behavior byte-for-byte. No cgo, no new deps, pure refactor. (Golden/retrieval tests pin equivalence.)
   **Invariants (fold-in boundary) — the named homes, so the golden-pin refactor can't silently relocate behavior:**
   - **Fusion moves up.** `Store` exposes only primitives — `Semantic`, `Keyword`, (later) `Enumerate`. The current `retrieval.Hybrid` logic (RRF fusion + dedup + MaxChunks cap) does **not** hide inside a Retriever wrapper; it moves **up into an explicit engine-layer query step** that composes Store primitives. The `fulltext` (heading-split lexical) and `semantic` (embed) legs become the `memory` Store's `Keyword`/`Semantic` implementations. `core.Retriever` folds in — no adapter that merely re-abstracts what `Store` already expresses.
   - **Query-embedding moves up too.** Today `Semantic.Retrieve(query)` embeds the query itself; `Store.Semantic(queryEmbedding, k)` takes a vector. So the engine query step **embeds once and passes the vector in**; the semantic leg stops owning the embedder. Name this explicitly or the refactor silently moves the embed call (same vector, new site). Payoff: one embed per query shared across legs — pgvector/ladybug never each re-embed.
   - **`Store.Index` owns the embedding cache seam.** Vault chunk-embedding (now in `NewSemantic` at boot) moves behind `Store.Index(docs)`; the **content-hash embedding cache** lives at that seam so every backend inherits it rather than re-implementing per backend. Increment 1's `memory` Store may leave the seam even if the cache lands later — but the seam is defined now (load-bearing before any large-vault/prod cutover).
   - **Partial-failure semantics preserved.** Today a leg that errors is logged + skipped; only *all* legs erroring fails (else the engine misreads empty context as a legitimate refusal). Keep it exactly: `Store.Semantic` failing while `Keyword` succeeds must still answer. The one-leg-down case goes in the **behavior pins**, not just the happy path.
   - If any logic in `Retriever` today is *not* expressible via these primitives, it gets a named home in the engine layer before this increment closes.
2. **`sqlite` backend** — persistence; proves the cgo/build-tag boundary.
3. **`Enumerate` + the `kb_enumerate` tool** — fixes the under-recall query class (the whole point). Demonstrated on "which episodes cover game X".
   **Normalization contract (invariant):** frontmatter values drift across a KB (`Irish Gauge` vs `irish-gauge` vs `IRISH GAUGE`), so `Enumerate` must not match raw strings. `Store.Index` computes a **normalized key** for each dimension value on the way in (Unicode-fold → lowercase → trim → collapse internal whitespace/hyphens), stores it alongside the raw display value, and `Enumerate` normalizes its query `value` with the **same** function before lookup. Both sides share one normalizer, so casing/format drift can't drop docs; the raw value is retained for display. (This is the concrete home for the earlier "fuzzy-matched" hand-wave — it is deterministic key normalization, not fuzzy scoring; approximate matching, if ever wanted, layers on top and stays out of the exact-lookup path.)
4. **`ladybug` graph backend** — relationships as edges; traversal queries.
5. **`postgres`/pgvector backend** — the pure-Go scale path.

Increment 1 is safe and immediately valuable (it unlocks everything else without changing behavior). 3 is where the user-visible fix lands.
