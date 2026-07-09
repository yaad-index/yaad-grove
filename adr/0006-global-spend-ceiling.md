# ADR 0006: Global spend ceiling — a metered, fail-safe cost backstop

**Status:** Proposed (2026-07-09)

## Context

Every answered query costs money: the bot calls an LLM (ADR 0005), and cost
scales with tokens. The per-user rate limit (ADR 0003, the ACL `Record`'s rate
counter) bounds any single user's frequency, but it is a **fairness** control,
not a **cost** control — N users each under their limit, a retry storm, or a
crash-loop can still run total spend away. The ACL comment already defers to
"the real backstop": a global ceiling. Nothing implements it yet.

Cost-safety must exist **before** any model-call path does, so the safety is
never retrofitted after money can already be spent. This ADR defines that
backstop.

## Decision

A single **global meter** caps total model spend per period; the model-call path
consults it before every call and refuses when the ceiling is reached. It is
global (all users, all surfaces aggregate into one meter) — the aggregate money
cap, distinct from and consulted alongside the per-user rate limit.

### Unit: tokens

The ceiling is expressed in **tokens**, not a currency amount. Tokens are the
model-agnostic cost driver the model reports directly (an OpenAI-compatible
response carries `usage.total_tokens`), so a token ceiling needs no per-model
price table and no floating-point money arithmetic. An operator sets the token
budget from their model's known price. Converting the meter to a currency budget
(multiply by a configured price-per-token) is a later refinement behind the same
meter, not needed for the backstop.

### Mechanism

- **Pre-call gate.** Before a model call, the path checks `Allow()`: true while
  the period's accumulated tokens are below the ceiling, false once the ceiling
  is reached. A false gate is a refusal (the reply declines rather than
  spending), the same fail-safe as an out-of-scope query.
- **Post-call accounting.** After a call returns, the path `Record(tokens)`s the
  response's actual token usage into the meter. Accounting is post-call because
  exact usage is only known then; the pre-call gate blocks further calls once the
  ceiling is crossed. Because usage is recorded only after a call returns, calls
  already in flight when the ceiling is crossed still complete, so overspend is
  bounded by the number of concurrently in-flight calls at that moment — one for
  a serial caller, at most the in-flight concurrency otherwise. Acceptable for a
  backstop.
- **Fixed period, rolling reset.** The meter accumulates over a configured period
  (e.g. daily or monthly) and resets when the period elapses. A fixed budget
  window is the operator-legible model ("N tokens per day").
- **Persisted.** The meter's `{period-start, spent}` state is persisted (bbolt,
  the house store — ADR 0005) and reloaded at startup, so a restart or crash-loop
  cannot reset the meter and blow the budget. This is the failure the backstop
  most needs to survive.
- **Refuse, not queue.** An over-ceiling call is refused with a typed error
  (`ErrOverBudget`), not queued to drain later — draining is just deferred
  spend. The refusal is the same fail-safe as an out-of-scope query.
- **A conservative default is always in force.** The ceiling and period have
  sane, low defaults (documented in `config.example.yaml`), so the budget is
  never off by omission — an operator who configures nothing still runs
  cost-capped. An explicit non-positive ceiling or period is refused at
  construction, so cost-safety cannot be disabled by setting it to zero.

### The rate limit defers to it

The ACL gate (ADR 0003) consults the global meter as the outermost cost check: a
per-user allowance never overrides an exhausted global budget. The meter is the
single source of the "are we out of money" answer. The two compose — both must
pass — but do not merge: per-user fairness stays in the ACL rate counter, this
meter is the one global aggregate.

## Consequences

- Total model spend is bounded regardless of user count, retries, or restarts —
  the cost failure mode is closed before the model path is built.
- The bot is cost-capped out of the box (conservative default) and cannot be run
  uncapped; the operator raises the ceiling to their real budget.
- Persistence is behind a `Store` interface — the in-memory store serves tests,
  the bbolt store (ADR 0005) serves production — so swapping it is not an engine
  change.
- It is a backstop, not a billing ledger: token-granular, single global figure,
  and overspend bounded by the in-flight call concurrency at the moment the
  ceiling is crossed (one for serial calls) — a coarse backstop, not a precise
  cutoff, which is the right trade for a safety limiter. Currency conversion (a
  configured price multiply) and per-model pricing are deliberate Phase-1
  non-goals, layerable later behind the same meter.
