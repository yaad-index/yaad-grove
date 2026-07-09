# yaad-grove

A config-driven engine for building **community knowledge-base bots**.

You have a community — a podcast, a magazine, a hobby group, a person
documenting their own work — and a knowledge base about it. yaad-grove answers
from that base and its tools, and refuses anything outside them. Smart enough to
understand the question, never allowed out of the bounds you designed.

> **Status: scaffold.** This is the Phase-1 skeleton — package boundaries,
> interfaces, CLI, and the recorded design (see [`adr/`](adr/)). No behavior is
> implemented yet; every stub returns a "not implemented" error by design.

## What it is

An **engine, not a bot**. A bot is `(vault + tools + scope + transport)`; the
engine stays generic. Same engine, different config, becomes a different bot —
no single community is baked in.

The guardrail is structural: a tiny tool surface, a scoped system prompt, and
refusal leave the model nowhere to freelance from. Grounding on the answering
side is mirrored by isolation on the data side — community chatter is logged to
a quarantined store the answering bot never reads.

## Architecture

One core function — `Answer(query, user) -> reply` — is the only place that
defines answering. Everything around it is pluggable and the core depends on
nothing concrete:

- **Transport** is a set of cleanly extractable adapters. The core carries zero
  transport dependencies; features are defined once in platform-neutral terms
  and degrade gracefully where a platform lacks a capability. Telegram first;
  Discord/Slack behind the same boundary later.
- **Tools** are external, reached over **MCP**. The engine is an MCP client;
  each instance's config lists which MCP servers to connect, and their tools
  become that bot's tools, scoped per instance.
- **Model** is any **OpenAI-compatible** endpoint, behind a one-method
  interface. **Retrieval** starts as plain full-text over the curated vault,
  behind an interface an embedding store can replace later.

### Access & consent

- **Consent is a hard gate.** No consent → the bot only sends a consent prompt;
  it does not answer and records nothing the user said. Answering and logging
  begin only after opt-in.
- **Two surfaces.** Group members may talk to the bot (membership is the
  boundary); DMs are served only to users an admin has approved.
- **Layered policy** — per-user override > tier > default — with per-user rate
  limits and a global spend ceiling. Admins configure it all live by talking to
  the bot.

See the ADRs for the reasoning:

| ADR | Decision |
|-----|----------|
| [0001](adr/0001-generic-engine-tiny-agnostic-core.md) | Generic engine, tiny transport- and tool-agnostic core |
| [0002](adr/0002-consent-is-a-hard-gate.md) | Consent is a hard gate on the whole interaction |
| [0003](adr/0003-two-surface-access-and-layered-policy.md) | Two-surface access model, layered per-user policy |
| [0004](adr/0004-growth-loop-quarantined-log-batch-curation.md) | Growth loop — quarantined logging, batch curation |
| [0005](adr/0005-stack-and-conventions.md) | Stack and conventions |

## Layout

```
cmd/yaad-grove/         thin CLI (Kong): parse config, wire, run
internal/core/          the engine — Answer(), domain types, collaborator interfaces
internal/model/         OpenAI-compatible model client (implements core.Model)
internal/retrieval/     full-text vault retriever (implements core.Retriever)
internal/tools/         MCP client / tool registry (implements core.Tools)
internal/transport/     platform-neutral transport interface + capabilities
internal/transport/telegram/  Phase-1 Telegram adapter
internal/acl/           consent + access gate + persistent per-user store
adr/                    architecture decision records
```

## Build & run

```sh
go build ./...
go run ./cmd/yaad-grove --help
go run ./cmd/yaad-grove version
```

Configuration layers as **file < env < flag**. A YAML config is searched at
`/etc/yaad-grove/config.yaml` and `./config.yaml`; see
[`config.example.yaml`](config.example.yaml). Secrets come from the environment,
never inlined:

- `YAADGROVE_MODEL_API_KEY` — OpenAI-compatible API key
- `YAADGROVE_TELEGRAM_TOKEN` — Telegram bot token

## Phasing

- **Phase 1** — the answering bot: grounded answering, tool registry, transport
  adapter, consent + access, rate limits, **and** consent-gated logging to the
  quarantined store (so Phase 2 has data).
- **Phase 2** — curation: an agent runs against the logged store and the vault,
  proposing and committing reviewed edits, admin in the loop. No overseer code
  in the engine for now.

## License

MIT — see [LICENSE](LICENSE).
