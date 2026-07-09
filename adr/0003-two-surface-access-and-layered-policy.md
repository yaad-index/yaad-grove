# ADR 0003: Two-surface access model with layered per-user policy

**Status:** Accepted (2026-07-09)

## Context
A community bot is reachable two ways — inside its community group, and by
direct message. These have different trust boundaries. A group's membership is
itself a boundary; a DM inbox is unbounded and anyone with the bot's handle can
try. They must not have the same access.

## Decision
Two surfaces, different reach:

- **Group.** Members of a group the bot is enabled in may talk to it; group
  membership is the boundary. Still consent-gated (ADR 0002) and rate-limited
  per user.
- **DM.** Served only to users an **admin has explicitly approved**. Everyone
  else who DMs gets a refusal. The DM allowlist is the extra gate the unbounded
  surface needs.

The gates **stack**: a DM user needs *admin-approval → consent → answered*; a
group user needs *in-the-group → consent → answered*.

Policy resolves in layers: **per-user override > named tier > instance
default**. Tiers are defined once and assigned by label — `unlimited`,
`trusted`, `default`, `throttled` (below default, for abusers), `admin`. Rate
limiting is per-user keyed on the platform user-id, with a global daily spend
ceiling as the real backstop; over-limit gets a polite "try again shortly," not
a silent drop.

Admins configure all of this **live by talking to the bot** — approve/ban a DM
user, set a tier, reset a limit, reload the vault, toggle the bot. The per-user
store survives restarts; instance config sets only defaults and who the admins
are.

## Consequences
- The unbounded surface (DM) is closed by default; the bounded one (group) is
  open to its members.
- Abuse has a graceful lever (throttled tier) short of a ban, and a hard
  backstop (global spend ceiling).
- Live admin control means no config redeploy to manage people; the store is the
  source of truth for per-user state.
