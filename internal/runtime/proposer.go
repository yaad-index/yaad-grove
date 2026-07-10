package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// Proposal is a suggested action offered to a user for one-tap approval (ADR
// 0010). Action is what runs on approve; Prompt is the human-readable context
// shown with the buttons. A proposal is the overseer's *suggestion* — its intent
// is the proposer's, not the approver's, which is why a privileged proposal must
// present its concrete effect for informed consent before it can be approved.
type Proposal struct {
	Prompt string
	Action core.Action
}

// Proposer emits proposals. Real, event-driven overseers come later; T4 ships one
// worked example (StaticProposer) that proves the loop end to end.
type Proposer interface {
	Propose(ctx context.Context) ([]Proposal, error)
}

// StaticProposer emits a fixed set of proposals — the worked example. It stands
// in for an overseer that would derive proposals from events or vault state.
type StaticProposer struct{ proposals []Proposal }

// NewStaticProposer returns a proposer that always emits proposals.
func NewStaticProposer(proposals ...Proposal) *StaticProposer {
	return &StaticProposer{proposals: proposals}
}

// Propose returns the fixed proposals.
func (p *StaticProposer) Propose(context.Context) ([]Proposal, error) {
	return p.proposals, nil
}

// Proposal-render refusals (ADR 0010). Each keeps an ungrounded or unsafe
// proposal from ever being offered.
var (
	// ErrUnknownProposedVerb: the proposed verb is not in the registry.
	ErrUnknownProposedVerb = errors.New("runtime: proposed verb is not registered")
	// ErrProposalNotDescribable: a privileged verb with no Describe cannot present
	// its effect for consent, so it cannot be proposed.
	ErrProposalNotDescribable = errors.New("runtime: privileged proposal has no effect description")
	// ErrProposalNotPrivate: a privileged proposal must be offered in a DM, where a
	// single recipient's authority is checkable and no admin affordance leaks to a
	// group.
	ErrProposalNotPrivate = errors.New("runtime: privileged proposal outside a direct message")
	// ErrProposalRecipientCannotApprove: the recipient can't currently approve the
	// proposal, so it isn't offered (defense-in-depth; re-auth at click remains the
	// boundary).
	ErrProposalRecipientCannotApprove = errors.New("runtime: proposal recipient cannot approve it")
)

const dismissLabel = "Dismiss"

// RenderProposal turns a Proposal into a reply offering approve/dismiss buttons,
// enforcing the informed-consent contract (ADR 0010). The order is deliberate —
// **validate params → describe → build actions** — so Describe (and later
// Execute) only ever see validated params; a malformed proposal is refused before
// any label is generated.
//
// For a PRIVILEGED verb (MinTier admin) it refuses unless three conditions hold:
// the verb can Describe its effect (so the approve label is registry-derived and
// cannot misstate the action), the surface is a DM (a single, tier-checkable
// recipient), and the recipient can currently approve it. The approve label is
// then the described effect — the canonical consent surface. approve routes into
// the existing execute path unchanged; dismiss is unprivileged.
//
// It does not mint tokens: it returns a Reply whose Actions the transport renders
// (minting a callback token per button on Send), so a tap flows through the T3
// resolve → re-authorize → execute path with no new authority here.
func (r *Registry) RenderProposal(ctx context.Context, p Proposal, recipient core.User, surface core.Surface, authz authorizer) (core.Reply, error) {
	verb, ok := r.Lookup(p.Action.Verb)
	if !ok {
		return core.Reply{}, ErrUnknownProposedVerb
	}
	if verb.Validate != nil {
		if err := verb.Validate(p.Action.Params); err != nil {
			return core.Reply{}, fmt.Errorf("runtime: invalid proposed params: %w", err)
		}
	}

	approve := p.Action
	approve.Label = "Approve"

	if isPrivileged(verb.MinTier) {
		if verb.Describe == nil {
			return core.Reply{}, ErrProposalNotDescribable
		}
		if surface != core.SurfaceDM {
			return core.Reply{}, ErrProposalNotPrivate
		}
		allowed, err := authz.Authorize(ctx, recipient, verb.MinTier)
		if err != nil {
			return core.Reply{}, err
		}
		if !allowed {
			return core.Reply{}, ErrProposalRecipientCannotApprove
		}
		approve.Label = verb.Describe(p.Action.Params) // the consent surface
	} else if verb.Describe != nil {
		approve.Label = verb.Describe(p.Action.Params)
	}

	dismiss := core.Action{Verb: verbDismiss, Label: dismissLabel}
	return core.Reply{Text: p.Prompt, Actions: []core.Action{approve, dismiss}}, nil
}

// isPrivileged reports whether a verb's authority makes it a privileged action.
// By the ADR 0009 rule that privileged verbs use MinTier=admin, that is exactly
// the admin authority level.
func isPrivileged(minTier acl.Tier) bool {
	return acl.AtLeast(minTier, acl.TierAdmin)
}
