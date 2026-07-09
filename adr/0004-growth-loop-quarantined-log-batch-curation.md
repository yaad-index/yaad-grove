# ADR 0004: Growth loop — quarantined logging, admin-triggered batch curation

**Status:** Accepted (2026-07-09)

## Context
The knowledge base should get richer over time from what the community says —
but never at the cost of the "always in bounds" guarantee. If unreviewed chatter
could reach the answering path, the bot would no longer be grounded on a curated
base.

## Decision
Isolation on the data side mirrors the grounding guarantee on the answering
side:

- **Consent-gated logging to a quarantined store.** Consented community messages
  (ADR 0002) are logged to a store that lives **outside the answering vault**.
  The answering bot never reads it; it only ever grounds on the curated vault.
- **Curation is admin-triggered and batch**, never an always-on daemon. A run
  reads the logged backlog, diffs it against the vault, and proposes a coherent
  batch of **typed actions** — e.g. "fetch external data for X into the vault"
  (tool-backed enrichment) or "append user N's review to X's page" (attributed
  content edit).
- **Every accepted edit is a git commit.** The vault is markdown + git:
  versioned, attributable, revertible, with a free audit trail.
- **Admin review is a hard step.** No community input reaches the vault unseen:
  comment → proposal → admin approves → git commit → the answering bot is now
  grounded on it.

**Phasing.** Phase 1 ships the answering bot **and** the consent-gated logging,
so Phase-2 curation has data to work from. Phase 2 defers the overseer: rather
than build bespoke curation code now, an agent (e.g. Claude Code) runs against
the quarantined log and the vault, proposing and committing edits with the admin
in the loop. The typed-actions/review-queue all happen naturally because the
agent edits the git-backed vault. No overseer code in the engine for now.

## Consequences
- The answering path can never be poisoned by raw chatter; only curated,
  reviewed, committed knowledge grounds answers.
- Logging must exist from Phase 1 even though curation is deferred — otherwise
  Phase 2 starts with no history.
- Attribution and rollback come for free from git; curation quality is bounded
  by admin review, not by a model.
