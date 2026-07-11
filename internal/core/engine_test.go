package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Recent-conversation history is injected as a labelled, timestamped, threaded
// block positioned after the grounding contract and before the retrieved CONTEXT
// (ADR 0014). A reply-to whose target is present names that speaker.
func TestHistoryInjectedAsContext(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "vault fact"}}}
	mdl := textModel("ok")
	e := core.New(mdl, ret, nopTools{}, "SCOPE")
	tm := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	q := core.Query{Text: "tldr", History: []core.HistoryTurn{
		{Speaker: "Al", Text: "how do I calibrate?", Time: tm, MessageID: "m1"},
		{Bot: true, Text: "Turn the blue dial.", Time: tm.Add(time.Minute), MessageID: "m2", ReplyTo: "m1"},
	}}
	_, err := e.Answer(context.Background(), q)
	require.NoError(t, err)

	sys := systemOf(mdl)
	assert.Contains(t, sys, "RECENT CONVERSATION")
	assert.Contains(t, sys, "Al: how do I calibrate?")
	assert.Contains(t, sys, "assistant (reply to Al): Turn the blue dial.")
	assert.Contains(t, sys, "09:30", "turns are timestamped")
	assert.Less(t, strings.Index(sys, core.RefusalToken), strings.Index(sys, "RECENT CONVERSATION"),
		"history follows the grounding contract")
	assert.Less(t, strings.Index(sys, "RECENT CONVERSATION"), strings.Index(sys, "CONTEXT:"),
		"history precedes the retrieved context")
}

// A reply-to whose target is not in the injected set renders "a message not
// shown" — the reply-to counterpart of the partial-record disclosure.
func TestHistoryReplyToNotShown(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}
	mdl := textModel("ok")
	e := core.New(mdl, ret, nopTools{}, "SCOPE")
	q := core.Query{Text: "more", History: []core.HistoryTurn{
		{Speaker: "Bo", Text: "replying to a gated-out msg", Time: time.Now(), MessageID: "m9", ReplyTo: "gone"},
	}}
	_, err := e.Answer(context.Background(), q)
	require.NoError(t, err)
	assert.Contains(t, systemOf(mdl), "reply to a message not shown")
}

// No history leaves the prompt unchanged — no conversation block (backwards-
// compatible with a disabled buffer / standalone question).
func TestNoHistoryNoBlock(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}
	mdl := textModel("ok")
	e := core.New(mdl, ret, nopTools{}, "SCOPE")
	_, err := e.Answer(context.Background(), core.Query{Text: "hi"})
	require.NoError(t, err)
	assert.NotContains(t, systemOf(mdl), "RECENT CONVERSATION")
}

type mockRetriever struct {
	chunks []core.Chunk
	err    error
}

func (m mockRetriever) Retrieve(context.Context, string) ([]core.Chunk, error) {
	return m.chunks, m.err
}

// mockModel returns scripted completions in order (the last repeats), so a test
// can drive the tool-call loop. It records the messages/tools of the last call.
type mockModel struct {
	replies      []core.Completion
	err          error
	calls        int
	lastMessages []core.Message
	lastTools    []core.ToolDef
}

func (m *mockModel) Complete(_ context.Context, messages []core.Message, tools []core.ToolDef) (core.Completion, error) {
	m.calls++
	m.lastMessages, m.lastTools = messages, tools
	if m.err != nil {
		return core.Completion{}, m.err
	}
	if len(m.replies) == 0 {
		return core.Completion{}, nil
	}
	idx := m.calls - 1
	if idx >= len(m.replies) {
		idx = len(m.replies) - 1
	}
	return m.replies[idx], nil
}

func textModel(reply string) *mockModel {
	return &mockModel{replies: []core.Completion{{Text: reply}}}
}

func systemOf(m *mockModel) string {
	if len(m.lastMessages) == 0 {
		return ""
	}
	return m.lastMessages[0].Content
}

// nopTools advertises no tools.
type nopTools struct{}

func (nopTools) Defs() []core.ToolDef                                         { return nil }
func (nopTools) Call(context.Context, string, map[string]any) (string, error) { return "", nil }

// mockTools advertises tools and returns scripted results/errors per name.
type mockTools struct {
	defs    []core.ToolDef
	results map[string]string
	errs    map[string]error
	calls   []string
}

func (m *mockTools) Defs() []core.ToolDef { return m.defs }
func (m *mockTools) Call(_ context.Context, name string, _ map[string]any) (string, error) {
	m.calls = append(m.calls, name)
	if m.errs != nil {
		if err := m.errs[name]; err != nil {
			return "", err
		}
	}
	return m.results[name], nil
}

func toolCall(id, name string) core.ToolCall {
	return core.ToolCall{ID: id, Name: name, Arguments: map[string]any{}}
}

// A grounded query is answered by the model; the system prompt carries the scope,
// the refusal contract, and each chunk's source in retrieval order.
func TestAnswerGrounded(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{
		{Source: "notes/a.md#Intro", Text: "The widget installs via the script."},
		{Source: "faq.md", Text: "Reset anytime."},
	}}
	mdl := textModel("Install with the script [notes/a.md#Intro].")
	e := core.New(mdl, ret, nopTools{}, "You answer about the widget.")

	reply, err := e.Answer(context.Background(), core.Query{Text: "how do I install?"})
	require.NoError(t, err)
	assert.False(t, reply.Refused)
	assert.Equal(t, "Install with the script [notes/a.md#Intro].", reply.Text)

	sys := systemOf(mdl)
	assert.Contains(t, sys, "You answer about the widget.")
	assert.Contains(t, sys, core.RefusalToken)
	assert.Contains(t, sys, "notes/a.md#Intro")
	assert.Contains(t, sys, "faq.md")
	assert.Less(t, strings.Index(sys, "notes/a.md#Intro"), strings.Index(sys, "faq.md"),
		"chunk sources appear in retrieval order (deterministic)")
	require.Len(t, mdl.lastMessages, 2)
	assert.Equal(t, "how do I install?", mdl.lastMessages[1].Content, "user message is the raw query")
}

// The persona layer is prepended to the system prompt before scope, and the
// grounding contract that follows reasserts it cannot relax scope or grounding
// (ADR 0013).
func TestPersonaInjectedBeforeScope(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}
	mdl := textModel("ok")
	persona := "You are Grove, warm and concise."
	e := core.New(mdl, ret, nopTools{}, "You answer about the widget.", core.WithPersona(persona))

	_, err := e.Answer(context.Background(), core.Query{Text: "hi"})
	require.NoError(t, err)

	sys := systemOf(mdl)
	require.Contains(t, sys, persona)
	assert.Less(t, strings.Index(sys, persona), strings.Index(sys, "You answer about the widget."),
		"persona precedes scope")
	assert.Contains(t, sys, "persona above sets your voice",
		"the grounding contract reasserts persona can't relax scope/grounding")
}

// Without a persona the prompt is unchanged: it begins at scope, with no persona
// section or override note (backwards-compatible — the WithPersona zero value and
// omitting the option both mean no layer).
func TestNoPersonaByDefault(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}
	mdl := textModel("ok")
	e := core.New(mdl, ret, nopTools{}, "SCOPE-LINE", core.WithPersona(""))

	_, err := e.Answer(context.Background(), core.Query{Text: "hi"})
	require.NoError(t, err)

	sys := systemOf(mdl)
	assert.True(t, strings.HasPrefix(sys, "SCOPE-LINE"), "system prompt begins at scope when no persona")
	assert.NotContains(t, sys, "persona above sets your voice", "no override note without a persona")
}

// Empty retrieval with no tools refuses without a model call.
func TestAnswerRefusesOnEmptyRetrievalWithoutModelCall(t *testing.T) {
	mdl := textModel("must not be produced")
	e := core.New(mdl, mockRetriever{chunks: nil}, nopTools{}, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "unknowable"})
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.Equal(t, 0, mdl.calls, "no model call when there is nothing to ground on and no tools")
	assert.NotEmpty(t, reply.Text)
}

// A sentinel-led decline is a refusal, and the persona-voiced note after the
// marker is what the user sees — the marker itself is stripped (ADR 0013).
func TestAnswerRefusesOnModelSentinel(t *testing.T) {
	mdl := textModel(core.RefusalToken + " I focus on widgets — happy to help with those.")
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "unrelated"}}}, nopTools{}, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "off topic"})
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.NotContains(t, reply.Text, core.RefusalToken, "the marker is stripped")
	assert.Contains(t, reply.Text, "I focus on widgets", "the persona-voiced decline is surfaced")
}

// Refusal detection is prefix-only: the token first (leading whitespace
// tolerated) is a refusal; a bare token falls back to the fixed line;
// a BURIED token is an instruction violation, treated as a normal answer with the
// stray marker stripped so no raw token reaches the user.
func TestRefusalParsing(t *testing.T) {
	chunk := mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}

	// Bare token → refusal with the fallback line, no raw marker.
	mdl := textModel(core.RefusalToken)
	reply, err := core.New(mdl, chunk, nopTools{}, "scope").Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.NotEmpty(t, reply.Text)
	assert.NotContains(t, reply.Text, core.RefusalToken)

	// Leading whitespace before the token still refuses.
	mdl2 := textModel("  \n" + core.RefusalToken + " here's what I can do")
	reply2, err := core.New(mdl2, chunk, nopTools{}, "scope").Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.True(t, reply2.Refused)
	assert.Contains(t, reply2.Text, "here's what I can do")

	// A buried token is NOT a refusal; the stray marker is stripped from the reply.
	mdl3 := textModel("Here is an answer " + core.RefusalToken + " tail")
	reply3, err := core.New(mdl3, chunk, nopTools{}, "scope").Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.False(t, reply3.Refused, "a buried sentinel is an instruction violation, not a refusal")
	assert.NotContains(t, reply3.Text, core.RefusalToken, "the stray marker is stripped from the user text")
}

// The early refuse-without-a-model-call must not fire when there is conversation
// history: a meta follow-up ("tldr") has no vault chunks but must still reach the
// model to summarize the recent conversation (ADR 0014). Reproduces + fixes the
// live 0.3.0 "tldr refuses" bug.
func TestAnswerWithHistoryReachesModel(t *testing.T) {
	mdl := textModel("summary of the prior turn")
	e := core.New(mdl, mockRetriever{chunks: nil}, nopTools{}, "SCOPE") // empty retrieval, no tools
	q := core.Query{Text: "tldr", History: []core.HistoryTurn{
		{Bot: true, Text: "The widget calibrates via the blue dial.", Time: time.Now()},
	}}
	reply, err := e.Answer(context.Background(), q)
	require.NoError(t, err)
	assert.Positive(t, mdl.calls, "the model is called when there is history to meta-operate on")
	assert.False(t, reply.Refused)
	assert.Contains(t, systemOf(mdl), "RECENT CONVERSATION", "the prior turn reaches the prompt")
	assert.Contains(t, systemOf(mdl), "MAY summarize", "the header permits meta-operations")

	// Still short-circuits (no model call) when there is neither grounding nor history.
	mdl2 := textModel("must not be produced")
	e2 := core.New(mdl2, mockRetriever{chunks: nil}, nopTools{}, "SCOPE")
	reply2, err := e2.Answer(context.Background(), core.Query{Text: "unknowable"})
	require.NoError(t, err)
	assert.True(t, reply2.Refused)
	assert.Zero(t, mdl2.calls, "empty retrieval + no tools + no history still refuses without a model call")
}

// Retriever and model errors propagate.
func TestAnswerPropagatesErrors(t *testing.T) {
	e1 := core.New(textModel(""), mockRetriever{err: errors.New("scan failed")}, nopTools{}, "scope")
	_, err := e1.Answer(context.Background(), core.Query{Text: "q"})
	assert.Error(t, err, "retriever error propagates")

	e2 := core.New(&mockModel{err: errors.New("model boom")},
		mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, nopTools{}, "scope")
	_, err = e2.Answer(context.Background(), core.Query{Text: "q"})
	assert.Error(t, err, "model error propagates")
}

func toolRegistry() *mockTools {
	return &mockTools{
		defs:    []core.ToolDef{{Name: "search", Description: "search transcripts", Schema: json.RawMessage(`{"type":"object"}`)}},
		results: map[string]string{"search": "found: the answer"},
	}
}

// The tool-call loop: the model requests a tool, the engine runs it and feeds the
// result back as scoped context, and the model then answers. The tool defs reach
// the model and the tool result is appended to the next round.
func TestAnswerToolLoop(t *testing.T) {
	tools := toolRegistry()
	mdl := &mockModel{replies: []core.Completion{
		{ToolCalls: []core.ToolCall{toolCall("c1", "search")}}, // round 1: call the tool
		{Text: "Here's the answer [search]."},                  // round 2: final
	}}
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, tools, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "in-domain q the vault lacks"})
	require.NoError(t, err)
	assert.False(t, reply.Refused)
	assert.Equal(t, "Here's the answer [search].", reply.Text)
	assert.Equal(t, []string{"search"}, tools.calls, "the tool was called once")
	assert.Equal(t, 2, mdl.calls, "model completed twice (request, then final)")

	// The tool defs were advertised, and the tool result was fed back as a tool turn.
	require.Len(t, mdl.lastTools, 1)
	assert.Equal(t, "search", mdl.lastTools[0].Name)
	var sawToolResult bool
	for _, m := range mdl.lastMessages {
		if m.Role == core.RoleTool && strings.Contains(m.Content, "found: the answer") {
			sawToolResult = true
		}
	}
	assert.True(t, sawToolResult, "the tool result is fed back as a tool-role message")
}

// A tool that ran and reported an error feeds its failure back as content so the
// model can adapt; the loop continues rather than aborting.
func TestAnswerToolErrorFedBack(t *testing.T) {
	tools := toolRegistry()
	tools.errs = map[string]error{"search": errors.New("no results")}
	mdl := &mockModel{replies: []core.Completion{
		{ToolCalls: []core.ToolCall{toolCall("c1", "search")}},
		{Text: "Sorry, nothing found, but here's what I know."},
	}}
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, tools, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.False(t, reply.Refused)
	assert.Equal(t, 2, mdl.calls, "the loop continued after the tool error")

	var sawErr bool
	for _, m := range mdl.lastMessages {
		if m.Role == core.RoleTool && strings.Contains(m.Content, "tool error") {
			sawErr = true
		}
	}
	assert.True(t, sawErr, "the tool failure is fed back as content")
}

// A transport-level tool failure (ErrToolUnavailable) aborts the loop.
func TestAnswerToolUnavailableAborts(t *testing.T) {
	tools := toolRegistry()
	// A transport failure wraps ErrToolUnavailable (as the real Registry does).
	tools.errs = map[string]error{"search": errWrap(core.ErrToolUnavailable)}
	mdl := &mockModel{replies: []core.Completion{
		{ToolCalls: []core.ToolCall{toolCall("c1", "search")}},
		{Text: "must not be reached"},
	}}
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, tools, "scope")

	_, err := e.Answer(context.Background(), core.Query{Text: "q"})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrToolUnavailable)
	assert.Equal(t, 1, mdl.calls, "the loop aborted; the model was not called again")
}

func errWrap(err error) error { return &wrapped{err} }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "tool call: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// A model that never stops requesting tools hits the iteration cap and refuses
// gracefully rather than looping forever.
func TestAnswerToolLoopCap(t *testing.T) {
	tools := toolRegistry()
	mdl := &mockModel{replies: []core.Completion{
		{ToolCalls: []core.ToolCall{toolCall("c1", "search")}}, // repeats (last-reply-repeats)
	}}
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, tools, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.True(t, reply.Refused, "capped loop refuses rather than hanging")
	assert.Equal(t, 5, mdl.calls, "the loop is capped at maxToolIterations")
}

// With no vault chunks but tools available, the model IS consulted (a tool may
// ground an in-domain answer) — no empty-retrieval short-circuit.
func TestAnswerEmptyChunksWithToolsCallsModel(t *testing.T) {
	tools := toolRegistry()
	mdl := textModel("grounded via tool [search].")
	e := core.New(mdl, mockRetriever{chunks: nil}, tools, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "q"})
	require.NoError(t, err)
	assert.False(t, reply.Refused)
	assert.Equal(t, 1, mdl.calls, "the model is consulted because a tool could ground it")
	assert.Contains(t, systemOf(mdl), "tools available to you", "the prompt mentions tool grounding")
}
