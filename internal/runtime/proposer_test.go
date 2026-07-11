package runtime_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// stubSetter satisfies the set_tier verb's writer without a real store.
type stubSetter struct {
	called  int
	gotUser string
	gotTier acl.Tier
}

func (s *stubSetter) SetTier(_ context.Context, user string, tier acl.Tier) error {
	s.called++
	s.gotUser, s.gotTier = user, tier
	return nil
}

func (s *stubSetter) SetConsent(context.Context, string, acl.Consent) error { return nil }

var promoteBob = runtime.Proposal{
	Prompt: "Promote bob?",
	Action: core.Action{Verb: "set_tier", Params: map[string]string{"user": "bob", "tier": "trusted"}},
}

func setTierRegistry() *runtime.Registry {
	return registryWith("set_tier", runtime.SetTierVerb(&stubSetter{}))
}

// A privileged proposal in a DM to a recipient who can approve renders with an
// effect-describing approve label (registry-derived) plus a dismiss button.
func TestRenderProposalPrivilegedHappy(t *testing.T) {
	authz := &mockAuthz{authorized: true}
	reply, err := setTierRegistry().RenderProposal(
		context.Background(), promoteBob, core.User{ID: "u1"}, core.SurfaceDM, authz)
	require.NoError(t, err)

	assert.Equal(t, "Promote bob?", reply.Text)
	require.Len(t, reply.Actions, 2)
	assert.Equal(t, "set_tier", reply.Actions[0].Verb)
	assert.Equal(t, "Set bob → trusted", reply.Actions[0].Label, "the label is the described effect, not 'Approve'")
	assert.Equal(t, "dismiss", reply.Actions[1].Verb)
	assert.Equal(t, acl.TierAdmin, authz.gotMinTier, "recipient checked against the verb's tier")
}

// A privileged verb with no Describe cannot present its effect, so it can't be
// proposed — fail closed.
func TestRenderProposalPrivilegedNoDescribe(t *testing.T) {
	bare := runtime.Verb{MinTier: acl.TierAdmin} // no Describe
	_, err := registryWith("set_tier", bare).RenderProposal(
		context.Background(), promoteBob, core.User{ID: "u1"}, core.SurfaceDM, &mockAuthz{authorized: true})
	assert.ErrorIs(t, err, runtime.ErrProposalNotDescribable)
}

// A privileged proposal outside a DM is refused (no single recipient, leaks an
// admin affordance).
func TestRenderProposalPrivilegedNotDM(t *testing.T) {
	_, err := setTierRegistry().RenderProposal(
		context.Background(), promoteBob, core.User{ID: "u1"}, core.SurfaceGroup, &mockAuthz{authorized: true})
	assert.ErrorIs(t, err, runtime.ErrProposalNotPrivate)
}

// A recipient who can't currently approve isn't offered the proposal.
func TestRenderProposalRecipientCannotApprove(t *testing.T) {
	_, err := setTierRegistry().RenderProposal(
		context.Background(), promoteBob, core.User{ID: "u1"}, core.SurfaceDM, &mockAuthz{authorized: false})
	assert.ErrorIs(t, err, runtime.ErrProposalRecipientCannotApprove)
}

// Invalid params are refused before any label is generated (validate precedes
// describe).
func TestRenderProposalInvalidParams(t *testing.T) {
	bad := runtime.Proposal{Action: core.Action{Verb: "set_tier", Params: map[string]string{"user": "bob", "tier": "bogus"}}}
	_, err := setTierRegistry().RenderProposal(
		context.Background(), bad, core.User{ID: "u1"}, core.SurfaceDM, &mockAuthz{authorized: true})
	require.Error(t, err)
	assert.NotErrorIs(t, err, runtime.ErrProposalNotDescribable, "the refusal is about params, before describe")
}

// An unregistered verb can't be proposed.
func TestRenderProposalUnknownVerb(t *testing.T) {
	ghost := runtime.Proposal{Action: core.Action{Verb: "ghost"}}
	_, err := runtime.NewRegistry().RenderProposal(
		context.Background(), ghost, core.User{ID: "u1"}, core.SurfaceDM, &mockAuthz{authorized: true})
	assert.ErrorIs(t, err, runtime.ErrUnknownProposedVerb)
}

// An unprivileged proposal renders anywhere (no DM restriction, no recipient
// check) with a default label.
func TestRenderProposalUnprivilegedAnywhere(t *testing.T) {
	authz := &mockAuthz{authorized: false} // must not be consulted
	reg := registryWith("echo", runtime.EchoVerb())
	reply, err := reg.RenderProposal(
		context.Background(), runtime.Proposal{Prompt: "ping?", Action: core.Action{Verb: "echo"}},
		core.User{ID: "u1"}, core.SurfaceGroup, authz)
	require.NoError(t, err)
	assert.Equal(t, "Approve", reply.Actions[0].Label)
	assert.False(t, authz.called, "an unprivileged proposal needs no recipient authority check")
}

// End to end: proposer → render → (mint the approve token as the transport would)
// → tap → the T3 re-authorize + execute path runs the privileged effect once.
func TestProposalApproveEndToEnd(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierAdmin)) // approver is admin

	registry := runtime.DefaultRegistry(gate)
	proposer := runtime.NewStaticProposer(promoteBob)
	proposals, err := proposer.Propose(ctx)
	require.NoError(t, err)
	require.Len(t, proposals, 1)

	reply, err := registry.RenderProposal(ctx, proposals[0], core.User{ID: "u1"}, core.SurfaceDM, gate)
	require.NoError(t, err)
	approve := reply.Actions[0]
	assert.Equal(t, "Set bob → trusted", approve.Label)

	// The transport would mint a token per rendered action; simulate that.
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, approve)

	h := runtime.NewHandler(nil, nil, store, registry, gate, nil, nil, runtime.Policy{})
	clickReply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, clickReply.Notice, "Done")
	assert.Contains(t, clickReply.Text, "Set bob → trusted")

	rec, err := aclStore.Get(ctx, "bob")
	require.NoError(t, err)
	assert.Equal(t, acl.TierTrusted, rec.Tier, "the proposed effect committed after an informed approve")
}

// Dismiss runs as an unprivileged verb and edits the message to say so.
func TestProposalDismiss(t *testing.T) {
	ctx := context.Background()
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "dismiss", Label: "Dismiss"})

	// dismiss is unprivileged (TierThrottled), which any subject clears.
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(&stubSetter{}), &mockAuthz{authorized: true}, nil, nil, runtime.Policy{})
	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, reply.Notice, "Done")
	assert.Equal(t, "Dismissed.", reply.Text)
}
