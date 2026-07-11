package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

const testTTL = time.Minute

type mockGate struct {
	decision   acl.Decision
	err        error
	called     bool
	gotUser    core.User
	gotSurface core.Surface
}

func (m *mockGate) Check(_ context.Context, user core.User, surface core.Surface) (acl.Decision, error) {
	m.called = true
	m.gotUser, m.gotSurface = user, surface
	return m.decision, m.err
}

type mockEngine struct {
	reply    core.Reply
	err      error
	called   bool
	gotQuery core.Query
}

func (m *mockEngine) Answer(_ context.Context, q core.Query) (core.Reply, error) {
	m.called = true
	m.gotQuery = q
	return m.reply, m.err
}

var inbound = transport.Inbound{
	User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "hello", ReplyTo: "chat-1",
}

// Each gate decision maps to the right reply; the gate is always checked first
// and the engine is reached only on DecideServe.
func TestHandlerDecisionMapping(t *testing.T) {
	cases := []struct {
		name        string
		decision    acl.Decision
		wantEngine  bool
		wantSilent  bool
		wantRefused bool
		wantTextSub string
	}{
		{"serve", acl.DecideServe, true, false, false, "engine answer"},
		{"ask-consent", acl.DecideAskConsent, false, false, false, "opting in"},
		{"rate-limited", acl.DecideRateLimited, false, false, false, "rate limit"},
		{"silent", acl.DecideSilent, false, true, false, ""},
		{"refuse", acl.DecideRefuse, false, false, true, "can't help"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &mockGate{decision: tc.decision}
			engine := &mockEngine{reply: core.Reply{Text: "engine answer"}}
			reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil)(context.Background(), inbound)
			require.NoError(t, err)

			assert.True(t, gate.called, "gate is always checked first")
			assert.Equal(t, inbound.User, gate.gotUser)
			assert.Equal(t, inbound.Surface, gate.gotSurface)
			assert.Equal(t, tc.wantEngine, engine.called, "engine only on serve")
			assert.Equal(t, tc.wantSilent, reply.Silent)
			assert.Equal(t, tc.wantRefused, reply.Refused)
			if tc.wantTextSub != "" {
				assert.Contains(t, reply.Text, tc.wantTextSub)
			}
			if tc.wantSilent {
				assert.Empty(t, reply.Text, "a silent reply carries no text")
			}
			if tc.wantEngine {
				// The inbound was normalized into the engine's Query.
				assert.Equal(t, inbound.User, engine.gotQuery.User)
				assert.Equal(t, inbound.Surface, engine.gotQuery.Surface)
				assert.Equal(t, inbound.Text, engine.gotQuery.Text)
			}
		})
	}
}

// A gate error fails closed: a refusal, and the engine is never called.
func TestHandlerGateErrorFailsClosed(t *testing.T) {
	gate := &mockGate{err: errors.New("store down")}
	engine := &mockEngine{}
	reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil)(context.Background(), inbound)
	require.NoError(t, err, "a gate error is handled, not surfaced as a crash")
	assert.True(t, reply.Refused)
	assert.False(t, engine.called, "engine not reached on a gate failure")
}

// Answer's ErrOverBudget becomes a graceful capacity reply, not a raw error.
func TestHandlerOverBudgetGraceful(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: budget.ErrOverBudget}
	reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil)(context.Background(), inbound)
	require.NoError(t, err, "over-budget is degraded gracefully")
	assert.Contains(t, reply.Text, "capacity")
	assert.True(t, reply.Refused)
}

// Any other Answer error propagates.
func TestHandlerAnswerErrorPropagates(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: errors.New("boom")}
	_, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil)(context.Background(), inbound)
	assert.Error(t, err)
}

// A served (consent-granted) message is logged to the quarantine store with the
// user, surface, and text.
func TestHandlerLogsServedMessage(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	h := runtime.NewHandler(gate, engine, nil, nil, nil, qlog)

	_, err := h(context.Background(), inbound)
	require.NoError(t, err)

	entries := qlog.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "u1", entries[0].UserID)
	assert.Equal(t, "group", entries[0].Surface)
	assert.Equal(t, "hello", entries[0].Text)
}

// The quarantine log is group-only (ADR 0004): a served group message is logged,
// a served DM is not (a private one-to-one is not community content).
func TestHandlerLogsGroupOnly(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, qlog)

	_, err := h(context.Background(), transport.Inbound{User: core.User{ID: "g1"}, Surface: core.SurfaceGroup, Text: "community msg"})
	require.NoError(t, err)
	require.Len(t, qlog.Entries(), 1, "a served group message is logged")

	_, err = h(context.Background(), transport.Inbound{User: core.User{ID: "d1"}, Surface: core.SurfaceDM, Text: "private msg"})
	require.NoError(t, err)
	assert.Len(t, qlog.Entries(), 1, "a served DM is not logged")
}

// Every non-serve decision logs nothing — consent is not confirmed, so ADR 0002's
// "record nothing without consent" holds.
func TestHandlerDoesNotLogWhenNotServed(t *testing.T) {
	for _, decision := range []acl.Decision{
		acl.DecideAskConsent, acl.DecideSilent, acl.DecideRateLimited, acl.DecideRefuse,
	} {
		qlog := &quarantine.MemoryLog{}
		h := runtime.NewHandler(&mockGate{decision: decision}, &mockEngine{}, nil, nil, nil, qlog)
		_, err := h(context.Background(), inbound)
		require.NoError(t, err)
		assert.Empty(t, qlog.Entries(), "decision %d logs nothing", decision)
	}
}

// Named: a user who has explicitly DECLINED consent logs nothing — end to end
// through the real gate. This is the clearest "record nothing without consent"
// case: the user actively refused.
func TestHandlerDeclinedConsentLogsNothing(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	require.NoError(t, aclStore.Put(ctx, acl.Record{UserID: "u1", Consent: acl.ConsentDeclined}))
	gate := acl.NewGate(aclStore, acl.TierDefault)

	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(gate, &mockEngine{}, nil, nil, nil, qlog)

	_, err = h(ctx, transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "secret"})
	require.NoError(t, err)
	assert.Empty(t, qlog.Entries(), "a declined user's message is never recorded")
}

// A nil log disables logging without a panic on the serve path.
func TestHandlerNilLogNoOp(t *testing.T) {
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, nil)
	_, err := h(context.Background(), inbound)
	require.NoError(t, err)
}

// A button click is not a community message and is never logged.
func TestHandlerCallbackDoesNotLog(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "echo"})
	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store,
		registryWith("echo", runtime.EchoVerb()), &mockAuthz{authorized: true}, qlog)

	_, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Empty(t, qlog.Entries(), "a callback is not logged")
}

// callbackInbound is a button-click inbound (user u1) carrying token.
func callbackInbound(token string) transport.Inbound {
	return transport.Inbound{
		User:    core.User{ID: "u1"},
		Surface: core.SurfaceDM,
		Callback: &transport.Callback{
			Token: token, QueryID: "cq1", MessageID: "5",
		},
	}
}

// mockAuthz records the re-authorization call and returns a canned verdict.
type mockAuthz struct {
	authorized bool
	err        error
	called     bool
	gotMinTier acl.Tier
}

func (m *mockAuthz) Authorize(_ context.Context, _ core.User, minTier acl.Tier) (bool, error) {
	m.called = true
	m.gotMinTier = minTier
	return m.authorized, m.err
}

// spyVerb counts validate/execute calls so a test can prove which stages ran.
type spyVerb struct {
	execCalls     int
	validateCalls int
	gotSubject    core.User
	gotParams     map[string]string
	execErr       error
	validateErr   error
	status        string
}

func (s *spyVerb) verb(minTier acl.Tier) runtime.Verb {
	return runtime.Verb{
		MinTier:  minTier,
		Validate: func(map[string]string) error { s.validateCalls++; return s.validateErr },
		Execute: func(_ context.Context, subj core.User, p map[string]string) (string, error) {
			s.execCalls++
			s.gotSubject, s.gotParams = subj, p
			return s.status, s.execErr
		},
	}
}

func registryWith(name string, v runtime.Verb) *runtime.Registry {
	r := runtime.NewRegistry()
	r.Register(name, v)
	return r
}

func putToken(t *testing.T, store pending.Store, a core.Action) string {
	t.Helper()
	tok, err := store.Put(context.Background(), a)
	require.NoError(t, err)
	return tok
}

// Happy path: an authorized subject with valid params runs the executor once and
// gets a success toast plus a status line; the gate and engine are untouched.
func TestHandlerCallbackHappyPath(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act", Params: map[string]string{"k": "v"}})
	spy := &spyVerb{status: "did the thing"}
	authz := &mockAuthz{authorized: true}
	gate, engine := &mockGate{}, &mockEngine{}

	h := runtime.NewHandler(gate, engine, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil)
	reply, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)

	assert.Equal(t, 1, spy.execCalls, "executor runs exactly once")
	assert.Equal(t, acl.TierAdmin, authz.gotMinTier, "re-authorized against the verb's tier")
	assert.Equal(t, "u1", spy.gotSubject.ID, "executor gets the acting subject")
	assert.Contains(t, reply.Notice, "Done")
	assert.Equal(t, "did the thing", reply.Text, "the status line rides in Text for the edit")
	assert.False(t, gate.called, "a click never consults the consent gate")
	assert.False(t, engine.called, "a click never reaches the engine")
}

// The denial matrix: unknown verb, under-tier, authorize error, and invalid
// params each fail closed with a toast and never reach the executor.
func TestHandlerCallbackDenials(t *testing.T) {
	cases := []struct {
		name        string
		authorized  bool
		authzErr    error
		validateErr error
		verbName    string // registered name; action always uses "act"
		wantNotice  string
		wantAuthz   bool // was Authorize consulted?
	}{
		{"unknown-verb", true, nil, nil, "other", "That action is no longer available.", false},
		{"under-tier", false, nil, nil, "act", "You don't have permission to do that.", true},
		{"authorize-error", false, errors.New("store down"), nil, "act", "You don't have permission to do that.", true},
		{"invalid-params", true, nil, errors.New("bad"), "act", "That action can't be completed as requested.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := pending.NewMemoryStore(testTTL)
			token := putToken(t, store, core.Action{Verb: "act"})
			spy := &spyVerb{validateErr: tc.validateErr}
			authz := &mockAuthz{authorized: tc.authorized, err: tc.authzErr}

			h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store,
				registryWith(tc.verbName, spy.verb(acl.TierAdmin)), authz, nil)
			reply, err := h(context.Background(), callbackInbound(token))
			require.NoError(t, err)

			assert.Equal(t, tc.wantNotice, reply.Notice)
			assert.Equal(t, 0, spy.execCalls, "the executor is never reached on a denial")
			assert.Equal(t, tc.wantAuthz, authz.called, "authorize is consulted only after a known verb")
		})
	}
}

// Authorize precedes validate: an unauthorized clicker never reaches param
// validation, so it can't probe valid param shapes from a validation error.
func TestHandlerCallbackAuthorizeBeforeValidate(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	spy := &spyVerb{}
	authz := &mockAuthz{authorized: false}

	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil)
	_, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)

	assert.True(t, authz.called)
	assert.Equal(t, 0, spy.validateCalls, "validation never runs for an unauthorized click")
}

// Executor error: a failure toast, the token stays consumed (no auto-retry).
func TestHandlerCallbackExecutorError(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	spy := &spyVerb{execErr: errors.New("boom")}
	authz := &mockAuthz{authorized: true}

	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil)
	reply, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "That didn't go through — please try again.", reply.Notice)
	assert.Equal(t, 1, spy.execCalls)

	// The token was consumed on this resolve; a second click reports so.
	reply, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "Already completed.", reply.Notice)
}

// Non-resolved statuses (consumed, expired, no store) toast and never execute.
func TestHandlerCallbackNonResolved(t *testing.T) {
	spy := &spyVerb{}
	authz := &mockAuthz{authorized: true}
	reg := registryWith("act", spy.verb(acl.TierAdmin))

	// No store -> expired.
	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, nil, reg, authz, nil)
	reply, err := h(context.Background(), callbackInbound("tok"))
	require.NoError(t, err)
	assert.Equal(t, "This action has expired.", reply.Notice)

	// Consumed after one resolve.
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	h = runtime.NewHandler(&mockGate{}, &mockEngine{}, store, reg, authz, nil)
	_, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	reply, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "Already completed.", reply.Notice)

	assert.Equal(t, 1, spy.execCalls, "only the single fresh resolve executed")
}

// The load-bearing rule end to end over a real acl gate: a tier demotion between
// render and click denies, and the privileged effect never commits.
func TestHandlerCallbackDemotionDenies(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierAdmin)) // clicker is admin at render

	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "set_tier", Params: map[string]string{"user": "target", "tier": "trusted"}})
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate), gate, nil)

	// Demote the clicker after the button was shown, before the click.
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierDefault))

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "You don't have permission to do that.", reply.Notice)

	rec, err := aclStore.Get(ctx, "target")
	require.NoError(t, err)
	assert.NotEqual(t, acl.TierTrusted, rec.Tier, "the privileged effect never committed")
}

// The happy path over the real gate + DefaultRegistry: an admin sets a target's
// tier, the effect commits, and the status line reflects it.
func TestHandlerCallbackSetTierHappyPath(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierAdmin))

	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "set_tier", Params: map[string]string{"user": "target", "tier": "trusted"}})
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate), gate, nil)

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, reply.Notice, "Done")
	assert.Contains(t, reply.Text, "trusted")

	rec, err := aclStore.Get(ctx, "target")
	require.NoError(t, err)
	assert.Equal(t, acl.TierTrusted, rec.Tier, "the tier change committed")
}
