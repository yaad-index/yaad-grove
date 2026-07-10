package acl

import (
	"context"
	"errors"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// ErrUnknownTier is returned when a tier that is not one of the defined tiers is
// assigned — a misconfiguration the store must never persist.
var ErrUnknownTier = errors.New("acl: unknown tier")

// tierAuthority ranks tiers by the authority they carry — the power to run
// privileged actions (ADR 0009). A tier absent from this map ranks at 0 (least
// authority), so an unrecognized assignment can never clear a real requirement.
//
// INTERIM MECHANISM — read before adding a verb: this single rank overloads two
// different axes. throttled/default/trusted/unlimited are *rate* (quota) tiers;
// admin is *authority* (privilege). They line up here only because admin sits at
// the top, so admin-gated verbs are safe. The rule that keeps it safe:
// **privileged verbs use MinTier=admin, full stop** — never a mid rate-tier,
// which would grant action-authority from a mere quota grant. If finer admin
// granularity is ever needed, add a separate authority/role axis rather than
// leaning on the rate tiers.
var tierAuthority = map[Tier]int{
	TierThrottled: 0,
	TierDefault:   1,
	TierTrusted:   2,
	TierUnlimited: 3,
	TierAdmin:     4,
}

// ValidTier reports whether t is a known tier — the validation an admin verb runs
// before assigning a tier to a user.
func ValidTier(t Tier) bool {
	_, ok := tierAuthority[t]
	return ok
}

// AtLeast reports whether have carries at least the authority of want. An
// unknown requirement (want not a real tier) is never satisfied — a misconfigured
// MinTier fails closed rather than authorizing everyone.
func AtLeast(have, want Tier) bool {
	w, ok := tierAuthority[want]
	if !ok {
		return false
	}
	return tierAuthority[have] >= w
}

// Authorize reports whether user currently carries at least minTier of authority
// (ADR 0009). It reads the tier FRESH from the store at call time — the
// load-bearing rule for interactive actions: a button click is re-authorized
// against the user's authority *now*, never the tier as it was when the button
// was rendered, so a demotion between render and click denies. A store error
// fails closed (false, err); a resolved token is necessary but not sufficient.
func (g *Gate) Authorize(ctx context.Context, user core.User, minTier Tier) (bool, error) {
	rec, err := g.store.Get(ctx, user.ID)
	if err != nil {
		return false, err
	}
	return AtLeast(g.resolveTier(rec), minTier), nil
}

// SetTier assigns tier to userID, creating the row if the user is unseen. It is
// the write half of the admin control plane (ADR 0009): a privileged verb calls
// it to change another user's authority. An unknown tier is refused so the store
// can never hold an unrankable authority. The read-modify-write runs in a single
// store transaction, so a concurrent update to the same record (say a consent
// change on the rate path) can't clobber the tier.
func (g *Gate) SetTier(ctx context.Context, userID string, tier Tier) error {
	if !ValidTier(tier) {
		return ErrUnknownTier
	}
	return g.store.Update(ctx, userID, func(r *Record) error {
		r.Tier = tier
		return nil
	})
}
