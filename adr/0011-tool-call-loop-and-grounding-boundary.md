# ADR 0011: The tool-call loop and the tool↔grounding boundary

**Status:** Proposed (2026-07-11)

## Context

ADR 0008 fixed how the engine composes a grounded answer or refuses: retrieve
vault chunks, prompt the model to answer only from that CONTEXT, and emit a
refusal sentinel when it can't. ADR 0001 always intended tools too — the engine
is an MCP client — but Answer was retrieval-only until the tool registry landed
(6c-1). This ADR adds the tool-call loop and, critically, the boundary that keeps
tools from becoming a grounding bypass.

Tools are the largest capability expansion in the codebase. A tool returns
external content, and a naive loop would let that content answer *anything* —
silently undoing the grounded-or-refuse guarantee. So the loop's design is
inseparable from its security boundary.

## Decision

### The model gains a tool-calling interface

`core.Model.Complete` changes from `(system, user string) → text` to
`(messages []Message, tools []ToolDef) → Completion`, where a `Completion` is
*either* a final `Text` *or* a set of `ToolCalls` the model wants to run. This is
the standard function-calling shape. `Message` carries a `Role`, `Content`,
optional `ToolCalls` (assistant turns), and a `ToolCallID` (tool turns) — the
assistant's tool-call id and the tool result's id must correlate on the wire.

Tool argument schemas are passed through to the model as-is; the **MCP server
owns argument validation**, so there is no client-side JSON Schema handling. The
tool *surface* is operator-curated (the instance config picks which MCP servers
to expose), so while tool *arguments* are model-generated, the set of callable
tools is bounded by configuration.

### The loop

`Answer` retrieves, builds the grounded system prompt, then loops: complete with
the tool set; if the model requests tools, run each and append its result as a
tool-role message (scoped context for the next round); when the model returns
text, refuse on the sentinel or answer. The loop is **capped** (5 iterations);
hitting the cap is a graceful refusal, not a hang. Every round goes through the
metered `Model` decorator (ADR 0006), so a multi-call answer is **naturally
bounded by the spend ceiling** — no separate budget logic.

A **tool error** (the tool ran and reported failure) is fed back to the model as
tool-result content, so the model can adapt within the cap. A **call error**
(`core.ErrToolUnavailable` — a dead session, a broken RPC) is infrastructure the
model can't reason around, so it **aborts the loop**. A tool name the model
invents is fed back (not aborted) so the model can correct.

### The boundary: tools ground in-scope answers; they must not widen scope

This is the load-bearing decision. The refusal predicate is anchored on the
instance's **domain**, not on whether some context happens to cover the query.
Once tools can add context, "covered by context" diverges from "in scope": a tool
could supply information *about an out-of-scope topic*, making an out-of-domain
query look answerable. If refusal keyed on context-presence, that would be a
back-door scope expansion.

So the system prompt instructs: **answer only questions within the scope; for
anything outside it, emit the sentinel — even if the context or a tool provides
information about it.** Then:

- **Legitimate tool use survives.** An *in-domain* query the vault doesn't cover
  but a tool can (a podcast bot's transcript search fetching a recent episode) is
  answered, tool-grounded. That is the point of tools.
- **The bypass is blocked.** An *out-of-domain* query is refused, even if a tool
  *could* fetch an answer. Tools extend grounding **within** scope; they never
  redefine scope.

The scope decision is the model's, driven by the prompt — the engine holds no
domain logic. The model's first move is the in-domain check: out-of-domain →
sentinel immediately (no tool calls); in-domain → it may call tools. So the
engine's sentinel detection fires on the final text exactly as before (ADR 0008),
without inspecting tool-call chains. Tool results are scoped, attributed context,
never authority.

## Consequences

- The engine can answer in-domain questions the vault alone can't — the payoff of
  the tool surface — without weakening the grounded-or-refuse guarantee.
- The security boundary is a single prompt property (domain-anchored refusal) plus
  the structural fact that tool results are just more candidate context. There is
  no new trust surface in the engine: the metered decorator caps cost, the cap
  bounds iteration, and the sentinel path is unchanged.
- The `Model` interface change ripples to the OpenAI adapter and the metered
  decorator (both additive) but not beyond — `core` still depends only on its
  interfaces.
- Open, by design: the model, not code, enforces the domain boundary. That is the
  same trust placed in the model by ADR 0008's refusal; tools don't change its
  nature, only its stakes. A future unit could add a structural out-of-scope
  check if the prompt-level guarantee proves insufficient in practice.
