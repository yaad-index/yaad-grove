# ADR 0026: Telemetry exports metadata, never content

**Status:** Proposed (2026-10-06)

## Context

grove can send traces and metrics to an OpenTelemetry collector over OTLP. The agent library it answers with (ADR 0023) emits a span for each run, loop step, model call and tool call, and the GenAI metrics for model calls. grove adds metrics of its own: answers by outcome, retrieval, embedding calls and the spend ceiling.

The agent library can also put content on its spans: the instructions, the messages sent to the model and its reply, and each tool call's arguments and result. A user's question, the recent conversation (ADR 0014) and the replied-to message would all be among them.

A collector is outside every store the consent rules reach. Withdrawal (ADR 0012, 0025) stops what is recorded about a user, the transcript keeps only consented turns (ADR 0015), and memory can be erased (ADR 0014, 0023). None of that reaches a span that has already been exported. Content sent there could not be withdrawn or erased, and it would be kept for as long as the collector keeps its data, not as long as grove does.

Metrics have the same problem in another form. An attribute that takes a user id, a chat id or a name makes a series per person, and the collector keeps a record of that person's activity.

## Decision

- **Export only when an endpoint is set.** grove sends telemetry only when the standard OTLP environment variables name an endpoint. With none set, nothing leaves the process and nothing is buffered for later.
- **Content capture is never on.** grove builds the agent library's telemetry with content capture off, and there is no flag, setting or environment variable to turn it on. Spans and metrics carry what a run did, never what anyone wrote:
  - **Allowed:** the model and tool names, token counts, durations, the step number, the request's limits, how the run ended, a tool call's id, the kind of an error, and a hash of the operator's instructions (the template, persona and scope, not the conversation).
  - **Never:** a question, an answer, the conversation, a replied-to message, a name, vault text, the instructions themselves, a tool's arguments or result, or an error's text.
- **Metric attributes are bounded.** Every metric attribute takes values from a small fixed set: a surface, an outcome, a retrieval mode, a model or tool name from the instance's configuration. A user id, a chat id, a name or any text is never an attribute value or part of a metric name.
- **A message is counted only once it reaches the engine.** None of the gate's decisions is a metric: its nudges, silences, throttles, the ambient messages it only logs, and its fail-closed refusals (a store error) are not counted, since none of them reaches the engine. This is apart from the engine's own refusal of an out-of-scope question, which is an answer and is counted under its outcome. Nudges and silences are also the unconsented path: nothing is counted about a user who has not consented (ADR 0002).
- **Names follow the conventions.** A metric takes the OpenTelemetry semantic conventions' name where one exists, else `grove.<area>.<thing>`. Its unit goes in the unit field. The README lists every metric with its unit and attributes.

## Consequences

- An answer cannot be debugged from telemetry alone. Telemetry shows which model and tools a run used, how long each step took and how it ended, but not what was asked or answered. The text of an answer is in the transcript when one is kept (ADR 0015), under the consent rules.
- Withdrawal and erasure need nothing from the collector: there is nothing of the user's there to remove.
- Turning content capture on takes a new ADR that supersedes this one. A debugging flag is not enough.
- How many unconsented users are nudged or silenced is not observable. That is the price of ADR 0002's "record nothing without consent", now applied to counts as well.
- A new metric or attribute is reviewed against this list. Its attribute values must come from a fixed set, and a metric about messages may count only those that reach the engine.
