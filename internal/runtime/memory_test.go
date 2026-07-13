package runtime_test

import (
	"context"
	"strings"
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

// capModel records the system prompt of each call — to inspect the assembled
// prompt end to end through the handler + real engine.
type capModel struct {
	lastSystem string
	calls      int
}

func (m *capModel) Complete(_ context.Context, msgs []core.Message, _ []core.ToolDef) (core.Completion, error) {
	m.calls++
	if len(msgs) > 0 {
		m.lastSystem = msgs[0].Content
	}
	return core.Completion{Text: "The widget calibrates via the blue dial."}, nil
}

// condRetriever grounds a normal question but returns nothing for a meta query,
// mirroring a retrieval-only bot: "tldr" has no vault chunks.
type condRetriever struct{}

func (condRetriever) Retrieve(_ context.Context, q string) ([]core.Chunk, error) {
	if strings.Contains(strings.ToLower(q), "tldr") {
		return nil, nil
	}
	return []core.Chunk{{Source: "a.md", Text: "widget info"}}, nil
}

type noTools struct{}

func (noTools) Defs() []core.ToolDef                                         { return nil }
func (noTools) Call(context.Context, string, map[string]any) (string, error) { return "", nil }

// End-to-end (the live 0.3.0 "tldr" reproduce, DM path): an admin asks a question,
// then "tldr". The follow-up reaches the model with the bot's prior answer in the
// RECENT CONVERSATION block — empty retrieval for the meta query no longer
// short-circuits it, and the block permits summarizing.
func TestHandlerDMTldrReachesModelWithHistory(t *testing.T) {
	buf := memory.New(20)
	model := &capModel{}
	engine := core.New(model, condRetriever{}, noTools{}, "You answer about the widget.")
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"}), Memory: buf, Inject: 15, FollowupWindow: time.Hour}
	consent := &mockConsenter{consent: acl.ConsentGranted}
	h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, consent, policy)

	dm := func(text, msgID string) transport.Inbound {
		return transport.Inbound{User: core.User{ID: "admin1", Display: "Ada"}, Surface: core.SurfaceDM, Text: text, ReplyTo: "dm-chat", Directed: true, MessageID: msgID}
	}

	// Q1 is grounded and answered → the bot's answer is buffered.
	_, err := h(context.Background(), dm("how do I calibrate the widget?", "m1"))
	require.NoError(t, err)
	callsAfterQ1 := model.calls

	// "tldr": empty retrieval, but history present → the model IS called, with the
	// bot's prior answer in the RECENT CONVERSATION block.
	_, err = h(context.Background(), dm("tldr", "m2"))
	require.NoError(t, err)
	assert.Greater(t, model.calls, callsAfterQ1, "the meta follow-up reaches the model, not an early refuse")
	assert.Contains(t, model.lastSystem, "RECENT CONVERSATION")
	assert.Contains(t, model.lastSystem, "The widget calibrates via the blue dial.",
		"the bot's prior answer is in the assembled prompt")
	assert.Contains(t, model.lastSystem, "MAY summarize", "the block permits meta-operations")
}

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
	policy := runtime.Policy{Memory: buf, Inject: 10, FollowupWindow: time.Hour}
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

// A sender who is NOT mid-conversation gets no history — the recency gate (ADR
// 0018): a first message, or a message from a sender who hasn't spoken, is
// standalone even with a populated buffer. Served turns are still recorded for
// later follow-ups.
func TestHandlerStandaloneNoHistory(t *testing.T) {
	buf := memory.New(10)
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	policy := runtime.Policy{Memory: buf, Inject: 10, FollowupWindow: time.Hour}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, policy)

	// u1's first message: no prior turn of theirs → not mid-conversation → standalone.
	_, err := h(context.Background(), groupMsg("u1", "how do I install the widget?", "m1", false))
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.History, "a sender's first message injects no history")

	// A different, unseen sender is standalone too, even though u1 has now spoken.
	_, err = h(context.Background(), groupMsg("u2", "how do I uninstall the widget?", "m2", false))
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.History, "a sender who hasn't spoken before is standalone")

	// But the turns were recorded — a reply, or either sender's next message, would
	// now see them.
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
