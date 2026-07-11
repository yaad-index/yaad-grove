package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

func groupMsg(id, text, msgID string, replyToBot bool) transport.Inbound {
	return transport.Inbound{
		User: core.User{ID: id, Display: "Al"}, Surface: core.SurfaceGroup,
		Text: text, ReplyTo: "chat", Directed: true, MessageID: msgID, ReplyToBot: replyToBot,
	}
}

// The agreed ordering gate (ADR 0014): Select runs before Append, so the current
// message never appears in its own injected context — and prior turns (including
// the bot's answer) do.
func TestHandlerSelectBeforeAppend(t *testing.T) {
	buf := memory.New(10)
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	policy := runtime.Policy{Memory: buf, Inject: 10}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, policy)

	// First message: buffer empty, so no history.
	_, err := h(context.Background(), groupMsg("u1", "how do I calibrate the widget?", "m1", false))
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.History, "the first message has no prior history")

	// Follow-up: injects prior context but never the current message.
	_, err = h(context.Background(), groupMsg("u1", "tldr", "m2", true))
	require.NoError(t, err)
	require.NotEmpty(t, engine.gotQuery.History, "the follow-up pulls prior context")

	var texts []string
	for _, ht := range engine.gotQuery.History {
		assert.NotEqual(t, "tldr", ht.Text, "the current message never appears in its own injected context")
		texts = append(texts, ht.Text)
	}
	assert.Contains(t, texts, "how do I calibrate the widget?", "the prior question is injected")
	assert.Contains(t, texts, "answer", "the bot's prior answer is buffered and injected")
}

// A standalone question pulls no history even with a full buffer (the follow-up
// gate), and a served turn is still recorded for later follow-ups.
func TestHandlerStandaloneNoHistory(t *testing.T) {
	buf := memory.New(10)
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	policy := runtime.Policy{Memory: buf, Inject: 10}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, policy)

	_, err := h(context.Background(), groupMsg("u1", "how do I install the widget?", "m1", false))
	require.NoError(t, err)
	_, err = h(context.Background(), groupMsg("u1", "how do I uninstall the widget?", "m2", false))
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.History, "a standalone question injects no history")
	// But the buffer did record the turns (a later follow-up would see them).
	assert.NotEmpty(t, buf.Recent("chat", 10))
}

// /consent remove purges the withdrawing user's turns from the buffer everywhere.
func TestHandlerPurgeOnConsentRemove(t *testing.T) {
	buf := memory.New(10)
	buf.Append("chatX", memory.Turn{SpeakerID: "u1", Text: "hi", Time: time.Now()})
	buf.Append("chatY", memory.Turn{SpeakerID: "u1", Text: "yo", Time: time.Now()})
	buf.Append("chatX", memory.Turn{SpeakerID: "u2", Text: "keep", Time: time.Now()})
	consent := &mockConsenter{consent: acl.ConsentGranted}
	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, nil, nil, nil, nil, consent, runtime.Policy{Memory: buf})

	_, err := h(context.Background(), transport.Inbound{
		User: core.User{ID: "u1"}, Surface: core.SurfaceDM, Text: "/consent remove",
	})
	require.NoError(t, err)

	// u1's turns are gone from every conversation; u2's survive.
	assert.Empty(t, buf.Recent("chatY", 10), "u1's only chatY turn is purged — withdrawal is global")
	remX := buf.Recent("chatX", 10)
	require.Len(t, remX, 1, "chatX drops u1's turn, keeps u2's")
	assert.Equal(t, "u2", remX[0].SpeakerID, "another user's turns are untouched")
}

// With memory disabled (nil buffer via the zero Policy) the handler answers in
// isolation without panicking and injects nothing.
func TestHandlerMemoryDisabledNoOp(t *testing.T) {
	engine := &mockEngine{reply: core.Reply{Text: "a"}}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, runtime.Policy{})
	_, err := h(context.Background(), groupMsg("u1", "hi", "m1", false))
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.History, "no buffer → no injected history")
}
