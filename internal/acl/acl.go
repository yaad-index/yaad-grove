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
)

// Gate stacks the access checks in order: surface reach (group membership vs DM
// admin-approval) -> rate limit -> consent -> serve. It is the single place
// that decides whether the engine ever sees a query.
type Gate struct {
	store   Store
	defTier Tier
}

// NewGate returns a Gate over store with the instance's default tier.
func NewGate(store Store, defaultTier Tier) *Gate {
	return &Gate{store: store, defTier: defaultTier}
}

// Check resolves the layered policy for q.User on q.Surface and returns the
// decision. It never lets an unconsented user's content through.
//
// Scaffold: structure only. The order is fixed above; each step reads/writes
// the Record via the Store.
func (g *Gate) Check(ctx context.Context, q core.Query) (Decision, error) {
	return DecideRefuse, core.ErrNotImplemented
}
