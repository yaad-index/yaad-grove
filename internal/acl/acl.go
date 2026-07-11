// Package acl is access control and consent: the gates that decide whether a
// query is served at all, and the persistent per-user state behind them.
//
// The model (ADR 0012, amending 0002/0003/0007):
//
//   - Consent is the gate for BOTH being answered and being logged. It is
//     granted explicitly and privately — via the DM opt-in button or the
//     `/consent` command — never inferred from group messages. The store keeps
//     only the minimal ACL row (id + consent flag + tier + rate counter); no
//     message content is persisted without consent.
//   - This gate decides a GROUP message by whether it is directed at the bot: a
//     directed message from a consented user is answered (and logged), a
//     directed message from an unconsented user draws a consent nudge, ambient
//     chatter from a consented user is logged silently, and ambient chatter from
//     an unconsented user is ignored. Only directed messages ever draw a reply,
//     so a nudge cannot flood the group.
//   - The DM surface is routed by the runtime, not here: an admin DM is answered
//     by the engine, a non-admin DM is consent management only. Admins are a
//     config allowlist (a DM-surface privilege), so this gate never sees them.
//   - Layered policy: per-user override > named tier > instance default.
//   - The store survives restarts, so consent and tier assignments are durable.
package acl

import (
	"context"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Tier is a named policy bucket. Tiers are defined once and assigned by label.
type Tier string

const (
	TierUnlimited Tier = "unlimited"
	TierTrusted   Tier = "trusted"
	TierDefault   Tier = "default"
	TierThrottled Tier = "throttled" // below default, for abusers
	TierAdmin     Tier = "admin"
)

// Consent is a user's recorded answer to the consent prompt. The zero value is
// ConsentUnknown: not yet answered, treated as "no consent" (answer nothing,
// keep prompting), never as an implied yes.
type Consent int

const (
	ConsentUnknown  Consent = iota // never answered — default, no consent
	ConsentGranted                 // opted in; answering + logging enabled
	ConsentDeclined                // explicitly declined
)

// Record is the minimal persistent per-user row. It deliberately holds no
// message content — only what access control needs to function.
type Record struct {
	UserID  string
	Tier    Tier
	Consent Consent
	// RateCount / RateWindowStart back a simple per-user rate limit; the global
	// spend ceiling is the real backstop and lives in the runtime.
	RateCount       int
	RateWindowStart time.Time
}

// Store persists Record state and survives restarts. Phase 1 intends a bbolt
// implementation (single-file, embedded), the house store choice (ADR 0005).
type Store interface {
	Get(ctx context.Context, userID string) (Record, error)
	Put(ctx context.Context, r Record) error
	// Update atomically reads the user's record, applies mutate, and writes it
	// back within a single transaction, so concurrent writers to the same record
	// cannot clobber one another. A first-seen user's record is the zero Record
	// with UserID set. If mutate returns an error, the record is left unchanged.
	Update(ctx context.Context, userID string, mutate func(*Record) error) error
}

// Decision is the outcome of the gate for one query.
type Decision int

const (
	// DecideServe: consented and directed at the bot — answer the query (and log
	// it, since serving implies consent).
	DecideServe Decision = iota
	// DecideLogOnly: consented ambient chatter — log it to the growth corpus with
	// no reply (ADR 0004/0012). Not answered, not rate-counted.
	DecideLogOnly
	// DecideNudge: unconsented but directed at the bot — deliver the configurable
	// consent nudge (ADR 0012), never an answer, and log nothing.
	DecideNudge
	// DecideRefuse: not permitted — refuse. The fail-closed outcome on a store
	// error.
	DecideRefuse
	// DecideRateLimited: consented and directed but over the user's rate allowance
	// — a polite "try again shortly".
	DecideRateLimited
	// DecideSilent: unconsented ambient chatter — reply nothing and log nothing
	// (ADR 0012). The transport renders this as no reply.
	DecideSilent
)

// Gate decides a group message from a user's consent and the message's
// directedness: consent -> (directed) rate limit -> serve, or log / nudge /
// ignore (ADR 0012). It is the single place that decides whether the engine ever
// sees a group query. DM routing is the runtime's job (admin answering vs
// consent management), so the gate never handles a DM.
type Gate struct {
	store   Store
	defTier Tier
}

// NewGate returns a Gate over store with the instance's default tier.
func NewGate(store Store, defaultTier Tier) *Gate {
	return &Gate{store: store, defTier: defaultTier}
}

// Access-control windows (ADR 0003). The per-tier allowances tune the rate
// limit.
const (
	// rateWindow is the per-user rate-limit window.
	rateWindow = time.Hour
	// unlimitedRate marks a tier with no per-user rate cap.
	unlimitedRate = -1
)

// tierAllowance is each tier's query allowance per rateWindow (ADR 0003 layered
// policy); unlimitedRate means no cap. An unknown tier falls back to the default.
var tierAllowance = map[Tier]int{
	TierUnlimited: unlimitedRate,
	TierAdmin:     unlimitedRate,
	TierTrusted:   120,
	TierDefault:   30,
	TierThrottled: 5,
}

func allowanceFor(t Tier) int {
	if a, ok := tierAllowance[t]; ok {
		return a
	}
	return tierAllowance[TierDefault]
}

// resolveTier is the layered tier policy (ADR 0003): a per-user tier override on
// the Record, else the instance default.
func (g *Gate) resolveTier(rec Record) Tier {
	if rec.Tier != "" {
		return rec.Tier
	}
	return g.defTier
}

// GateInput is a group message's gate query: who sent it and whether it is
// directed at the bot (a reply-to-bot or an @mention) vs ambient chatter (ADR
// 0012). It carries no message text — the gate never needs content to decide, so
// ADR 0002's "record nothing the user said" guarantee holds at the type level.
// It is a struct so a later dimension can be added without breaking callers.
type GateInput struct {
	User     core.User
	Surface  core.Surface
	Directed bool
}

// Check decides a group message (ADR 0012). Consent gates first: an unconsented
// user directing a message at the bot draws a nudge, their ambient chatter is
// ignored — neither is logged, and nothing they said is recorded. A consented
// user's ambient chatter is logged (DecideLogOnly, no reply, not rate-counted);
// a consented directed message is rate-limited and, under the allowance, served.
//
// Directedness only changes the reply, never whether a consented message is
// logged — the runtime logs on both DecideServe and DecideLogOnly, so every
// consented group message reaches the growth corpus (ADR 0004), matching the DM
// disclosure. A store error fails closed (DecideRefuse) — never serve on an
// unknown state.
func (g *Gate) Check(ctx context.Context, in GateInput) (Decision, error) {
	rec, err := g.store.Get(ctx, in.User.ID)
	if err != nil {
		return DecideRefuse, err
	}

	// Consent gate (ADR 0012): consent is granted only via the DM flow, never
	// inferred here. An unconsented user is nudged when they direct a message at
	// the bot, and ignored otherwise; only a directed message draws a reply, so a
	// nudge cannot flood the group. Nothing unconsented is recorded.
	if rec.Consent != ConsentGranted {
		if in.Directed {
			return DecideNudge, nil
		}
		return DecideSilent, nil
	}

	// Consented ambient chatter is logged with no reply (ADR 0004/0012); it is not
	// rate-counted because nothing is answered.
	if !in.Directed {
		return DecideLogOnly, nil
	}

	// Consented + directed: an answer. Rate limit for per-user fairness (ADR
	// 0003); the global spend ceiling (ADR 0006) is the cost backstop, on the
	// model-call path, not here.
	allow := allowanceFor(g.resolveTier(rec))
	now := time.Now()
	if rec.RateWindowStart.IsZero() || now.Sub(rec.RateWindowStart) >= rateWindow {
		rec.RateWindowStart, rec.RateCount = now, 0
	}
	if allow != unlimitedRate && rec.RateCount >= allow {
		return DecideRateLimited, nil
	}
	rec.RateCount++
	if err := g.store.Put(ctx, rec); err != nil {
		return DecideRefuse, err
	}
	return DecideServe, nil
}
