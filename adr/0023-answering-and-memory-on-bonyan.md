# ADR 0023 — Answering on bonyan, and long-term memory per user

**Status:** Proposed (2026-10-03)
**Builds on:** ADR 0001 (generic engine), ADR 0008/0011 (grounding contract and tool loop), ADR 0012 (consent), ADR 0014 (bounded conversation buffer), ADR 0015 (transcript). Depends on an amendment to bonyan's ADR 0001 §4 (extracting facts is the memory backend's job).

## Context

The engine answers through its own model client and tool loop (`internal/core`, `internal/model`), and keeps no state about a person beyond the consent record. Its only memory is the in-process buffer of ADR 0014: keyed by chat, bounded, lost on restart, and purged per user on withdrawal. Nothing is remembered about a person from one conversation to the next.

bonyan (`github.com/yaad-index/bonyan`) is a Go agent library that already does what the engine's loop does, under rules the engine has no equivalent of: every untrusted text is classified and marked by a trust policy, resolved secrets are scrubbed before anything is stored, recorded or logged, spend is metered per run, runs are recorded for evaluation, and memory is a store whose backends must delete by subject.

Three requirements drive this ADR:

1. Long-term memory lives in the framework, as a bonyan memory backend, and the backend is the memory service from Plastic Labs, `github.com/plastic-labs/honcho` (AGPL-3.0), run as a separate service and reached over its HTTP API. It is never vendored or linked.
2. Memory is per user: the chat user is the memory subject.
3. The engine remembers facts and data from conversations, and each person's opinions.

## Decision

### 1. Answering moves onto bonyan; the bot's own rules stay

One directed message that the consent gate serves becomes one bonyan agent run.

| Before this ADR | On bonyan |
|---|---|
| `core.Model` and the OpenAI-compatible client | the agent's chat model slot |
| the tool loop of ADR 0011 | the agent loop |
| MCP servers through `internal/tools` | bonyan's MCP client, same allow and deny lists |
| `kb_enumerate`, `kb_dimensions` | bonyan tools over the same `store.Structured` |
| retrieved vault chunks in the system prompt | the run's material, placed by bonyan |
| the ADR 0014 buffer in the system prompt | the run's history |
| `internal/budget` metering per model call | bonyan's budget; the ceiling and its period are kept, and whether bonyan's budget holds the period or the engine's meter stays around the model is decided in that increment |

The bot keeps its transport, its consent gate and decisions (ADR 0012), retrieval and the store (ADR 0017, 0019, 0020, 0022), the quarantine log and the transcript, the persona, scope, language pack and prompt template, and the grounding contract of ADR 0008: the refusal sentinel is parsed from the run's answer, and a question with no grounding, no tools, no history and no reply context still refuses without a model call.

The move is made in increments. Each keeps the engine's answers byte-identical where its behaviour is not meant to change, pinned by golden tests, as ADR 0019's first increment was.

### 2. Subjects and sessions

- **Namespace:** each instance of the engine has its own memory namespace, set in its configuration, and may give a group chat a namespace of its own, mapped from the chat's ID. A group chat not in the map uses the instance's namespace. No namespace has a built-in default: each is the deployment's own value. bonyan applies a namespace when it builds a memory store, one store per namespace, so two instances sharing one memory service, or two groups with namespaces of their own, never see each other's records, and neither the engine nor the backend can reach outside a namespace (bonyan ADR 0001 §4).
- **Subject:** the chat user's ID. Every memory record is about one user.
- **Session:** the chat ID, the user's ID and the window the session started in. bonyan's session history is per subject, so a session is one person's turns in one chat. The window is in the key because the service deletes messages only a whole session at a time: a session that ends with its window can be deleted whole once retention has passed, where one that went on forever would keep its oldest messages. A window's length is set in configuration, and the retention period must be a whole number of windows: the purge runs at each window boundary, so its cut falls between sessions and deletes each one whole, where a cut inside a session would rewrite it at every purge.
- **The group's recent conversation** (several speakers) stays the ADR 0014 buffer and reaches the run as its history. It is not long-term memory, and its rules do not change.
- **Long-term memory** is per user and across the chats that share a namespace: what a person said in one group can be recalled when they speak in another group of the same namespace. A user in two groups with different namespaces has two separate memories, and nothing kept in one is recalled in the other.

### 3. What is remembered: the memory service derives it

Extracting facts is the memory backend's job, not bonyan's or the engine's (bonyan ADR 0001 §4 as amended). bonyan stores the user's admitted turns and the engine's answers as events, scrubbed of resolved secrets; the service's own deriver forms conclusions about the user from them, and its representation of the user, on a model the deployment configures for it (a local model endpoint, for example).

- **Opinions** are conclusions like any other, kept as text ("thinks the expansion is too long"). There is no separate fact or opinion type, and nothing in bonyan or the engine classifies conclusions after the fact.
- **The bot** is a peer the service does not model; only the user is observed.
- **Statements about another person:** the workspace's custom instructions tell the deriver to form conclusions about the speaker only, and not to keep what the speaker says about someone else. This **steers** the deriver; it does not enforce anything. A conclusion about another person can still be formed, and when one is, it is stored under the speaker's subject and erased with the speaker's data, never under the other person's.

### 4. Trust: memory personalises, it never grounds

- Recalled records are untrusted: each comes back as memory whose source is the kind of the turns it was derived from, classified under that kind and as model output, since the service's model wrote it, inside a marked section. No record is ever trusted, whatever it says (bonyan ADR 0001 §4).
- The grounding boundary of ADR 0008 and 0011 holds. Memory may shape an answer to the person asking (their language, what they said before, what they prefer). It cannot answer an in-scope question in place of retrieval, and it cannot widen the scope: a question the vault cannot ground is still refused, whatever memory holds.

### 5. Consent and deletion

- **Only consented turns reach long-term memory:** a directed group turn the consent gate serves, and the engine's answer to it. Ambient turns the gate only logs are not kept, and neither are the DMs the engine answers for an admin, who need not have consented. Nothing is derived from a turn that is not kept.
- **Every event is scrubbed of resolved secrets before it reaches the service**, so the service's deriver never sees what bonyan would not store.
- **Withdrawal (`/consent remove`) deletes the subject:** every event and every derived record about that user, in every chat and every namespace the instance has kept memory in, through bonyan's `DeleteSubject`. This is stronger than the buffer-only purge it replaces. The consent disclosure says that memory is kept and is erased on withdrawal. The quarantine log and the transcript keep their own rules (ADR 0004, 0015); their withdrawal stays prospective, as disclosed.
- **Retention:** a retention period is required, and the engine runs bonyan's purge on a schedule, in every namespace the instance has kept memory in.
- **A namespace dropped from the configuration is not forgotten.** The engine keeps a durable record of every namespace it has kept memory in, and withdrawal and the retention purge cover every recorded namespace, configured or not. A namespace leaves the record only when a purge has run in it after everything it held has expired, so removing a group's mapping never leaves memory that withdrawal cannot reach or retention cannot delete.

### 6. The service as a bonyan memory backend

The backend is `memory/honcho` in bonyan's repository: a Go module of its own, with its own `go.mod`, as `memory/sqlite` is, and nothing of it, code or dependency, in bonyan's core module. It plugs in like any backend: the engine registers it under a name in bonyan's registry and selects it in configuration. It is a client of the service's v3 HTTP API, written against the service's published OpenAPI document, since no Go SDK exists, and it must pass bonyan's memory conformance suite. The suite cannot wait for the service's deriver, so the backend's own tests, against a running service, show that deleting a subject leaves no derived record.

| bonyan backend | the service |
|---|---|
| a subject | one workspace, named by the namespace and the user's ID, escaped, holding the user's peer and the bot's peer |
| an event | a message in a session, from the user's peer or the bot's, carrying bonyan's fields for the record (layer, source, server, decision) in its metadata, and the record's time as its creation time |
| a fact a program remembers | a message in a reserved facts session, carrying its fields the same way, with the deriver turned off for it |
| a fact the service derives | a conclusion the deriver formed about the user's peer, read as an untrusted record whose time is the conclusion's creation time |
| recall | a semantic search of the facts session, and a semantic query over the user's conclusions; whether the user's representation also comes back, as a record, is settled in the backend's increment |
| history | the session's messages |
| `DeleteSubject` | deactivate the subject's sessions, then delete the workspace |
| `DeleteBefore` | delete each conclusion older than the cut and each session wholly before it, and rewrite a session that straddles the cut (below) |

**Why records are messages.** A conclusion has no field to carry a record's source, server and decision, and the service sets its creation time itself, so a record written as a conclusion could not come back as written, which bonyan's conformance suite requires. A message carries metadata and takes its creation time from the writer, so every record bonyan writes is a message, and only what the deriver forms is read from conclusions.

**Rewriting a session that straddles the cut.** The service deletes messages only a whole session at a time, so a session holding records on both sides of the cut is rewritten: its newer messages are written to a new generation of the session, tagged with a generation ID in their metadata and re-added with the deriver turned off so it does not derive from them twice, and only then is the old generation deleted. A purge interrupted between the two steps leaves both generations, and the next purge finds the half-done rewrite, finishes it and removes the old one: a crash may leave a duplicate until the next purge, and never loses a newer record. The generation ID is part of the session's name. Sessions keyed by their retention window (§2) make a rewrite rare.

**Namespaces.** The service's workspaces do not nest, so the namespace is part of each workspace's name. The backend refuses to read or delete a workspace outside its namespace, so one namespace's withdrawal or retention purge can never touch another namespace's users, whether the other belongs to another instance or to another group of the same one.

**Why one workspace per subject:** the service cannot delete a peer or a single message, and deleting a session leaves in place the peer itself and every conclusion the service keeps outside that session. Deleting a workspace is its only complete erase. The cost is one workspace per user who consents, each with its own configuration, and no modelling across users, which this design does not want anyway. A backend that cannot show complete deletion does not ship.

## Invariants (acceptance)

- A turn from a user who has not consented never reaches the service.
- Two namespaces never see each other's memory, and neither can delete the other's, whether they belong to two instances or to two groups of one.
- After `/consent remove`, recall for that user returns nothing, in any chat and any namespace the instance has kept memory in, configured or not, and the service holds no workspace for them.
- Recalled memory never enters the request as trusted.
- A question the vault cannot ground is refused whether or not memory holds an answer to it.
- No text reaches the service before bonyan's scrubber has removed every resolved secret from it.
- Where a migration increment is not meant to change behaviour, the engine's answers are byte-identical before and after it, pinned by golden tests.

## Consequences

- People are remembered across conversations, under consent, and erased completely on withdrawal.
- The engine gains bonyan's trust marking, secret scrubbing, recording and evaluation for every answer.
- **The deriver's model calls are the service's, not bonyan's.** bonyan neither meters nor records them: their spend is outside the engine's spend ceiling, on the model the deployment configures for the service, and what the deriver concluded, and why, is outside bonyan's recordings and evaluation.
- Opinions have no type of their own, so a policy or a prompt cannot treat them differently from other conclusions except by what their text says.
- Keeping statements about other people out is steering, not a guarantee (§3).
- A group given a namespace of its own keeps its members' memory apart from the instance's other groups. A user in both has two memories, and withdrawal has to erase every one of them, so the engine records every namespace it has kept memory in, including one whose mapping was later removed.
- A deployment that uses long-term memory runs one more service, with its own database. Without it, the engine answers as before, with the ADR 0014 buffer only.

## Alternatives considered

- **A bonyan extraction step writing to the service.** It would keep extraction under bonyan's scrubbing, budget and recording, and could type records as fact or opinion. Rejected: it leaves the service only storage and search, which bonyan's sqlite backend already gives. Secret scrubbing is bonyan's; extracting facts is the memory layer's.
- **A bonyan extraction step writing to sqlite, with no service.** Rejected for the same reason: extraction is the memory layer's, and the service is the memory layer this ADR adopts.
- **A separate opinion type.** Rejected: it would need a classification pass over the service's conclusions, which is the extraction step by another name. An opinion is a conclusion's text.
- **Enforcing that nothing is kept about other people.** Not available: the deriver is the service's, and its custom instructions steer it without enforcing anything. §3 says so rather than promising it.
- **One workspace for all users, one peer per user.** Rejected: the service cannot delete a peer, so withdrawal could not erase a user completely.

## Deferred to the build

- Whether the vault's chunks enter the run as trusted material, since the vault is operator-curated, or untrusted, as the engine's own prompt treats them. Decided in the increment that moves retrieval onto the run, with golden tests on the prompt.
- How soon after a turn its conclusions must be recallable. bonyan already promises only that derived records may lag, and the service derives in batches.
