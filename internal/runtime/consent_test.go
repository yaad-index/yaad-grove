package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	assert.True(t, strings.HasSuffix(strings.TrimSpace(reply.Text), "`/consent remove`."), "tap instruction stays last")
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

// /consent remove withdraws the sender's own consent and records a decline,
// whatever their consent was before (ADR 0025), and confirms with how to opt back
// in.
func TestDMConsentSelfRemove(t *testing.T) {
	for _, before := range []acl.Consent{acl.ConsentGranted, acl.ConsentUnknown, acl.ConsentDeclined} {
		consent := &mockConsenter{consent: before}
		reply, err := consentHandler(consent)(context.Background(), dmInbound("/consent remove"))
		require.NoError(t, err)
		assert.Contains(t, reply.Text, "opted out")
		assert.Contains(t, reply.Text, "`/consent` to opt back in")
		assert.Equal(t, acl.ConsentDeclined, consent.consent, "from %d, a withdrawal records a decline", before)
	}
}

// A user who declined can always opt back in through the DM (ADR 0025): a DM
// shows them the disclosure and the opt-in button, and /consent grants.
func TestDMConsentDeclinedCanOptBackIn(t *testing.T) {
	consent := &mockConsenter{consent: acl.ConsentDeclined}
	reply, err := consentHandler(consent)(context.Background(), dmInbound("/start"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "opting in means")
	require.Len(t, reply.Actions, 1)
	assert.Equal(t, "consent_grant", reply.Actions[0].Verb)

	reply, err = consentHandler(consent)(context.Background(), dmInbound("/consent"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "opted in")
	assert.Equal(t, acl.ConsentGranted, consent.consent)
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

// A non-en Strings catalog on the Policy flows end to end through the handler: the
// consent disclosure, the opt-in button label, the group nudge, and the rate-limit
// reply all render from the catalog, not the embedded en fallback. This guards the
// wiring the strings_test.go isolation tests can't see — that a Policy carrying a
// language catalog actually localizes the whole consent/nudge path (ADR 0018 / #25),
// so a Policy that omits Strings (nil) doesn't silently answer in English.
func TestPolicyStringsLocalizesUserFacingPaths(t *testing.T) {
	// Sentinel "language": distinctive values for the keys these paths render, so an
	// en fallback would be unmistakable in the assertions below.
	cat := runtime.Strings{
		runtime.StrConsentDisclosureIntro: "INTRO_XX ",
		runtime.StrConsentDisclosureTap:   "TAP_XX",
		runtime.StrConsentOptInLabel:      "OPTIN_XX",
		runtime.StrNudge:                  "NUDGE_XX",
		runtime.StrRateLimited:            "RATELIMIT_XX",
	}

	// Consent disclosure + opt-in label render from the catalog.
	consentH := runtime.NewHandler(
		&mockGate{decision: acl.DecideServe}, &mockEngine{}, nil, nil, nil, nil,
		&mockConsenter{consent: acl.ConsentUnknown},
		runtime.Policy{Strings: cat},
	)
	reply, err := consentH(context.Background(), dmInbound("/start"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "INTRO_XX", "disclosure intro renders from the catalog, not en")
	assert.Contains(t, reply.Text, "TAP_XX", "tap line renders from the catalog")
	require.Len(t, reply.Actions, 1)
	assert.Equal(t, "OPTIN_XX", reply.Actions[0].Label, "opt-in label renders from the catalog")

	// The group nudge (message mode) and the rate-limit reply render from the
	// catalog too — the two other Policy.Strings-routed group paths.
	nudgeH := runtime.NewHandler(
		&mockGate{decision: acl.DecideNudge}, &mockEngine{}, nil, nil, nil, nil, nil,
		runtime.Policy{Nudge: runtime.Nudge{Mode: runtime.NudgeMessage}, Strings: cat},
	)
	nudge, err := nudgeH(context.Background(), inbound)
	require.NoError(t, err)
	assert.Contains(t, nudge.Text, "NUDGE_XX", "the nudge renders from the catalog")

	limitH := runtime.NewHandler(
		&mockGate{decision: acl.DecideRateLimited}, &mockEngine{}, nil, nil, nil, nil, nil,
		runtime.Policy{Strings: cat},
	)
	limited, err := limitH(context.Background(), inbound)
	require.NoError(t, err)
	assert.Contains(t, limited.Text, "RATELIMIT_XX", "the rate-limit reply renders from the catalog")
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
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate, nil), gate, nil, nil, runtime.Policy{})

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, reply.Notice, "Done")
	assert.Contains(t, reply.Text, "opted in")

	c, err := gate.ConsentOf(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, acl.ConsentGranted, c, "the button grants the clicker's own consent")
}

// fakeEraser records whom it erases, and the consent each had at the time.
type fakeEraser struct {
	consent   *mockConsenter
	erased    []string
	consentAt []acl.Consent
	results   []runtime.EraseResult
}

func (f *fakeEraser) Withdrawals(string) uint64 { return 0 }

func (f *fakeEraser) Erase(_ context.Context, user string) []runtime.EraseResult {
	f.erased = append(f.erased, user)
	f.consentAt = append(f.consentAt, f.consent.consent)
	return f.results
}

func withdrawWith(t *testing.T, policy runtime.Policy, consent *mockConsenter) core.Reply {
	t.Helper()
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{}, nil, nil, nil, nil, consent, policy)
	reply, err := h(context.Background(), dmInbound("/consent remove"))
	require.NoError(t, err)
	return reply
}

// With long-term memory, /consent remove erases the user's memory once their
// consent is off, logs the withdrawal with the user, and says the memory is
// erased; if any namespace was not erased it says so, and how to retry.
func TestDMConsentRemoveErasesMemory(t *testing.T) {
	log := captureLog(t)
	consent := &mockConsenter{consent: acl.ConsentGranted}
	eraser := &fakeEraser{consent: consent, results: []runtime.EraseResult{{Namespace: "inst-a"}, {Namespace: "club"}}}
	reply := withdrawWith(t, runtime.Policy{Erase: eraser}, consent)
	assert.Equal(t, []string{"u1"}, eraser.erased)
	assert.Equal(t, []acl.Consent{acl.ConsentDeclined}, eraser.consentAt, "consent is off before anything is erased")
	assert.Contains(t, reply.Text, "opted out")
	assert.Contains(t, reply.Text, "long-term memory is erased")
	assert.Contains(t, log.String(), `msg="consent withdrawn" user=u1`)

	consent = &mockConsenter{consent: acl.ConsentGranted}
	eraser = &fakeEraser{consent: consent, results: []runtime.EraseResult{{Namespace: "inst-a"}, {Namespace: "club", Err: errors.New("down")}}}
	reply = withdrawWith(t, runtime.Policy{Erase: eraser}, consent)
	assert.Contains(t, reply.Text, "opted out")
	assert.Contains(t, reply.Text, "could not erase all of your long-term memory")
	assert.Contains(t, reply.Text, "send `/consent remove` again")
	assert.NotContains(t, reply.Text, "is erased", "a partial erase is never reported as done")
	assert.Equal(t, acl.ConsentDeclined, consent.consent, "the withdrawal itself stands")
}

// Without long-term memory, /consent remove says nothing about it.
func TestDMConsentRemoveWithoutMemory(t *testing.T) {
	reply := withdrawWith(t, runtime.Policy{}, &mockConsenter{consent: acl.ConsentGranted})
	assert.Equal(t, "You're opted out — I no longer log your messages or answer you. Send `/consent` to opt back in anytime.", reply.Text)
}

// With long-term memory the disclosure says that memory is kept and erased on
// withdrawal, and, when the service derives, that conclusions are drawn; the
// tap instruction still reads last. Without it, neither line appears.
func TestDMConsentDisclosureMemoryLines(t *testing.T) {
	disclose := func(p runtime.Policy) string {
		h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{}, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentUnknown}, p)
		reply, err := h(context.Background(), dmInbound("/start"))
		require.NoError(t, err)
		return reply.Text
	}
	base := disclose(runtime.Policy{})
	assert.NotContains(t, base, "long-term memory")
	assert.NotContains(t, base, "conclusions")

	mem := disclose(runtime.Policy{Erase: &fakeEraser{}})
	assert.Contains(t, mem, "kept in a long-term memory")
	assert.Contains(t, mem, "Withdrawing erases it")
	assert.NotContains(t, mem, "conclusions")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(mem), "`/consent remove`."), "tap instruction stays last")

	derive := disclose(runtime.Policy{Erase: &fakeEraser{}, MemoryDerive: true})
	assert.Contains(t, derive, "draws conclusions about you")
	assert.Less(t, strings.Index(derive, "long-term memory"), strings.Index(derive, "draws conclusions"))
	assert.True(t, strings.HasSuffix(strings.TrimSpace(derive), "`/consent remove`."))
	assert.NotContains(t, disclose(runtime.Policy{MemoryDerive: true}), "conclusions", "the derive line needs memory on")
}

// captureLog sends the default logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
