# ADR 0000: Record architecture decisions

**Status:** Accepted (2026-07-09)

## Context
yaad-grove's design was worked out in discussion before any code. We want the
reasoning behind each significant decision to be durable and reviewable, not
lost in chat history — the same convention the sibling Go projects use.

## Decision
We use Architecture Decision Records. Each ADR is a numbered Markdown file in
`adr/` with Status, Context, Decision, Consequences. ADRs are immutable once
Accepted; a later ADR can supersede an earlier one.

## Consequences
Decisions are auditable and onboardable. Changing a decision is itself a
recorded, deliberate act.
