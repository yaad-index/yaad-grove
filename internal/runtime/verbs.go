package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

const (
	verbEcho         = "echo"
	verbSetTier      = "set_tier"
	verbDismiss      = "dismiss"
	verbConsentGrant = "consent_grant"
)

// tierSetter is the acl write the set_tier verb performs; *acl.Gate satisfies it.
type tierSetter interface {
	SetTier(ctx context.Context, userID string, tier acl.Tier) error
}

// consentSetter is the acl write the consent_grant verb performs; *acl.Gate
// satisfies it.
type consentSetter interface {
	SetConsent(ctx context.Context, userID string, c acl.Consent) error
}

// gateWriter is the acl write surface the default verb set needs; *acl.Gate
// satisfies it.
type gateWriter interface {
	tierSetter
	consentSetter
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
			return "Set " + target + " → " + string(tier) + ".", nil
		},
		// The consent surface for a proposed set_tier: the exact target and
		// destination tier, derived from the same params Execute will apply.
		Describe: func(p map[string]string) string {
			return "Set " + p["user"] + " → " + p["tier"]
		},
	}
}

// DismissVerb is the unprivileged dismiss action offered alongside a proposal
// (ADR 0010): it drops the proposal with no effect and edits the message to say
// so. TierThrottled is the authority floor, so anyone may dismiss.
func DismissVerb() Verb {
	return Verb{
		MinTier: acl.TierThrottled,
		Execute: func(context.Context, core.User, map[string]string) (string, error) {
			return "Dismissed.", nil
		},
	}
}

// ConsentGrantVerb is the self-service opt-in behind the DM opt-in button (ADR
// 0012). It grants the acting subject's OWN consent — no cross-user grant — so it
// is unprivileged (TierThrottled: anyone may opt themselves in). The confirmation
// names the withdraw path.
func ConsentGrantVerb(setter consentSetter) Verb {
	return Verb{
		MinTier: acl.TierThrottled,
		Execute: func(ctx context.Context, subject core.User, _ map[string]string) (string, error) {
			if err := setter.SetConsent(ctx, subject.ID, acl.ConsentGranted); err != nil {
				return "", err
			}
			return consentGrantedText, nil
		},
	}
}

// DefaultRegistry registers the Phase-1 verbs: the unprivileged echo/dismiss
// baselines, the self-service consent_grant, and the set_tier admin verb — all
// over the acl gate.
func DefaultRegistry(gate gateWriter) *Registry {
	r := NewRegistry()
	r.Register(verbEcho, EchoVerb())
	r.Register(verbDismiss, DismissVerb())
	r.Register(verbConsentGrant, ConsentGrantVerb(gate))
	r.Register(verbSetTier, SetTierVerb(gate))
	return r
}
