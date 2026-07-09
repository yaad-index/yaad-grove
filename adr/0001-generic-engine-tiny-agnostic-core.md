# ADR 0001: A generic engine with a tiny, transport- and tool-agnostic core

**Status:** Accepted (2026-07-09)

## Context
The goal is not one bot about one thing. It is an engine for building community
knowledge-base bots: you have a community — a podcast, a magazine, a hobby
group, a person documenting their own work — and a knowledge base about it, and
the bot answers from that base and refuses anything outside it. No single
community can be "the base," and the design must not center on any one of them.

The failure mode to avoid is a smart assistant that freelances from its training
and drifts out of the bounds we intended.

## Decision
A bot is `(vault + tools + scope + transport)`; the engine stays generic. The
core is one function — `Answer(query, user) -> reply` — and it is the only place
that defines answering. Around it, everything is pluggable and the core depends
on nothing concrete:

- **Transport-agnostic.** The core has zero transport dependencies. Each
  platform is a separate adapter implementing a platform-neutral interface;
  "plug anywhere" means adding an adapter, never touching core. Features are
  defined once in the core through a capability interface, in platform-neutral
  terms, and degrade gracefully where a platform lacks a capability (reactions
  fall back to commands). Telegram is the only adapter for now; Discord/Slack
  follow behind the same boundary.
- **Tools are external, via MCP.** Tools are not built into the engine. The
  engine is an MCP client/host; each instance's config lists which MCP servers
  to connect, and their tools become that bot's tools, scoped per instance. A
  future instance plugs different servers with zero core change.
- **Grounding is structural.** The engine retrieves from the curated vault and
  answers only from that context plus tool results, under a scoped system
  prompt, refusing out-of-scope input. A tiny tool surface + scoped prompt +
  refusal leave nowhere to freelance from — the guardrail is the shape of the
  system, not a plea in the prompt.

## Consequences
- Same engine, different config, becomes a different bot. Generic from day one.
- The core is small and stable; churn lives in adapters and instance config.
- No feature may assume the transport, and no tool is special-cased in core.
- Retrieval and the model sit behind interfaces too (see ADR 0005), so they can
  be swapped without disturbing the core.
