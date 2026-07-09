// Package acl is access control and consent: the gates that decide whether a
// query is served at all, and the persistent per-user state behind them.
//
// The model, decided for Phase 1 (ADR 0001):
//
//   - Consent is a HARD gate on the whole interaction. With no consent the
//     bot's only response is a consent prompt — it does not answer the query
//     and it records nothing the user said. Message content is never persisted
//     without consent; only the minimal ACL row (id + consent flag + rate
//     counter) is kept, so the bot can remember the state and throttle the
//     reminder.
//   - Two surfaces, different reach. A group member may talk to the bot
//     (membership is the boundary). A DM is served only if an admin has
//     explicitly approved that user; everyone else DMing gets a refusal.
//   - Layered policy: per-user override > named tier > instance default.
//   - Admins configure all of this live by talking to the bot; the store
//     survives restarts.
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
	// DMApproved is the admin allowlist flag for direct messages. Irrelevant in
	// groups (membership is the boundary there).
	DMApproved bool
	// RateCount / RateWindowStart back a simple per-user rate limit; the global
	// spend ceiling is the real backstop and lives in the runtime.
	RateCount       int
	RateWindowStart time.Time
	LastPromptedAt  time.Time // throttles the consent reminder
}

// Store persists Record state and survives restarts. Phase 1 intends a bbolt
// implementation (single-file, embedded), matching the fleet's house choice.
type Store interface {
	Get(ctx context.Context, userID string) (Record, error)
	Put(ctx context.Context, r Record) error
}

// Decision is the outcome of the gate for one query.
type Decision int

const (
	// DecideServe: all gates pass — answer the query (and log, since serving
	// implies consent granted).
	DecideServe Decision = iota
	// DecideAskConsent: no consent yet — reply only with the consent prompt,
	// record nothing the user said.
	DecideAskConsent
	// DecideRefuse: not permitted here (e.g. an unapproved DM) — refuse.
	DecideRefuse
	// DecideRateLimited: over the user's limit — a polite "try again shortly".
	DecideRateLimited
	// DecideSilent: no consent, and the consent prompt is within its throttle
	// window — reply nothing at all (ADR 0007). The gate owns the throttle, so the
	// transport just renders this as no reply.
	DecideSilent
)

// Gate stacks the access checks in order: surface reach (group membership vs DM
// admin-approval) -> consent -> rate limit -> serve (ADR 0007). It is the single
// place that decides whether the engine ever sees a query.
type Gate struct {
	store   Store
	defTier Tier
}

// NewGate returns a Gate over store with the instance's default tier.
func NewGate(store Store, defaultTier Tier) *Gate {
	return &Gate{store: store, defTier: defaultTier}
}

// Access-control windows (ADR 0002/0003). Phase-1 defaults; the per-tier
// allowances tune the rate limit.
const (
	// consentPromptThrottle is the minimum gap between consent reminders to an
	// unconsented user; within it the gate stays silent (ADR 0007).
	consentPromptThrottle = time.Hour
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

// Check resolves the layered policy for a user on a given surface and returns the
// decision, in order surface-reach -> consent -> rate-limit -> serve (ADR 0007).
//
// It deliberately takes only the user and surface, never the message text: the
// gate never needs content to decide, so ADR 0002's "record nothing the user
// said" guarantee is enforced at the type level — the gate literally cannot see,
// let alone retain, what the user wrote. Consent gates before the rate limit so
// an unconsented user is never counted (they are bounded by the consent-prompt
// throttle instead), extending that guarantee to the rate path. A store error at
// any step fails closed (DecideRefuse) — never serve on an unknown state.
func (g *Gate) Check(ctx context.Context, user core.User, surface core.Surface) (Decision, error) {
	rec, err := g.store.Get(ctx, user.ID)
	if err != nil {
		return DecideRefuse, err
	}

	// Surface reach (ADR 0003): a DM is served only to an admin-approved user; a
	// group message is already membership-bounded upstream by the transport.
	if surface == core.SurfaceDM && !rec.DMApproved {
		return DecideRefuse, nil
	}

	// Consent hard gate (ADR 0002): anything but ConsentGranted is unconsented
	// (unknown and declined collapse to the same "keep prompting" state). The only
	// response is the consent prompt, throttled; nothing the user said is recorded.
	if rec.Consent != ConsentGranted {
		now := time.Now()
		if !rec.LastPromptedAt.IsZero() && now.Sub(rec.LastPromptedAt) < consentPromptThrottle {
			return DecideSilent, nil // within the throttle window — say nothing
		}
		rec.LastPromptedAt = now
		if err := g.store.Put(ctx, rec); err != nil {
			return DecideRefuse, err
		}
		return DecideAskConsent, nil
	}

	// Rate limit (ADR 0003) — consented users only, per-user fairness. The global
	// spend ceiling (ADR 0006) is the cost backstop, checked on the model-call
	// path, not here.
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
