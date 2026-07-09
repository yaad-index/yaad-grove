# ADR 0005: Stack and conventions

**Status:** Accepted (2026-07-09)

## Context
yaad-grove should feel like a sibling of the fleet's other Go projects — familiar
to read and maintain — not a bespoke setup. Readability is a first-class goal:
the code should be understandable by its owner, not just runnable.

## Decision
- **Language: Go.** Chosen partly so the code stays readable and reviewable.
- **CLI: Kong** (`github.com/alecthomas/kong`, with `kong-yaml`), the house CLI
  library. Config layers as file < env < flag; secrets come from the
  environment, never inlined. The `cmd/` binary stays thin — parse and wire
  only; behavior lives under `internal/`.
- **Logging: `log/slog`** (stdlib structured logging). Established, zero extra
  dependency, structured from the start.
- **Model: OpenAI-compatible.** The engine depends only on a `Complete`
  interface; the concrete client speaks the OpenAI chat-completions shape, so
  any compatible endpoint works, selected by config (base URL, key, model). No
  provider is baked in. Hosting is a deploy concern, not a design one — build
  the software, install it wherever when it is ready.
- **Retrieval: full-text first.** For a small curated corpus, start with plain
  full-text matching behind the `Retriever` interface; an embedding-backed
  store can replace it later with no engine change.
- **ADRs.** Architecture decisions are recorded as numbered files in `adr/`
  (this set), same as the sibling repos.
- **Reuse mature libraries.** Standard, established deps for cross-cutting
  concerns; do not reinvent.

## Consequences
- The repo's structure and tooling match the other Go projects, so it is
  familiar on sight.
- Swapping model provider or retrieval strategy is a config/impl change behind a
  stable interface, not a rewrite.
- The thin-CLI + internal split keeps wiring and logic separate and testable.
