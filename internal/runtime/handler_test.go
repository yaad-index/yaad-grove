package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

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
			reply, err := runtime.NewHandler(gate, engine)(context.Background(), inbound)
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
	reply, err := runtime.NewHandler(gate, engine)(context.Background(), inbound)
	require.NoError(t, err, "a gate error is handled, not surfaced as a crash")
	assert.True(t, reply.Refused)
	assert.False(t, engine.called, "engine not reached on a gate failure")
}

// Answer's ErrOverBudget becomes a graceful capacity reply, not a raw error.
func TestHandlerOverBudgetGraceful(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: budget.ErrOverBudget}
	reply, err := runtime.NewHandler(gate, engine)(context.Background(), inbound)
	require.NoError(t, err, "over-budget is degraded gracefully")
	assert.Contains(t, reply.Text, "capacity")
	assert.True(t, reply.Refused)
}

// Any other Answer error propagates.
func TestHandlerAnswerErrorPropagates(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: errors.New("boom")}
	_, err := runtime.NewHandler(gate, engine)(context.Background(), inbound)
	assert.Error(t, err)
}
