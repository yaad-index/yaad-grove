# ADR 0020 — Structured faceted recall (value resolution + vocabulary discovery)

**Status:** Proposed (2026-07-17)
**Relates to:** ADR 0019 (pluggable retrieval store — this refines Increment 3), #135, #115 (Enumerate + kb_enumerate)

## Context

ADR 0019 Increment 3 shipped `Enumerate` + the `kb_enumerate(dimension, value)`
tool: the exact-and-complete answer to "which documents carry value X in
dimension D". The primitive works, but on a live instance the dominant faceted
query — "which items have attribute X" (asked in natural language, often in
another language) — still returns nothing though the KB holds the answer. It is
the #1 recall complaint from real usage.

The vault-data half of the fix is already shipped (the notes now carry the facet
fields). What remains is grove-code: the structured path is **reachable only if
the model can spell the value exactly and knows the dimension holds it**. Three
gaps compound:

1. **Values have no discovery surface.** `kb_enumerate` advertises dimension
   *names* (an enum) but not the *values* each dimension holds. The model
   supplies `value` blind — it guesses a spelling and, on a miss, silently falls
   back to lossy top-k semantic search.

2. **Value matching under-resolves.** ADR 0019's normalization contract folds
   case / whitespace / hyphen / cross-script *characters*, but not other
   punctuation: a facet value like `Route/Network Building` keeps its slash and
   never matches a query typed `route network building`. And a *word-level*
   cross-script or synonym facet ("train games" / «قطاری» → `Trains`) has no
   resolution path unless the value happens to be spelled identically.

3. **Single-dimension only.** `Enumerate` filters one dimension. A compound
   faceted query — "train games for 2 players" — cannot compose two facets.

ADR 0019 already anticipated (2): its "Surface-form / alias resolution"
invariant (Increment 3) states that an entity has many surface forms mapping to
one identity, and `Enumerate` resolves any surface form → entity → documents.
The grove code wired that through the note-alias map (`aliases:` / `name_<lang>`
frontmatter) — and, because that map is a *global* surface-form→canonical
namespace applied to the enumerated value on both backends, a facet value that
unifies with a note entity **already** resolves cross-script today. It was never
documented or tested at the value level, and it does not cover facet values that
have no note of their own. This ADR makes that mechanism explicit and closes the
three gaps.

## Decision

Three additions to the structured-recall surface, all generic (driven by the
instance's declared dimensions), none changing the `memory`-default zero-config
behavior.

### 1. Value-vocabulary discovery — `Store.Dimensions` + `kb_dimensions`

Add one read-only primitive to the `Store` port:

```go
// Dimensions returns, for each declared dimension, its distinct values by
// DISPLAY form (the first-seen raw spelling behind the normalized key), sorted.
// It is the vocabulary a model needs to choose a valid kb_enumerate value.
Dimensions(ctx context.Context) (map[string][]string, error)
```

- `memory`: iterate the structured index; the index now retains a display form
  per normalized key (today it keeps only the key).
- `ladybug`: `MATCH (v:Value {dim: …}) RETURN v.disp` — the `Value` node gains a
  `disp` property (first-seen raw value), leaving `nk` as the match key.

Expose it as a **`kb_dimensions` tool** (no arguments; returns each dimension →
its values). The model calls it before enumerating an unfamiliar dimension. This
is preferred over embedding the vocabulary in the `kb_enumerate` description: a
tool reflects the live index, a static description bloats and goes stale.

**High-cardinality guard.** A dimension like `designer` may hold hundreds of
values. `kb_dimensions` caps the list per dimension (default 50) and, when it
truncates, reports the total count and that the list is partial, so the model
knows to enumerate by a value it already has rather than trust a complete
listing. `Enumerate` itself is never capped — completeness is its contract; only
the *discovery* listing is bounded.

### 2. Value resolution — punctuation fold + value-level aliasing

**2a. Punctuation normalization.** Extend the shared `normalizeKey` separator
class (ADR 0019's normalization contract) to fold ASCII punctuation
(`/ \ ( ) , . : ; & | _` and the like) to the same separator as whitespace and
hyphen. Applied on both sides (index + query), symmetric, so `Route/Network
Building`, `route network building`, and `Route / Network Building` share one
key. The cross-script letter/digit folds are unchanged; the existing
normalization tests still pin them.

**2b. Value-level aliasing.** The primary mechanism is the ADR 0019 surface-form
contract, now stated explicitly for facet values: **a facet value that also
exists as a note resolves through that note's aliases.** If an instance has a
note titled `Trains` with `aliases: [قطاری, rail]`, then
`kb_enumerate(category, «قطاری»)` resolves «قطاری» → `trains` → every document
whose `category` carries `Trains`. This is the graph contract — a facet worth
aliasing is an entity, and its value node and note entity unify on the same
normalized key — and it composes with everything else for free.

For facet values that do **not** warrant their own note, add a thin optional
operator-declared value-alias source (config): a per-dimension
`canonical → [surface forms]` map, registered into the same alias layer at index
time. Additive and optional — without it, only the canonical/normalized form
resolves (the ADR 0019 base case is unchanged).

### 3. Multi-facet composition — tool-layer intersection

Compound facets compose at the **tool layer**, not via a new `Store` method:
`kb_enumerate` accepts either a single `{dimension, value}` (unchanged, backward
compatible) or a list of predicates, and the handler intersects the per-predicate
`DocRef` sets by path (AND). Each predicate is a complete `Enumerate`, so the
intersection is exact and deterministic; it works identically on both backends
with no contract change. (Ladybug already models each value as a first-class
node, so a future push-down to a single multi-hop Cypher traversal is available
as an optimization, not a requirement.)

## Consequences

**Good:** the structured path becomes reachable — the model can discover a
dimension's vocabulary, values resolve across punctuation / script / synonym
drift, and compound facets compose. The #1 recall complaint is fixed
deterministically once the instance declares the (already-populated) dimensions.
Fully generic; `memory`-default behavior and the pure-Go/distroless build are
unchanged.

**Cost / risk:** `Store` gains one method (both backends + the stub implement
it); the `ladybug` `Value` node gains a `disp` property (a reindex, no schema
migration — the graph is rebuilt each index). High-cardinality dimensions risk
tool-output bloat — bounded by the discovery cap, and the operator still chooses
which fields to declare. Punctuation folding is a normalization change: it can
only *merge* keys that were previously distinct (never split), so it cannot drop
a currently-resolving document; the reindex re-derives all keys.

## Rollout (increments, each shippable)

1. **This ADR** (design of record).
2. **Punctuation normalization + value-alias contract** — extend `normalizeKey`;
   document + test value-level alias resolution end-to-end (note-based, both
   backends); optional value-alias config source. (2a + 2b)
3. **`Store.Dimensions` + `kb_dimensions` tool** — display-form retention on both
   backends; the discovery tool with the high-cardinality cap. (1)
4. **Multi-predicate `kb_enumerate`** — predicate-list schema + intersection. (3)

Cut 2 is the smallest user-visible win (punctuation + cross-script values start
resolving); 3 makes the path discoverable; 4 unlocks compound facets.
