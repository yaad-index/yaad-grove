# AGENTS.md

Orientation for agents and contributors working **on** yaad-grove. To *use* a
built bot, see the [README](README.md). This file carries the architecture map,
the conventions, and an index into the architecture decision records (ADRs).
Read the relevant ADR before changing the area it governs.

## The shape

One core function — `Answer(query, user) -> reply` — is the only place that
defines answering. Everything around it is pluggable, and the core depends on
nothing concrete:

- **Transport** — platform-neutral adapters; the core carries zero transport
  dependencies. Features are defined once in neutral terms and degrade where a
  platform lacks a capability. Telegram is the first adapter.
- **Tools** — external, reached over **MCP**. The engine is an MCP client; each
  instance's config lists which servers to connect, and their tools become that
  bot's tools. Zero configured servers leaves a retrieval-only bot.
- **Model** — any OpenAI-compatible endpoint behind a one-method interface.
- **Retrieval** — full-text over the curated vault, behind an interface an
  embedding store can replace later.

## Package layout

```
cmd/yaad-grove/               thin CLI (Kong): parse config, wire collaborators, run
internal/core/                the engine — Answer(), domain types, collaborator interfaces
internal/model/               OpenAI-compatible model client (implements core.Model)
internal/retrieval/           full-text vault retriever (implements core.Retriever)
internal/tools/               MCP client / tool registry (implements core.Tools)
internal/transport/           platform-neutral transport interface + capabilities
internal/transport/telegram/  Telegram adapter
internal/acl/                 consent + access gate + persistent per-user store
internal/pending/             callback token store for interactive actions
internal/quarantine/          consent-gated community-message log, isolated from answering
internal/budget/              persisted spend meter for the global ceiling
internal/runtime/             request handler composing the gate, engine, and actions
adr/                          architecture decision records
```

## Invariants (must hold)

These are load-bearing; a change that weakens one needs its ADR revisited first.

- **Consent is a hard gate** on the whole interaction (ADR 0002, 0007). No
  consent → consent prompt only: nothing answered, nothing the user said
  recorded.
- **Grounding and refusal** (ADR 0008, 0011). The tool surface, the scoped
  prompt, and an explicit refusal path bound the model. Refusal keys on the
  *domain*, not on whether context happens to be present — tools are not a scope
  bypass.
- **Data isolation** (ADR 0004). Community chatter goes to a quarantined store
  that the answering path never reads.
- **Cost is capped** (ADR 0006). A persisted spend meter refuses model calls
  (it does not queue them) once the ceiling is hit, and cannot be turned off by
  zeroing it.
- **Action safety** (ADR 0009, 0010). Interactive action buttons are
  authenticated and intent-labeled, with the label derived from the verb so it
  cannot misrepresent what it does.

## ADR index

| ADR | Decision |
|-----|----------|
| [0000](adr/0000-record-architecture-decisions.md) | Record architecture decisions |
| [0001](adr/0001-generic-engine-tiny-agnostic-core.md) | A generic engine with a tiny, transport- and tool-agnostic core |
| [0002](adr/0002-consent-is-a-hard-gate.md) | Consent is a hard gate on the whole interaction |
| [0003](adr/0003-two-surface-access-and-layered-policy.md) | Two-surface access model with layered per-user policy |
| [0004](adr/0004-growth-loop-quarantined-log-batch-curation.md) | Growth loop — quarantined logging, admin-triggered batch curation |
| [0005](adr/0005-stack-and-conventions.md) | Stack and conventions |
| [0006](adr/0006-global-spend-ceiling.md) | Global spend ceiling — a metered, fail-safe cost backstop |
| [0007](adr/0007-consent-gate-silent-throttle-and-ordering.md) | Consent gate — a silent-throttle decision and consent-before-rate ordering |
| [0008](adr/0008-answer-composition-and-refusal.md) | Answer composition topology and the refusal mechanism |
| [0009](adr/0009-interactive-actions-and-callback-security.md) | Interactive actions and the callback security spine |
| [0010](adr/0010-proposed-actions-and-informed-consent.md) | Proposed actions and informed consent |
| [0011](adr/0011-tool-call-loop-and-grounding-boundary.md) | The tool-call loop and the tool-grounding boundary |

## Conventions

- **Stack:** Go 1.26, a thin Kong CLI; collaborators sit behind interfaces in
  `internal/core` (ADR 0001, 0005).
- **Config:** layers as file < env < flag; secrets come only from the
  environment, never inlined (see `config.example.yaml`).
- **Commits:** Conventional Commits (`feat:`, `fix:`, `chore:`, with scopes like
  `feat(acl):`). Versioning and the CHANGELOG are automated by release-please,
  which reads those messages — so the commit type drives the version bump.
- **CI (required to merge):** `go build ./...`, `go vet ./...`, `gofmt` clean,
  and `go test -race ./...`.
- **Merge:** squash-merge; PRs need review approval plus green CI under branch
  protection.
- **ADRs:** significant decisions get a numbered ADR in `adr/`; supersede rather
  than rewrite history, following the existing format.

## Build, test, run

```sh
go build ./...
go test -race ./...
go run ./cmd/yaad-grove --help
go run ./cmd/yaad-grove serve      # needs config.yaml + the env secrets
```
