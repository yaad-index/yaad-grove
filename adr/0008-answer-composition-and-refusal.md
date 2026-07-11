# ADR 0008: Answer composition topology and the refusal mechanism

**Status:** Accepted (2026-07-09)

## Context

`core.Engine.Answer` is where the four Phase-1 collaborators — the retriever
(ADR 0001), the model (ADR 0005), the spend meter (ADR 0006), and the consent
gate (ADR 0002/0003/0007) — must compose into an actual answer that refuses
anything it cannot ground.

An import constraint shapes the composition. `acl` imports `core`, and `budget`
is core-independent. So **`core` may import neither**: an `acl` import would
cycle, and pulling `budget` into `core` would both couple the engine to the meter
and erode the ADR 0001 boundary that the core knows only interfaces. The engine
therefore cannot own the gate or the meter directly — yet ADR 0006 requires the
ceiling to sit "on the model-call path," and the grounding guarantee (ADR 0001)
requires refusal. This ADR fixes how those compose.

## Decision

### The engine stays pure

`Answer` depends only on the existing `Model` / `Retriever` / `Tools` interfaces.
Its flow: **retrieve → assemble a grounded prompt (scope + chunks) → model.Complete
→ refuse if ungrounded.** `core` imports no transport, no `acl`, no `budget`.

### The spend ceiling is a `core.Model` decorator, in `internal/runtime`

A small `meteredModel` wraps a `core.Model`: it calls `meter.Allow()` before
`Complete` (over budget → a typed error, **no** underlying call) and
`meter.Record(usage.TotalTokens)` after a successful call. It is injected as the
engine's `Model`, so ADR 0006's "checked on the model-call path" stays literally
true while `core` stays free of `budget`.

It lives in a new **`internal/runtime`** package, not `internal/model`. The
choice: `internal/runtime` imports `core` + `budget` + `model` and is the
composition layer, which keeps `internal/model` a pure OpenAI adapter and keeps
`budget` core-independent (the property the import constraint relies on).
`internal/runtime` is also where the consent-gate composition lands (Unit 6), so
the decorator and the gate share the one boundary layer.

### The consent/ACL gate stays at the runtime boundary (Unit 6, not here)

The gate's decisions are transport-rendered and it can't live in `core` (cycle),
so it composes in `internal/runtime` ahead of the engine:
`gate.Check → on DecideServe → engine.Answer`. This ADR documents the seam; the
gate wiring is Unit 6. The over-budget error the decorator returns is likewise
surfaced at this boundary (the runtime can import `budget`; the engine cannot).

### Refusal is two-layered

The grounding guarantee is "refuse anything outside scope." Two layers:

1. **Empty-grounding short-circuit.** If retrieval returns zero chunks (and no
   tool applies), `Answer` returns `Reply{Refused: true}` **without** a model call
   — honest ("I can't ground this") and spends nothing.
2. **Model-signalled refusal.** The system prompt instructs the model to reply
   with a single machine sentinel when the context does not support an answer.
   `Answer` detects the sentinel and sets `Refused: true`, replacing it with a
   fixed user-facing refusal line. A prompt contract plus a sentinel check is
   enough for Phase 1 — no classifier, no template engine.

### Grounded prompt

`Answer` assembles the scope, the refusal contract, and the retrieved chunks —
each tagged with its `Source` so the model can cite and the answer stays
attributable — into the system message; the user message is the raw query. Plain,
deterministic string assembly.

### Tools are a documented seam

The engine carries a `Tools` field, but Unit 5 leaves `Answer` retrieval-grounded.
The MCP tool-call loop lands with the transport/tools unit; the seam is a doc note,
not dead code.

## Consequences

- The engine composes only interfaces, so `core` stays transport-, acl-, and
  budget-free (ADR 0001 preserved; no import cycle).
- The ceiling is enforced exactly where a model call happens, transparently to the
  engine, and an over-budget refusal is a plain model error the runtime handles.
- `internal/model` stays a pure provider adapter and `budget` stays
  core-independent; composition concerns (metered model now, gate next) collect in
  one `internal/runtime` layer.
- Refusal never depends on a fragile parse of free text: empty grounding is
  structural, and the model path uses an explicit sentinel contract.
- A best-effort accounting gap is possible if `meter.Record` fails after a
  successful (already-paid) call — the running meter still counts it in memory;
  only persistence lags. The decorator logs it and returns the answer rather than
  discarding a paid completion.
