package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

type mockRetriever struct {
	chunks []core.Chunk
	err    error
}

func (m mockRetriever) Retrieve(context.Context, string) ([]core.Chunk, error) {
	return m.chunks, m.err
}

type mockModel struct {
	reply     string
	err       error
	called    bool
	gotSystem string
	gotUser   string
}

func (m *mockModel) Complete(_ context.Context, system, user string) (core.Completion, error) {
	m.called = true
	m.gotSystem, m.gotUser = system, user
	return core.Completion{Text: m.reply}, m.err
}

// nopTools satisfies core.Tools; Answer does not call tools in Unit 5.
type nopTools struct{}

func (nopTools) List() []string                                               { return nil }
func (nopTools) Call(context.Context, string, map[string]any) (string, error) { return "", nil }

// A grounded query is answered by the model, and the system prompt carries the
// scope, the refusal contract, and each chunk's source in retrieval order.
func TestAnswerGrounded(t *testing.T) {
	ret := mockRetriever{chunks: []core.Chunk{
		{Source: "notes/a.md#Intro", Text: "The widget installs via the script."},
		{Source: "faq.md", Text: "Reset anytime."},
	}}
	mdl := &mockModel{reply: "Install with the script [notes/a.md#Intro]."}
	e := core.New(mdl, ret, nopTools{}, "You answer about the widget.")

	reply, err := e.Answer(context.Background(), core.Query{Text: "how do I install?"})
	require.NoError(t, err)
	assert.False(t, reply.Refused)
	assert.Equal(t, "Install with the script [notes/a.md#Intro].", reply.Text)

	assert.Contains(t, mdl.gotSystem, "You answer about the widget.")
	assert.Contains(t, mdl.gotSystem, core.RefusalToken)
	assert.Contains(t, mdl.gotSystem, "notes/a.md#Intro")
	assert.Contains(t, mdl.gotSystem, "faq.md")
	assert.Less(t, strings.Index(mdl.gotSystem, "notes/a.md#Intro"), strings.Index(mdl.gotSystem, "faq.md"),
		"chunk sources appear in retrieval order (deterministic)")
	assert.Equal(t, "how do I install?", mdl.gotUser, "user message is the raw query")
}

// Empty retrieval refuses without a model call.
func TestAnswerRefusesOnEmptyRetrievalWithoutModelCall(t *testing.T) {
	mdl := &mockModel{reply: "must not be produced"}
	e := core.New(mdl, mockRetriever{chunks: nil}, nopTools{}, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "unknowable"})
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.False(t, mdl.called, "no model call when there is nothing to ground on")
	assert.NotEmpty(t, reply.Text)
}

// The model's out-of-scope sentinel becomes a refusal, with the sentinel replaced
// by a clean user-facing line.
func TestAnswerRefusesOnModelSentinel(t *testing.T) {
	mdl := &mockModel{reply: "preamble " + core.RefusalToken}
	e := core.New(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "unrelated"}}}, nopTools{}, "scope")

	reply, err := e.Answer(context.Background(), core.Query{Text: "off topic"})
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.NotContains(t, reply.Text, core.RefusalToken)
}

// Retriever and model errors propagate.
func TestAnswerPropagatesErrors(t *testing.T) {
	e1 := core.New(&mockModel{}, mockRetriever{err: errors.New("scan failed")}, nopTools{}, "scope")
	_, err := e1.Answer(context.Background(), core.Query{Text: "q"})
	assert.Error(t, err, "retriever error propagates")

	e2 := core.New(&mockModel{err: errors.New("model boom")},
		mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "x"}}}, nopTools{}, "scope")
	_, err = e2.Answer(context.Background(), core.Query{Text: "q"})
	assert.Error(t, err, "model error propagates")
}
