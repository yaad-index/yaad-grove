package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

const (
	verbEcho    = "echo"
	verbSetTier = "set_tier"
)

// tierSetter is the acl write the set_tier verb performs; *acl.Gate satisfies it.
type tierSetter interface {
	SetTier(ctx context.Context, userID string, tier acl.Tier) error
}

// EchoVerb is the unprivileged baseline carried over from T2: it proves the
// action loop end to end with no side effect. TierThrottled is the authority
// floor, so any subject clears it — echo is intentionally public.
func EchoVerb() Verb {
	return Verb{
		MinTier: acl.TierThrottled,
		Execute: func(_ context.Context, _ core.User, _ map[string]string) (string, error) {
			return "", nil // no effect and no status line — the generic done toast suffices
		},
	}
}

// SetTierVerb is the first privileged verb (ADR 0009 T3): an admin sets another
// user's tier. It changes authority, so MinTier is admin — never a rate tier.
func SetTierVerb(setter tierSetter) Verb {
	return Verb{
		MinTier: acl.TierAdmin,
		Validate: func(p map[string]string) error {
			if strings.TrimSpace(p["user"]) == "" {
				return errors.New("set_tier: missing user")
			}
			if !acl.ValidTier(acl.Tier(p["tier"])) {
				return errors.New("set_tier: unknown tier")
			}
			return nil
		},
		Execute: func(ctx context.Context, _ core.User, p map[string]string) (string, error) {
			target, tier := p["user"], acl.Tier(p["tier"])
			if err := setter.SetTier(ctx, target, tier); err != nil {
				return "", err
			}
			return "Set " + target + " to " + string(tier) + ".", nil
		},
	}
}

// DefaultRegistry registers the Phase-1 verbs: the unprivileged echo baseline and
// the set_tier admin verb over setter (the acl gate).
func DefaultRegistry(setter tierSetter) *Registry {
	r := NewRegistry()
	r.Register(verbEcho, EchoVerb())
	r.Register(verbSetTier, SetTierVerb(setter))
	return r
}
