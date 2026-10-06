# yaad-grove

A config-driven engine for building **community knowledge-base bots**.

You have a community — a podcast, a magazine, a hobby group, a person
documenting their own work — and a knowledge base about it. yaad-grove answers
from that base and its tools, and refuses anything outside them: smart enough to
understand the question, never allowed out of the bounds you designed.

> **Built by agents.** yaad-grove is designed and implemented by a team of AI
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

## Telemetry

`serve` can send traces and metrics to an OpenTelemetry collector over OTLP. It
is off unless an endpoint is set, and is configured only by the standard
environment variables:

- `OTEL_EXPORTER_OTLP_ENDPOINT` turns on both signals;
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
  turns on that signal alone.
- `OTEL_EXPORTER_OTLP_PROTOCOL` (or the per-signal `…_TRACES_PROTOCOL` /
  `…_METRICS_PROTOCOL`) is `http/protobuf`, the default, or `grpc`.
- The other `OTEL_EXPORTER_OTLP_*` variables (headers, timeout, compression,
  certificate) work as the standard describes. `OTEL_SDK_DISABLED=true` turns
  export off.
- The service is `yaad-grove` at the build version; `OTEL_SERVICE_NAME` and
  `OTEL_RESOURCE_ATTRIBUTES` override it.

Each answer is a trace: an `invoke_agent grove` span with a `bonyan.step` span
per loop step, and under those a `chat <model>` span per model call and an
`execute_tool <tool>` span per tool call. The metrics follow the OpenTelemetry
GenAI conventions:

| Metric | Unit | What it measures |
|--------|------|------------------|
| `gen_ai.client.operation.duration` | `s` | How long each model call took. |
| `gen_ai.client.token.usage` | `{token}` | Input and output tokens per model call. |
| `bonyan.usage.cost` | `1` | Cost per model call. Always 0 here: the spend ceiling counts tokens, so no price is set. |

Telemetry carries what a run did, never content: no message text, names, user
or chat ids, prompts, vault text, tool arguments or tool results. The
attributes are the model and tool names, the tool call's id, token counts,
durations, the step number, how the run ended, the kind of any error, and a
hash of the instructions the model was given.

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

### Replaying questions

`replay run` answers a file of questions with the engine `serve` would build,
from the same flags and the `serve` section of `config.yaml`, and writes one
JSON line per answer: the answer, whether it was refused and why, the model
calls it took, and the run's model and `--label`. Each question is asked once
and on its own: no history, no consent gate, no long-term memory, a spend meter
of its own in memory, and the vault indexed in memory (a persistent store is
never opened). `replay compare` prints two such runs side by side and flags
changed refusals, errors and empty answers, so two builds can be read against
each other on the same questions.

```sh
yaad-grove replay run --questions questions.jsonl --out old.jsonl --label A   # build A
yaad-grove replay run --questions questions.jsonl --out new.jsonl --label B   # build B
yaad-grove replay compare old.jsonl new.jsonl
```

A refusal's reason is `no-call` (nothing retrieved and no tools, so no model
call), `model` (the model declined in its own words) or `fixed` (the engine's
fixed decline after model calls: the step limit, or a decline with no words).

With `--record-dir`, each question's run is also recorded to a file of its own
(every model request and response, tool results included, scrubbed of the
process's secrets), named on the question's line as `recording`. A recording
keeps the question file's text, so give it no real people's messages. `serve`
never records.

A question line is `{"id": "q1", "query": "…"}`; an optional `query_en` is
asked too, as `q1@en`. Other fields are ignored.

## Releases

Versioned with [release-please](https://github.com/googleapis/release-please)
from Conventional Commits; each release publishes a container image to
`ghcr.io/yaad-index/yaad-grove`.

## Contributing

Architecture, conventions, and the design record are in
[AGENTS.md](AGENTS.md). Read the relevant ADR before changing an area it covers.

## License

MIT — see [LICENSE](LICENSE).
