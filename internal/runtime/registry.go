package runtime

import (
	"context"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// Verb is a registered admin operation (ADR 0009): the authority it requires, a
// param validator, and the executor that performs the effect. A new admin
// capability is added by registering a verb — no transport change.
//
// MinTier is the authority to run the verb, re-checked against the acting
// subject's CURRENT tier at click time — a resolved token is never sufficient.
//
// Privileged verbs use MinTier=acl.TierAdmin, full stop. The rate tiers
// (trusted, unlimited) are quota grants, not authority grants; gating a
// privileged verb on one would grant action-authority from a mere rate grant.
// See acl.tierAuthority for why the rank is an interim mechanism.
type Verb struct {
	MinTier acl.Tier
	// Validate checks the params before execution; nil means no params to check.
	// It runs only after authorization, so an unauthorized clicker can't probe
	// valid param shapes from validation errors.
	Validate func(params map[string]string) error
	// Execute performs the effect for the acting subject with validated params and
	// returns a short status line for the message (empty = no status edit).
	Execute func(ctx context.Context, subject core.User, params map[string]string) (string, error)
	// Describe renders the verb's concrete effect for a set of validated params —
	// the human consent surface on a proposed action's approve button (ADR 0010).
	// Because it is registry-derived, the label cannot diverge from the effect
	// Execute will apply; a proposer can't misstate what a tap will do. A
	// PRIVILEGED verb without a Describe cannot be proposed (refused, fail-closed).
	// It receives only validated params — the proposal render path validates before
	// it describes.
	Describe func(params map[string]string) string
}

// Registry maps a verb name to its Verb. It is the single source of a verb's
// required tier and executor: both are read from code at execution, never from
// the stored action, so a token can carry a verb name but not its authority.
type Registry struct {
	verbs map[string]Verb
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{verbs: make(map[string]Verb)}
}

// Register adds (or replaces) a verb by name.
func (r *Registry) Register(name string, v Verb) {
	r.verbs[name] = v
}

// Lookup returns the verb registered under name, or ok=false if none is — the
// clean not-found path a callback to an unknown verb takes.
func (r *Registry) Lookup(name string) (Verb, bool) {
	v, ok := r.verbs[name]
	return v, ok
}
