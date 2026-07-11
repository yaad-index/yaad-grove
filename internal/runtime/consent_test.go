package runtime_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transcript"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// mockConsenter records grants and reports a canned consent state.
type mockConsenter struct {
	consent acl.Consent
	granted []string
}

func (m *mockConsenter) ConsentOf(context.Context, string) (acl.Consent, error) {
	return m.consent, nil
}

func (m *mockConsenter) SetConsent(_ context.Context, userID string, c acl.Consent) error {
	m.consent = c
	if c == acl.ConsentGranted {
		m.granted = append(m.granted, userID)
	}
	return nil
}

func dmInbound(text string) transport.Inbound {
	return transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceDM, Text: text}
}

func consentHandler(consent runtimeConsenter) transport.Handler {
	return runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "ANSWER"}}, nil, nil, nil, nil, consent, runtime.Policy{})
}

// runtimeConsenter matches the consenter the handler takes.
type runtimeConsenter interface {
	ConsentOf(context.Context, string) (acl.Consent, error)
	SetConsent(context.Context, string, acl.Consent) error
}

// An unconsented DM (/start) gets the disclosure + an opt-in button; the
// disclosure names both what opting in covers (answering) AND that group messages
// are logged.
func TestDMConsentUnconsentedPresentsOptIn(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentUnknown}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("/start"))
	require.NoError(t, err)

	assert.Contains(t, reply.Text, "opting in means")
	assert.Contains(t, reply.Text, "answer you", "discloses answering")
	assert.Contains(t, reply.Text, "knowledge base", "discloses logging")
	require.Len(t, reply.Actions, 1)
	assert.Equal(t, "consent_grant", reply.Actions[0].Verb)
}

// When a transcript is active, the disclosure adds the durable-record line so
// opt-in is informed that past entries persist after withdrawal (ADR 0015); with
// no transcript it stays the base wording.
func TestDMConsentDisclosureTranscriptLine(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentUnknown}

	// Base (no transcript): no persistence line.
	base, err := consentHandler(consent)(context.Background(), dmInbound("/start"))
	require.NoError(t, err)
	assert.NotContains(t, base.Text, "lasting conversation record")

	// Transcript active: the persistence line appears, and the tap instruction still
	// reads last.
	withT := runtime.NewHandler(
		&mockGate{decision: acl.DecideServe}, &mockEngine{}, nil, nil, nil, nil,
		&mockConsenter{consent: acl.ConsentUnknown},
		runtime.Policy{Transcript: &transcript.MemoryLog{}},
	)
	reply, err := withT(context.Background(), dmInbound("/start"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "lasting conversation record", "discloses the durable record")
	assert.Contains(t, reply.Text, "earlier ones stay", "discloses prospective withdrawal")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(reply.Text), "/consent remove."), "tap instruction stays last")
	require.Len(t, reply.Actions, 1)
}

// A bare non-command DM is an implicit /start — it offers the opt-in, never falls
// through to silence.
func TestDMBareMessageIsImplicitStart(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentUnknown}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("hey"))
	require.NoError(t, err)
	require.Len(t, reply.Actions, 1, "a bare DM offers the opt-in")
	assert.Equal(t, "consent_grant", reply.Actions[0].Verb)
}

// An already-consented DM gets status + the withdraw hint, and NO re-grant button.
func TestDMConsentAlreadyConsented(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentGranted}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("/start"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "already opted in")
	assert.Contains(t, reply.Text, "/consent remove")
	assert.Empty(t, reply.Actions, "no re-grant button when already consented")
}

// /consent is the text-backup grant: it opts the user in and confirms with the
// withdraw hint.
func TestDMConsentTextGrant(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentUnknown}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("/consent"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "opted in")
	assert.Contains(t, reply.Text, "/consent remove")
	assert.Equal(t, []string{"u1"}, consent.granted, "/consent grants the user's consent")
}

// /consent remove withdraws the sender's own consent (→ ConsentUnknown, re-join
// possible) and confirms with how to opt back in.
func TestDMConsentSelfRemove(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentGranted}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("/consent remove"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "opted out")
	assert.Contains(t, reply.Text, "/consent to opt back in")
	assert.Equal(t, acl.ConsentUnknown, consent.consent, "consent is withdrawn to unknown")
}

// A DM never reaches the engine — the non-admin DM surface is consent-only (ADR
// 0012), even for a message that looks like a query.
func TestDMNeverAnswers(t *testing.T) {
	engine := &mockEngine{reply: core.Reply{Text: "ANSWER"}}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentGranted}, runtime.Policy{})
	reply, err := h(context.Background(), dmInbound("what is the meaning of X?"))
	require.NoError(t, err)
	assert.False(t, engine.called, "a DM is consent-only, never answered")
	assert.NotEqual(t, "ANSWER", reply.Text)
}

// The opt-in button end to end: tapping it runs the consent_grant verb, which
// grants the clicker's own consent through the real gate.
func TestConsentGrantViaButton(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)

	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "consent_grant"})
	// gate is the authorizer + the consenter; no consenter for the message path here.
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate), gate, nil, nil, runtime.Policy{})

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, reply.Notice, "Done")
	assert.Contains(t, reply.Text, "opted in")

	c, err := gate.ConsentOf(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, acl.ConsentGranted, c, "the button grants the clicker's own consent")
}
