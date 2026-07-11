# yaad-grove

A config-driven engine for building **community knowledge-base bots**.

You have a community — a podcast, a magazine, a hobby group, a person
documenting their own work — and a knowledge base about it. yaad-grove answers
from that base and its tools, and refuses anything outside them: smart enough to
understand the question, never allowed out of the bounds you designed.

> **Built by agents.** yaad-grove is designed and implemented by a fleet of AI
> agents, working through pull requests, independent review, and the
> architecture decision records in [`adr/`](adr/). See [AGENTS.md](AGENTS.md)
> for how the project is structured for them.

## What it is

An **engine, not a bot**. A bot is `(vault + tools + scope + transport)`; the
engine stays generic. The same engine, pointed at a different config, becomes a
different bot — no single community is baked in.

The guardrail is structural: a tiny tool surface, a scoped system prompt, and an
explicit refusal path leave the model nowhere to freelance from. Grounding on
the answering side is mirrored by isolation on the data side — community chatter
is logged to a quarantined store the answering bot never reads.

**Status:** functional (v0.1.0). The engine answers from a vault over Telegram,
with the consent gate, per-user rate limits, and the global spend ceiling all
live. Phase 2 (agent-assisted curation of the knowledge base) is planned; see
[AGENTS.md](AGENTS.md) and the [ADRs](adr/) for the design and roadmap.

## Quickstart (Docker)

A container image is published to GHCR on every release:

```sh
docker pull ghcr.io/yaad-index/yaad-grove:0.1.0   # or a later release tag
```

1. **Write a config.** Copy [`config.example.yaml`](config.example.yaml) to
   `config.yaml` and set the vault directory, the scope statement, and the model
   endpoint. Config layers as **file < env < flag**.

2. **Provide secrets via the environment** (never inline them in the config):
   - `YAADGROVE_MODEL_API_KEY` — an OpenAI-compatible API key
   - `YAADGROVE_TELEGRAM_TOKEN` — a Telegram bot token

3. **Run**, mounting the config and vault:

   ```sh
   docker run --rm \
     -e YAADGROVE_MODEL_API_KEY \
     -e YAADGROVE_TELEGRAM_TOKEN \
     -v "$PWD/config.yaml:/etc/yaad-grove/config.yaml:ro" \
     -v "$PWD/vault:/vault:ro" \
     ghcr.io/yaad-index/yaad-grove:0.1.0 serve
   ```

   Point `vault-dir: /vault` in the config at the mounted path.

## Telegram setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and put its token in
   `YAADGROVE_TELEGRAM_TOKEN`.
2. Add the bot to the community's group chat.
3. Put that group's chat id in `telegram-allowed-groups`. Membership in an
   allowed group is the access boundary for the group surface; DMs are served
   only to users an admin has approved.

## Configuration

Key fields (the full annotated reference lives in
[`config.example.yaml`](config.example.yaml)):

| Field | What it does |
|-------|--------------|
| `serve.vault-dir` | Curated markdown vault the bot grounds its answers on. |
| `serve.scope` | System prompt that bounds the bot; half of the grounding guarantee — keep it specific. |
| `serve.model-base-url` / `serve.model-name` | Any OpenAI-compatible endpoint (OpenAI, DeepSeek, GLM, a hosted gateway, or a local one). |
| `serve.spend-ceiling` / `serve.spend-period` | Hard token-cost backstop per window; persisted so a restart cannot reset it. |
| `serve.telegram-allowed-groups` | Group chat ids that count as "the community." |
| `serve.default-tier` | Rate-limit tier for users without a per-user override. |

The model is any **OpenAI-compatible** endpoint — swap providers by changing
`model-base-url` + `model-name` + the key, with no code change.

## Access & consent

- **Consent is a hard gate.** Before opt-in the bot only sends a consent prompt;
  it does not answer, and records nothing the user said.
- **Two surfaces.** Group members may talk to the bot (membership is the
  boundary); DMs are served only to admin-approved users.
- **Layered limits** — per-user override > tier > default — with per-user rate
  limits under a global spend ceiling.

## From source

```sh
go build ./...
go run ./cmd/yaad-grove --help
go run ./cmd/yaad-grove version
go run ./cmd/yaad-grove serve      # needs config.yaml + the env secrets
```

Requires Go 1.26+.

## Releases

Versioned with [release-please](https://github.com/googleapis/release-please)
from Conventional Commits; each release publishes a container image to
`ghcr.io/yaad-index/yaad-grove`.

## Contributing

Architecture, conventions, and the design record are in
[AGENTS.md](AGENTS.md). Read the relevant ADR before changing an area it covers.

## License

MIT — see [LICENSE](LICENSE).
